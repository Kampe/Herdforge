package resources

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

type LaneCategory string

const (
	LaneNew            LaneCategory = "new"
	LaneLegacyStanding LaneCategory = "legacy_standing"
	LaneCurrent        LaneCategory = "current"
	LaneTask           LaneCategory = "task"
	LaneReviewPool     LaneCategory = "review_pool"
	LaneReviewSurface  LaneCategory = "review_surface"
	LaneHarvest        LaneCategory = "harvest"
)

type LaneState string

const (
	LaneIdle    LaneState = "idle"
	LaneActive  LaneState = "active"
	LaneHeld    LaneState = "held"
	LaneDone    LaneState = "done"
	LaneUnknown LaneState = "unknown"
)

// RegisteredWorktree is one Git-registered checkout plus the evidence needed
// to decide whether its derived data is immutable. An unresolved bit always
// becomes PreserveReason; zero values never mean safe.
type RegisteredWorktree struct {
	Path                  string       `json:"-"`
	ReportPath            string       `json:"path"`
	Branch                string       `json:"branch,omitempty"`
	Head                  string       `json:"head"`
	Category              LaneCategory `json:"category"`
	State                 LaneState    `json:"state"`
	LastUse               time.Time    `json:"last_use"`
	AllocatedBytes        uint64       `json:"allocated_bytes"`
	AllocationTruncated   bool         `json:"allocation_truncated,omitempty"`
	Dirty                 bool         `json:"dirty,omitempty"`
	Untracked             bool         `json:"untracked,omitempty"`
	ActiveCWD             bool         `json:"active_cwd,omitempty"`
	OpenFile              bool         `json:"open_file,omitempty"`
	ActiveLease           bool         `json:"active_lease,omitempty"`
	Held                  bool         `json:"held,omitempty"`
	Unmerged              bool         `json:"unmerged,omitempty"`
	FailedCandidate       bool         `json:"failed_candidate,omitempty"`
	ReviewHandoffAdmitted bool         `json:"review_handoff_admitted,omitempty"`
	PreserveReason        string       `json:"preserve_reason,omitempty"`
}

type GovernorPolicy struct {
	HostID               string
	RepositoryID         string
	RepositoryRoot       string
	BaseRef              string
	LockPath             string
	GeneratedDirectories []string
	// OrphanRoots are known repository-local lane roots whose children may no
	// longer be registered with Git. They are census-only: no orphan is ever a
	// deletion target without canonical claim, ownership, and handle proof.
	OrphanRoots            []string
	OrphanDerivedTargets   []string
	OrphanCacheTTL         time.Duration
	OrphanCacheBudgetBytes uint64
	PressureBytes          uint64
	RecoveryBytes          uint64
	TaskReserveBytes       uint64
	MaxDispatchConcurrency int
	ReapBatchLimit         int
	MaxScanEntries         int
	LockTimeout            time.Duration
	LockRetry              time.Duration
	AllowApply             bool
	ApplyBeforeDispatch    bool
}

func (p GovernorPolicy) validate() error {
	if strings.TrimSpace(p.HostID) == "" || strings.TrimSpace(p.RepositoryRoot) == "" || strings.TrimSpace(p.LockPath) == "" {
		return errors.New("resource governor requires host, repository root, and lock path")
	}
	if len(p.GeneratedDirectories) == 0 && len(p.OrphanDerivedTargets) == 0 {
		return errors.New("resource governor requires declared generated directories")
	}
	if p.PressureBytes == 0 || p.RecoveryBytes <= p.PressureBytes || p.TaskReserveBytes == 0 {
		return errors.New("resource governor watermarks and task reserve are invalid")
	}
	if p.PressureBytes > math.MaxUint64-p.TaskReserveBytes || p.RecoveryBytes > math.MaxUint64-p.TaskReserveBytes {
		return errors.New("resource governor watermark plus task reserve overflows")
	}
	if p.MaxDispatchConcurrency <= 0 || p.ReapBatchLimit <= 0 || p.LockTimeout <= 0 || p.LockRetry <= 0 || p.LockRetry > p.LockTimeout {
		return errors.New("resource governor bounds are invalid")
	}
	if p.ApplyBeforeDispatch && !p.AllowApply {
		return errors.New("resource governor dispatch apply is not admitted")
	}
	if len(p.OrphanDerivedTargets) != 0 && (p.OrphanCacheTTL <= 0 || p.OrphanCacheBudgetBytes == 0) {
		return errors.New("orphan cache targets require a positive TTL and nonzero budget")
	}
	for _, target := range p.OrphanDerivedTargets {
		if target != "graph.db" && target != "bootstrap-go-mod" && target != "bootstrap-go-build" {
			return fmt.Errorf("unsupported orphan derived target %q", target)
		}
	}
	seen := make(map[string]struct{}, len(p.GeneratedDirectories))
	for _, path := range p.GeneratedDirectories {
		if err := validateGeneratedPath(path); err != nil {
			return err
		}
		clean := filepath.ToSlash(filepath.Clean(path))
		for existing := range seen {
			if clean == existing || strings.HasPrefix(clean, existing+"/") || strings.HasPrefix(existing, clean+"/") {
				return fmt.Errorf("generated targets %q and %q overlap", existing, clean)
			}
		}
		seen[clean] = struct{}{}
	}
	return nil
}

func validateGeneratedPath(raw string) error {
	trimmed := strings.TrimSpace(raw)
	clean := filepath.Clean(trimmed)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("generated target %q is not an exact repository-relative directory", raw)
	}
	if raw != filepath.ToSlash(clean) {
		return fmt.Errorf("generated target %q is not canonical", raw)
	}
	if strings.ContainsAny(raw, "*?[]{}") {
		return fmt.Errorf("generated target %q contains a glob", raw)
	}
	for _, component := range strings.Split(filepath.ToSlash(clean), "/") {
		if component == ".git" || component == ".herd" {
			return fmt.Errorf("generated target %q intersects canonical %s state", raw, component)
		}
	}
	return nil
}

type WorktreeEnumerator interface {
	List(context.Context, string, string) ([]RegisteredWorktree, error)
}

// TrackedSourceInspector proves that an exact generated target contains no
// Git-tracked source. Repository policy alone is never deletion authority for
// tracked content.
type TrackedSourceInspector interface {
	HasTrackedSource(context.Context, string, string) (bool, error)
}

type PhysicalUsage struct {
	Bytes     uint64
	Entries   int
	Truncated bool
}

type PhysicalMeasurer interface {
	Measure(string, int) (PhysicalUsage, error)
}

type LockProvider interface {
	Acquire(context.Context, string, time.Duration, time.Duration) (io.Closer, error)
}

type RemoveTreeFunc func(string) error

type Governor struct {
	Policy     GovernorPolicy
	Capacity   StatFSBackend
	Worktrees  WorktreeEnumerator
	Source     TrackedSourceInspector
	Measure    PhysicalMeasurer
	Locks      LockProvider
	RemoveTree RemoveTreeFunc
	Processes  ProcessInspector
	OwnerID    func(os.FileInfo) (string, bool)
	Now        func() time.Time
	// SharedPopulation is the per-run host-wide process snapshot captured by
	// the registered census stage and reused by the orphan census stage. It
	// must alias the same *ProcessPopulation handed to the enumerator so a
	// run never rescans the population between the two stages.
	SharedPopulation *ProcessPopulation
}

type TargetDecision string

const (
	TargetWouldReap TargetDecision = "would_reap"
	TargetReaped    TargetDecision = "reaped"
	TargetAbsent    TargetDecision = "absent"
	TargetBlocked   TargetDecision = "blocked"
)

type TargetReport struct {
	Path               string         `json:"-"`
	ReportPath         string         `json:"path"`
	WorktreePath       string         `json:"-"`
	ReportWorktreePath string         `json:"worktree_path"`
	RelativePath       string         `json:"relative_path"`
	Decision           TargetDecision `json:"decision"`
	Reason             string         `json:"reason,omitempty"`
	BeforeBytes        uint64         `json:"before_bytes"`
	AfterBytes         uint64         `json:"after_bytes"`
	LastUse            time.Time      `json:"last_use"`
}

type ForeignTarget struct {
	Path       string `json:"-"`
	ReportPath string `json:"path"`
	Owner      string `json:"owner"`
	Kind       string `json:"kind"`
	AlertBytes uint64 `json:"alert_bytes"`
}

type ForeignTelemetry struct {
	ForeignTarget
	AllocatedBytes uint64 `json:"allocated_bytes"`
	Entries        int    `json:"entries"`
	Truncated      bool   `json:"truncated,omitempty"`
	Escalate       bool   `json:"escalate"`
	Action         string `json:"action"`
	Error          string `json:"error,omitempty"`
}

type GovernorReport struct {
	HostID                       string               `json:"host_id"`
	RepositoryID                 string               `json:"repository_id"`
	Mode                         string               `json:"mode"`
	ObservedAt                   time.Time            `json:"observed_at"`
	PressureBytes                uint64               `json:"pressure_bytes"`
	RecoveryBytes                uint64               `json:"recovery_bytes"`
	TaskReserveBytes             uint64               `json:"task_reserve_bytes"`
	EstimatedTaskReserveBytes    uint64               `json:"estimated_task_reserve_bytes"`
	CapacityBefore               Capacity             `json:"capacity_before"`
	CapacityAfter                Capacity             `json:"capacity_after"`
	CapacityAwareConcurrency     int                  `json:"capacity_aware_concurrency"`
	AvailableDispatchConcurrency int                  `json:"available_dispatch_concurrency"`
	Worktrees                    []RegisteredWorktree `json:"worktrees"`
	Orphans                      []OrphanWorktree     `json:"unregistered_orphans,omitempty"`
	OrphanTargets                []TargetReport       `json:"orphan_targets,omitempty"`
	Targets                      []TargetReport       `json:"targets"`
	Foreign                      []ForeignTelemetry   `json:"foreign,omitempty"`
	Reaped                       int                  `json:"reaped"`
	ReclaimedBytes               uint64               `json:"reclaimed_bytes"`
	OrphanCensusTruncated        bool                 `json:"orphan_census_truncated,omitempty"`
	OrphanCensusRemaining        int                  `json:"orphan_census_remaining,omitempty"`
	// Error is set on a nonzero-exit run so the structured partial report is
	// actionable on its own: the refusal stays, the cause is retained.
	Error  string        `json:"error,omitempty"`
	Stages []CensusStage `json:"census_stages,omitempty"`
}

// CensusStage records the bounded timing and counts of one census phase so a
// failed run exposes which stage consumed the budget instead of only where
// expiry was observed.
type CensusStage struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	Scanned    int    `json:"scanned"`
	Deferred   int    `json:"deferred"`
	Cause      string `json:"cause,omitempty"`
}

type orphanCensusResult struct {
	Orphans   []OrphanWorktree
	Truncated bool
	Remaining int
}

// OrphanWorktree is an unregistered child of a repository-declared known lane
// root. Its bytes are reported to make disk pressure actionable, while the
// complete checkout remains preserved because absence from Git is not proof
// of retirement.
type OrphanWorktree struct {
	Path                string                `json:"-"`
	ReportPath          string                `json:"path"`
	AllocatedBytes      uint64                `json:"allocated_bytes"`
	Entries             int                   `json:"entries"`
	AllocationTruncated bool                  `json:"allocation_truncated,omitempty"`
	DerivedTargets      []OrphanDerivedTarget `json:"derived_targets,omitempty"`
	PreserveReason      string                `json:"preserve_reason"`
}

type OrphanDerivedTarget struct {
	Path           string `json:"-"`
	RelativePath   string `json:"relative_path"`
	ReportPath     string `json:"report_path"`
	AllocatedBytes uint64 `json:"allocated_bytes"`
	Decision       string `json:"decision"`
	Reason         string `json:"reason"`
}

func fileOwnerID(info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return strconv.FormatUint(uint64(stat.Uid), 10), true
}

type RunOptions struct {
	Apply          bool
	BatchLimit     int
	ForeignTargets []ForeignTarget
}

type SweepTrigger string

const (
	SweepStartup             SweepTrigger = "startup"
	SweepPeriodic            SweepTrigger = "periodic"
	SweepPostVerdict         SweepTrigger = "post_verdict"
	SweepPostHarvest         SweepTrigger = "post_harvest"
	SweepReviewBeforeRefusal SweepTrigger = "review_before_refusal"
)

// Sweep is the coordinator-facing lifecycle seam. Callers invoke it at
// startup, periodically, after verdict/harvest, and before review refusal;
// trigger is retained in the report mode so those observations are auditable.
func (g *Governor) Sweep(ctx context.Context, trigger SweepTrigger, apply bool) (GovernorReport, error) {
	if trigger == "" {
		return GovernorReport{}, errors.New("resource governor sweep trigger is required")
	}
	report, err := g.Run(ctx, RunOptions{Apply: apply})
	if report.Mode != "" {
		report.Mode = string(trigger) + ":" + report.Mode
	}
	return report, err
}

// LifecycleApply reports whether repository policy permits lifecycle seams to
// act. ApplyBeforeDispatch controls only the dispatch admission path; startup,
// periodic, verdict, harvest, and review seams use this independent authority.
func (g *Governor) LifecycleApply() bool {
	return g != nil && g.Policy.AllowApply
}

func (g *Governor) defaults() {
	if g.Capacity == nil {
		g.Capacity = OSBackend{}
	}
	if g.Worktrees == nil {
		g.Worktrees = GitWorktreeEnumerator{}
	}
	if g.Measure == nil {
		g.Measure = OSPhysicalMeasurer{}
	}
	if g.Source == nil {
		g.Source = GitTrackedSourceInspector{}
	}
	if g.Locks == nil {
		g.Locks = FileLockProvider{}
	}
	if g.RemoveTree == nil {
		g.RemoveTree = os.RemoveAll
	}
	if g.Now == nil {
		g.Now = time.Now
	}
	if g.Processes == nil {
		g.Processes = LSOFProcessInspector{Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20}
	}
	if g.OwnerID == nil {
		g.OwnerID = fileOwnerID
	}
	if g.Policy.MaxScanEntries <= 0 {
		g.Policy.MaxScanEntries = 250000
	}
}

func (g *Governor) Run(ctx context.Context, options RunOptions) (report GovernorReport, err error) {
	if g == nil {
		return report, errors.New("resource governor unavailable")
	}
	g.defaults()
	if err := g.Policy.validate(); err != nil {
		return report, err
	}
	lock, err := g.Locks.Acquire(ctx, g.Policy.LockPath, g.Policy.LockTimeout, g.Policy.LockRetry)
	if err != nil {
		return report, fmt.Errorf("resource governor lock: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	report, err = g.runLocked(ctx, options)
	if err != nil {
		// A failed run still carries its structured partial report: the
		// refusal and the causal stage evidence travel together (FAC-613).
		report.Error = err.Error()
	}
	return report, err
}

func (g *Governor) runLocked(ctx context.Context, options RunOptions) (GovernorReport, error) {
	if options.BatchLimit < 0 || options.BatchLimit > g.Policy.ReapBatchLimit {
		return GovernorReport{}, fmt.Errorf("reap batch limit must be between 0 and repository limit %d", g.Policy.ReapBatchLimit)
	}
	if options.Apply && !g.Policy.AllowApply {
		return GovernorReport{}, errors.New("resource governor apply is not enabled by repository policy")
	}
	report, err := g.census(ctx)
	if err != nil {
		return report, err
	}
	if options.Apply {
		report.Mode = "apply"
	} else {
		report.Mode = "dry-run"
	}
	limit := options.BatchLimit
	if limit == 0 {
		limit = g.Policy.ReapBatchLimit
	}
	if options.Apply {
		recovered, recoveredBytes, recoverErr := g.recoverInterruptedQuarantines(ctx, &report, limit)
		if recoverErr != nil {
			return report, recoverErr
		}
		report.Reaped += recovered
		report.ReclaimedBytes += recoveredBytes
		if err := g.applyTargets(ctx, &report, limit); err != nil {
			return report, err
		}
		if err := g.applyOrphanTargets(ctx, &report, limit-report.Reaped); err != nil {
			return report, err
		}
	}
	after, err := g.Capacity.StatFS(g.Policy.RepositoryRoot)
	if err != nil {
		return report, fmt.Errorf("fresh post-reap statfs: %w", err)
	}
	if err := validCapacity(after); err != nil {
		return report, fmt.Errorf("fresh post-reap statfs: %w", err)
	}
	if safeDiskIdentity(after.FilesystemID) != safeDiskIdentity(report.CapacityBefore.FilesystemID) {
		return report, errors.New("fresh post-reap statfs changed filesystem identity")
	}
	report.CapacityAfter = after
	report.Foreign = g.inspectForeign(options.ForeignTargets)
	g.setConcurrency(&report)
	return report, nil
}

// now returns the injectable clock, defaulting to the real time.
func (g *Governor) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Governor) sinceMS(start time.Time) int64 {
	elapsed := g.now().Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.Milliseconds()
}

func (g *Governor) recordStage(report *GovernorReport, stage CensusStage) {
	report.Stages = append(report.Stages, stage)
}

func (g *Governor) census(ctx context.Context) (GovernorReport, error) {
	report := GovernorReport{}
	start := g.now()
	before, err := g.Capacity.StatFS(g.Policy.RepositoryRoot)
	statfsStage := CensusStage{Name: "statfs", DurationMS: g.sinceMS(start)}
	if err != nil {
		statfsStage.Cause = err.Error()
		report.Stages = append(report.Stages, statfsStage)
		report.Error = fmt.Sprintf("resource governor statfs: %v", err)
		return report, fmt.Errorf("resource governor statfs: %w", err)
	}
	if err := validCapacity(before); err != nil {
		statfsStage.Cause = err.Error()
		report.Stages = append(report.Stages, statfsStage)
		report.Error = fmt.Sprintf("resource governor statfs: %v", err)
		return report, fmt.Errorf("resource governor statfs: %w", err)
	}
	report.Stages = append(report.Stages, statfsStage)
	report.CapacityBefore, report.CapacityAfter = before, before

	listStart := g.now()
	lanes, listErr := g.Worktrees.List(ctx, g.Policy.RepositoryRoot, g.Policy.BaseRef)
	listStage := CensusStage{Name: "registered_census", DurationMS: g.sinceMS(listStart)}
	if listErr != nil {
		listStage.Cause = listErr.Error()
		report.Stages = append(report.Stages, listStage)
		report.Error = fmt.Sprintf("resource governor registered-worktree census: %v", listErr)
		return report, fmt.Errorf("resource governor registered-worktree census: %w", listErr)
	}
	listStage.Scanned = len(lanes)
	report.Stages = append(report.Stages, listStage)
	seen := make(map[string]struct{}, len(lanes))
	unknownLanes := 0
	for i := range lanes {
		resolved, resolveErr := filepath.EvalSymlinks(lanes[i].Path)
		if resolveErr != nil {
			lanes[i].State = LaneUnknown
			lanes[i].PreserveReason = "worktree_realpath_unavailable"
			unknownLanes++
			continue
		}
		lanes[i].Path = filepath.Clean(resolved)
		if _, exists := seen[lanes[i].Path]; exists {
			listStage.Cause = fmt.Sprintf("duplicate registered worktree realpath %q", lanes[i].Path)
			listStage.Deferred = unknownLanes
			g.recordStage(&report, listStage)
			report.Error = listStage.Cause
			return report, fmt.Errorf("duplicate registered worktree realpath %q", lanes[i].Path)
		}
		seen[lanes[i].Path] = struct{}{}
		usage, measureErr := g.Measure.Measure(lanes[i].Path, g.registeredMeasureLimit())
		if measureErr != nil {
			lanes[i].State = LaneUnknown
			lanes[i].PreserveReason = "worktree_allocation_unavailable"
			unknownLanes++
			continue
		}
		lanes[i].AllocatedBytes = usage.Bytes
		lanes[i].AllocationTruncated = usage.Truncated
		if usage.Truncated && lanes[i].PreserveReason == "" {
			lanes[i].PreserveReason = "worktree_allocation_truncated"
		}
	}
	listStage.Deferred = unknownLanes
	g.recordStage(&report, listStage)
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Path < lanes[j].Path })
	report.HostID = g.Policy.HostID
	report.RepositoryID = g.Policy.RepositoryID
	report.ObservedAt = g.Now().UTC()
	report.PressureBytes = g.Policy.PressureBytes
	report.RecoveryBytes = g.Policy.RecoveryBytes
	report.TaskReserveBytes = g.Policy.TaskReserveBytes
	report.Worktrees = lanes
	for i := range report.Worktrees {
		report.Worktrees[i].ReportPath = reportPath(g.Policy.RepositoryRoot, report.Worktrees[i].Path)
	}
	orphanStart := g.now()
	orphanResult, orphanErr := g.censusOrphans(ctx, report.Worktrees, before)
	orphanStage := CensusStage{
		Name: "unregistered_orphan_census", DurationMS: g.sinceMS(orphanStart),
		Scanned: len(orphanResult.Orphans), Deferred: orphanResult.Remaining,
	}
	if orphanErr != nil {
		orphanStage.Cause = orphanErr.Error()
		g.recordStage(&report, orphanStage)
		report.OrphanCensusTruncated = orphanResult.Truncated
		report.OrphanCensusRemaining = orphanResult.Remaining
		report.Orphans = orphanResult.Orphans
		report.Error = fmt.Sprintf("resource governor unregistered-orphan census: %v", orphanErr)
		return report, fmt.Errorf("resource governor unregistered-orphan census: %w", orphanErr)
	}
	g.recordStage(&report, orphanStage)
	orphans := orphanResult.Orphans
	report.OrphanCensusTruncated = orphanResult.Truncated
	report.OrphanCensusRemaining = orphanResult.Remaining
	report.Orphans = orphans
	for _, orphan := range orphans {
		for _, target := range orphan.DerivedTargets {
			if target.Decision == string(TargetWouldReap) {
				report.OrphanTargets = append(report.OrphanTargets, TargetReport{
					Path: target.Path, ReportPath: target.ReportPath, WorktreePath: orphan.Path,
					ReportWorktreePath: orphan.ReportPath, RelativePath: target.RelativePath,
					Decision: TargetWouldReap, Reason: target.Reason,
				})
			}
		}
	}
	targetStart := g.now()
	if err := ctx.Err(); err != nil {
		report.Error = fmt.Sprintf("registered target inspection cancelled before start: %v", err)
		return report, err
	}
	for _, lane := range lanes {
		for _, rel := range g.Policy.GeneratedDirectories {
			target := g.inspectTarget(ctx, lane, rel)
			target.ReportPath = reportPath(g.Policy.RepositoryRoot, target.Path)
			target.ReportWorktreePath = reportPath(g.Policy.RepositoryRoot, target.WorktreePath)
			report.Targets = append(report.Targets, target)
		}
	}
	g.recordStage(&report, CensusStage{Name: "registered_target_inspection", DurationMS: g.sinceMS(targetStart), Scanned: len(report.Targets)})
	report.EstimatedTaskReserveBytes = report.TaskReserveBytes
	for _, target := range report.Targets {
		if target.BeforeBytes > report.EstimatedTaskReserveBytes {
			report.EstimatedTaskReserveBytes = target.BeforeBytes
		}
	}
	sort.Slice(report.Targets, func(i, j int) bool {
		if !report.Targets[i].LastUse.Equal(report.Targets[j].LastUse) {
			return report.Targets[i].LastUse.Before(report.Targets[j].LastUse)
		}
		return report.Targets[i].Path < report.Targets[j].Path
	})
	g.setConcurrency(&report)
	return report, nil
}

func (g *Governor) censusOrphans(ctx context.Context, registered []RegisteredWorktree, before Capacity) (orphanCensusResult, error) {
	var result orphanCensusResult
	if len(g.Policy.OrphanRoots) == 0 {
		return result, nil
	}
	known := make(map[string]struct{}, len(registered))
	for _, lane := range registered {
		resolved, err := filepath.EvalSymlinks(lane.Path)
		if err == nil {
			known[filepath.Clean(resolved)] = struct{}{}
		}
	}
	repoRoot, err := filepath.EvalSymlinks(g.Policy.RepositoryRoot)
	if err != nil {
		return result, fmt.Errorf("resolve repository root for orphan census: %w", err)
	}
	seenRoots := make(map[string]struct{}, len(g.Policy.OrphanRoots))
	for _, rawRoot := range g.Policy.OrphanRoots {
		root, err := filepath.EvalSymlinks(rawRoot)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return result, fmt.Errorf("resolve known orphan root: %w", err)
		}
		root = filepath.Clean(root)
		if !containedPath(repoRoot, root) {
			return result, errors.New("known orphan root escapes repository root")
		}
		if _, duplicate := seenRoots[root]; duplicate {
			continue
		}
		seenRoots[root] = struct{}{}
		entries, err := os.ReadDir(root)
		if err != nil {
			return result, fmt.Errorf("read known orphan root: %w", err)
		}
		// Rotate the bounded window by a batch-sized time stride. A protected
		// early entry must not permanently starve later eligible entries across
		// sweeps, while each sweep remains bounded by orphanCensusLimit.
		limit := g.orphanCensusLimit()
		if len(entries) > 0 {
			bucket := (g.Now().Unix() / int64(time.Minute/time.Second)) % int64(len(entries))
			if bucket < 0 {
				bucket += int64(len(entries))
			}
			start := int((bucket * int64(limit)) % int64(len(entries)))
			entries = append(append([]os.DirEntry(nil), entries[start:]...), entries[:start]...)
		}
		rootProcessUsage := map[string]ProcessUsage(nil)
		var rootProcessErr error
		if batch, ok := g.Processes.(BatchProcessInspector); ok {
			rootProcessUsage = make(map[string]ProcessUsage)
			batchPaths := orphanBatchPaths(entries, root, known, limit, g.Policy.OrphanDerivedTargets)
			if len(batchPaths) > 0 {
				// FAC-613: reuse the population snapshot the registered
				// census stage captured on this run instead of rescanning
				// the process population and owner table between stages.
				// The per-path lsof and reference evidence stays fresh.
				if popAware, shared := batch.(PopulationAwareBatchInspector); shared && g.SharedPopulation != nil && len(g.SharedPopulation.PIDs) > 0 {
					rootProcessUsage, rootProcessErr = popAware.InUseManyPopulation(ctx, batchPaths, g.SharedPopulation)
				} else {
					rootProcessUsage, rootProcessErr = batch.InUseMany(ctx, batchPaths)
				}
				if rootProcessUsage == nil {
					rootProcessUsage = make(map[string]ProcessUsage)
				}
				if err := ctx.Err(); err != nil {
					return result, err
				}
			}
		}
		candidates := 0
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			path := filepath.Join(root, entry.Name())
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				continue
			}
			resolved = filepath.Clean(resolved)
			if _, ok := known[resolved]; ok {
				continue
			}
			if candidates >= limit {
				result.Truncated = true
				result.Remaining++
				continue
			}
			candidates++
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				continue
			}
			usage, measureErr := g.Measure.Measure(path, g.orphanMeasureLimit())
			if measureErr != nil {
				result.Orphans = append(result.Orphans, OrphanWorktree{Path: path, ReportPath: reportPath(g.Policy.RepositoryRoot, path), PreserveReason: "orphan_allocation_unavailable"})
				continue
			}
			orphan := OrphanWorktree{Path: path, ReportPath: reportPath(g.Policy.RepositoryRoot, path), AllocatedBytes: usage.Bytes, Entries: usage.Entries, AllocationTruncated: usage.Truncated, PreserveReason: "unregistered_worktree_authority_unavailable"}
			processUsage, processErr := rootProcessUsage, rootProcessErr
			for _, rel := range g.Policy.OrphanDerivedTargets {
				target, targetRel, resolveErr := orphanTargetPath(path, rel)
				row := OrphanDerivedTarget{Path: target, RelativePath: targetRel, ReportPath: reportPath(g.Policy.RepositoryRoot, target), Decision: "blocked", Reason: "orphan_derived_target_evidence_unavailable"}
				if resolveErr != nil {
					row.Reason = resolveErr.Error()
					orphan.DerivedTargets = append(orphan.DerivedTargets, row)
					continue
				}
				usage, eligible, reason := g.orphanTargetProofWithProcess(ctx, path, target, rel, before, processUsage, processErr)
				row.AllocatedBytes, row.Decision, row.Reason = usage.Bytes, "blocked", reason
				if eligible {
					row.Decision = string(TargetWouldReap)
				}
				orphan.DerivedTargets = append(orphan.DerivedTargets, row)
			}
			result.Orphans = append(result.Orphans, orphan)
		}
	}
	sort.Slice(result.Orphans, func(i, j int) bool { return result.Orphans[i].Path < result.Orphans[j].Path })
	return result, nil
}

// orphanCensusLimit bounds expensive recursive accounting and process proof.
// It is deliberately several batches wide so repeated periodic sweeps make
// progress after earlier candidates are reclaimed, while unknown/deferred
// candidates remain conservatively preserved and reported as partial census.
func (g *Governor) orphanCensusLimit() int {
	limit := g.Policy.ReapBatchLimit * 4
	if limit < 16 {
		limit = 16
	}
	if limit > 64 {
		limit = 64
	}
	return limit
}

func orphanBatchPaths(entries []os.DirEntry, root string, known map[string]struct{}, limit int, targets []string) []string {
	paths := make([]string, 0, limit*len(targets))
	candidates := 0
	for _, entry := range entries {
		if candidates >= limit {
			break
		}
		path := filepath.Join(root, entry.Name())
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if _, ok := known[filepath.Clean(resolved)]; ok {
			continue
		}
		candidates++
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		for _, rel := range targets {
			target, _, err := orphanTargetPath(path, rel)
			if err != nil {
				continue
			}
			info, err := os.Lstat(target)
			if err == nil && info.Mode()&os.ModeSymlink == 0 {
				paths = append(paths, target)
			}
		}
	}
	return paths
}

func (g *Governor) orphanMeasureLimit() int {
	limit := g.Policy.MaxScanEntries
	if limit <= 0 || limit > 4096 {
		return 4096
	}
	return limit
}

func (g *Governor) registeredMeasureLimit() int {
	// Registered worktree accounting is capacity telemetry, not destructive
	// authority. Keep it bounded across large fleets; target proofs below still
	// use MaxScanEntries and refuse truncated ownership evidence.
	limit := g.Policy.MaxScanEntries
	if limit <= 0 || limit > 4096 {
		return 4096
	}
	return limit
}

type bootstrapReceiptEvidence struct {
	Version         int    `json:"version"`
	ContractDigest  string `json:"contract_digest"`
	ToolchainDigest string `json:"toolchain_digest"`
	CacheDir        string `json:"cache_dir"`
}

func orphanTargetPath(orphan, policyTarget string) (string, string, error) {
	switch policyTarget {
	case "graph.db":
		return filepath.Join(orphan, "graph.db"), "graph.db", nil
	case "bootstrap-go-mod", "bootstrap-go-build":
		data, err := os.ReadFile(filepath.Join(orphan, filepath.FromSlash(gitroot.BootstrapReceiptPath)))
		if err != nil {
			return "", "", errors.New("bootstrap_receipt_unavailable")
		}
		var receipt bootstrapReceiptEvidence
		if err := json.Unmarshal(data, &receipt); err != nil || receipt.Version != 1 || receipt.ContractDigest == "" || len(receipt.ToolchainDigest) != 64 || receipt.CacheDir == "" {
			return "", "", errors.New("bootstrap_receipt_invalid")
		}
		wantPrefix := filepath.ToSlash(filepath.Join(".herd", "bootstrap", "cache")) + "/"
		cache := filepath.ToSlash(filepath.Clean(receipt.CacheDir))
		if !strings.HasPrefix(cache, wantPrefix) || strings.Count(strings.TrimPrefix(cache, wantPrefix), "/") != 0 {
			return "", "", errors.New("bootstrap_cache_receipt_path_invalid")
		}
		child := "go-mod"
		if policyTarget == "bootstrap-go-build" {
			child = "go-build"
		}
		return filepath.Join(orphan, filepath.FromSlash(cache), child), filepath.ToSlash(filepath.Join(cache, child)), nil
	default:
		return "", "", errors.New("orphan_derived_target_policy_invalid")
	}
}

func (g *Governor) orphanTargetProof(ctx context.Context, orphan, target, policyTarget string, before Capacity) (PhysicalUsage, bool, string) {
	return g.orphanTargetProofWithProcess(ctx, orphan, target, policyTarget, before, nil, nil)
}

func (g *Governor) orphanTargetProofWithProcess(ctx context.Context, orphan, target, policyTarget string, before Capacity, processUsage map[string]ProcessUsage, processErr error) (PhysicalUsage, bool, string) {
	expected, _, err := orphanTargetPath(orphan, policyTarget)
	if err != nil || filepath.Clean(expected) != filepath.Clean(target) {
		return PhysicalUsage{}, false, "derived_target_authority_changed"
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return PhysicalUsage{}, false, "derived_target_absent"
	}
	if err != nil {
		return PhysicalUsage{}, false, "derived_target_lstat_unavailable"
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return PhysicalUsage{}, false, "derived_target_symlink"
	}
	if (policyTarget == "bootstrap-go-mod" || policyTarget == "bootstrap-go-build") && !info.IsDir() {
		return PhysicalUsage{}, false, "bootstrap_cache_not_directory"
	}
	if policyTarget == "graph.db" && !info.Mode().IsRegular() {
		return PhysicalUsage{}, false, "graph_index_not_regular_file"
	}
	resolved, err := filepath.EvalSymlinks(target)
	root, rootErr := filepath.EvalSymlinks(orphan)
	if err != nil || rootErr != nil || !containedPath(root, resolved) {
		return PhysicalUsage{}, false, "derived_target_realpath_escape"
	}
	owner, owned := g.OwnerID(info)
	if !owned || owner != strconv.Itoa(os.Getuid()) {
		return PhysicalUsage{}, false, "derived_target_foreign_uid"
	}
	// Foreign processes whose metadata is inaccessible cannot reference a
	// target behind an owner-only ancestor. Bootstrap producers create the
	// digest cache privately, then tools may create its child (including go-mod)
	// as 0755. Require that private provenance rather than imposing 0700 on the
	// producer's leaf mode; shared/unknown ancestry remains blocked.
	private, privacyReason := privateTargetProvenance(orphan, target, owner)
	if !private {
		return PhysicalUsage{}, false, privacyReason
	}
	usage, err := g.Measure.Measure(target, g.Policy.MaxScanEntries)
	if err != nil || usage.Truncated {
		return usage, false, "derived_target_allocation_unavailable"
	}
	var process ProcessUsage
	if processUsage != nil {
		var ok bool
		process, ok = processUsage[filepath.Clean(resolved)]
		if !ok || processErr != nil {
			return usage, false, "derived_target_process_evidence_unavailable"
		}
	} else {
		process, err = g.Processes.InUse(ctx, target)
		if err != nil {
			return usage, false, "derived_target_process_evidence_unavailable"
		}
	}
	if active, reason := managedCacheLeaseActive(ctx, g.Policy.RepositoryRoot, orphan, target, g.Now()); active {
		return usage, false, reason
	}
	if process.MetadataUnavailable || process.CWD || process.OpenFile || process.ReferencedPath {
		return usage, false, "derived_target_active_process"
	}
	if g.Policy.OrphanCacheTTL <= 0 {
		return usage, false, "orphan_cache_ttl_unconfigured"
	}
	pressure := before.FreeBytes < g.Policy.PressureBytes+g.Policy.TaskReserveBytes
	if !pressure && g.Now().Sub(info.ModTime()) < g.Policy.OrphanCacheTTL {
		return usage, false, "orphan_cache_ttl_not_reached"
	}
	return usage, true, "proof_backed_regenerable_cache"
}

type managedCacheLease struct {
	Version         int    `json:"version"`
	CacheDir        string `json:"cache_dir"`
	ToolchainDigest string `json:"toolchain_digest"`
	Consumer        struct {
		Repository      string `json:"Repository"`
		TaskRef         string `json:"TaskRef"`
		LeaseGeneration int64  `json:"LeaseGeneration"`
		Worktree        string `json:"Worktree"`
	} `json:"consumer"`
	Process struct {
		PID        int    `json:"pid"`
		ParentPID  int    `json:"parent_pid"`
		StartToken string `json:"start_token"`
	} `json:"process"`
	ExpiresAt time.Time `json:"expires_at"`
}

func managedCacheLeaseActive(ctx context.Context, repositoryRoot, orphan, target string, now time.Time) (bool, string) {
	path := filepath.Join(orphan, ".herd", "bootstrap", "cache-use.lock")
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, leaseErr := os.Stat(filepath.Join(orphan, ".herd", "bootstrap", "cache-use.json")); errors.Is(leaseErr, os.ErrNotExist) {
				return false, ""
			}
		}
		return true, "managed_cache_lease_lock_unavailable"
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return true, "managed_cache_lease_lock_unavailable"
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return managedCacheLeaseActiveUnlocked(ctx, repositoryRoot, orphan, target, now)
}

func managedCacheLeaseActiveUnlocked(ctx context.Context, repositoryRoot, orphan, target string, now time.Time) (bool, string) {
	path := filepath.Join(orphan, ".herd", "bootstrap", "cache-use.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, ""
	}
	if err != nil {
		return true, "managed_cache_lease_unreadable"
	}
	var lease managedCacheLease
	root, rootErr := filepath.EvalSymlinks(repositoryRoot)
	worktreeRel, relErr := filepath.Rel(root, orphan)
	if rootErr != nil || relErr != nil || json.Unmarshal(data, &lease) != nil || lease.Version != 1 || lease.CacheDir == "" || len(lease.ToolchainDigest) != 64 || lease.Consumer.Repository == "" || lease.Consumer.TaskRef == "" || lease.Consumer.LeaseGeneration <= 0 || filepath.Clean(filepath.FromSlash(lease.Consumer.Worktree)) != filepath.Clean(worktreeRel) || lease.Process.PID <= 0 || lease.Process.ParentPID <= 0 || lease.Process.StartToken == "" || lease.ExpiresAt.IsZero() {
		return true, "managed_cache_lease_invalid"
	}
	cacheRoot := filepath.Clean(filepath.Join(orphan, filepath.FromSlash(lease.CacheDir)))
	if !containedPath(cacheRoot, filepath.Clean(target)) {
		return true, "managed_cache_lease_target_mismatch"
	}
	if lease.ExpiresAt.After(now) {
		return true, "managed_cache_lease_active"
	}
	current, present, err := processLeaseIdentity(ctx, lease.Process.PID)
	if err != nil {
		return true, "managed_cache_lease_identity_unknown"
	}
	if !present {
		return false, ""
	}
	if current.ParentPID != lease.Process.ParentPID || current.StartToken != lease.Process.StartToken {
		return true, "managed_cache_lease_pid_reused"
	}
	return true, "managed_cache_lease_expired_process_live"
}

type leaseProcessIdentity struct {
	ParentPID  int
	StartToken string
}

func processLeaseIdentity(ctx context.Context, pid int) (leaseProcessIdentity, bool, error) {
	ps, err := exec.LookPath("ps")
	if err != nil {
		return leaseProcessIdentity{}, false, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, ps, "-p", strconv.Itoa(pid), "-o", "ppid=,lstart=").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return leaseProcessIdentity{}, false, nil
		}
		return leaseProcessIdentity{}, false, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return leaseProcessIdentity{}, false, errors.New("process identity is incomplete")
	}
	parent, err := strconv.Atoi(fields[0])
	if err != nil || parent <= 0 {
		return leaseProcessIdentity{}, false, errors.New("process parent identity is invalid")
	}
	return leaseProcessIdentity{ParentPID: parent, StartToken: strings.Join(fields[1:], " ")}, true, nil
}

func privateTargetProvenance(orphan, target, owner string) (bool, string) {
	current := filepath.Clean(target)
	root := filepath.Clean(orphan)
	privateAncestor := false
	for {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false, "derived_target_shared_permissions"
		}
		actualOwner, owned := fileOwnerID(info)
		if !owned || actualOwner != owner {
			return false, "derived_target_foreign_uid"
		}
		if info.Mode().Perm()&0o077 == 0 {
			privateAncestor = true
		}
		if current == root {
			if privateAncestor {
				return true, ""
			}
			return false, "derived_target_shared_permissions"
		}
		parent := filepath.Dir(current)
		if parent == current || !containedPath(root, parent) {
			return false, "derived_target_shared_permissions"
		}
		current = parent
	}
}

func (g *Governor) applyOrphanTargets(ctx context.Context, report *GovernorReport, limit int) error {
	if limit <= 0 || len(report.OrphanTargets) == 0 {
		return nil
	}
	used := uint64(0)
	for i := range report.OrphanTargets {
		if report.Reaped >= limit || report.OrphanTargets[i].Decision != TargetWouldReap {
			continue
		}
		if used >= g.Policy.OrphanCacheBudgetBytes {
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, "orphan_cache_budget_exhausted"
			continue
		}
		orphan := report.OrphanTargets[i].WorktreePath
		policyTarget := "graph.db"
		if strings.HasSuffix(report.OrphanTargets[i].RelativePath, "/go-mod") {
			policyTarget = "bootstrap-go-mod"
		} else if strings.HasSuffix(report.OrphanTargets[i].RelativePath, "/go-build") {
			policyTarget = "bootstrap-go-build"
		}
		usage, eligible, reason := g.orphanTargetProof(ctx, orphan, report.OrphanTargets[i].Path, policyTarget, report.CapacityBefore)
		if !eligible {
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, reason
			continue
		}
		lockPath := filepath.Join(orphan, ".herd", "bootstrap", "cache-use.lock")
		lockFile, lockErr := os.OpenFile(lockPath, os.O_RDWR, 0o600)
		locked := lockErr == nil && syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX) == nil
		if !locked && !errors.Is(lockErr, os.ErrNotExist) {
			if lockFile != nil {
				_ = lockFile.Close()
			}
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, "managed_cache_lease_lock_unavailable"
			continue
		}
		if !locked {
			if _, leaseErr := os.Stat(filepath.Join(orphan, ".herd", "bootstrap", "cache-use.json")); !errors.Is(leaseErr, os.ErrNotExist) {
				report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, "managed_cache_lease_lock_unavailable"
				continue
			}
		}
		active, activeReason := managedCacheLeaseActiveUnlocked(ctx, g.Policy.RepositoryRoot, orphan, report.OrphanTargets[i].Path, g.Now())
		if active {
			if locked {
				_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
				_ = lockFile.Close()
			}
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, activeReason
			continue
		}
		if used > g.Policy.OrphanCacheBudgetBytes-usage.Bytes {
			if locked {
				_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
				_ = lockFile.Close()
			}
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, "orphan_cache_budget_exhausted"
			continue
		}
		if err := safeRemoveGeneratedTree(g.Policy.RepositoryRoot, report.OrphanTargets[i].Path, g.RemoveTree); err != nil {
			if locked {
				_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
				_ = lockFile.Close()
			}
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, "orphan_cache_remove_failed"
			return fmt.Errorf("remove exact orphan cache %q: %w", report.OrphanTargets[i].Path, err)
		}
		after, err := g.Measure.Measure(report.OrphanTargets[i].Path, g.Policy.MaxScanEntries)
		if errors.Is(err, os.ErrNotExist) {
			after = PhysicalUsage{}
			err = nil
		}
		if err != nil {
			if locked {
				_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
				_ = lockFile.Close()
			}
			return fmt.Errorf("orphan cache post-reap readback: %w", err)
		}
		if locked {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
		}
		report.OrphanTargets[i].Decision = TargetReaped
		report.OrphanTargets[i].BeforeBytes, report.OrphanTargets[i].AfterBytes = usage.Bytes, after.Bytes
		report.Reaped++
		used += usage.Bytes
		if usage.Bytes >= after.Bytes {
			report.ReclaimedBytes += usage.Bytes - after.Bytes
		}
	}
	return nil
}

func reportPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "./" + filepath.ToSlash(rel)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return fmt.Sprintf("./external/%x", sum[:8])
}

func (g *Governor) inspectTarget(ctx context.Context, lane RegisteredWorktree, rel string) TargetReport {
	target := TargetReport{WorktreePath: lane.Path, RelativePath: filepath.ToSlash(filepath.Clean(rel)), LastUse: lane.LastUse}
	target.Path = filepath.Join(lane.Path, filepath.FromSlash(target.RelativePath))
	if reason := laneBlockReason(lane); reason != "" {
		target.Decision, target.Reason = TargetBlocked, reason
		return target
	}
	tracked, err := g.Source.HasTrackedSource(ctx, lane.Path, target.RelativePath)
	if err != nil {
		target.Decision, target.Reason = TargetBlocked, "tracked_source_evidence_unavailable"
		return target
	}
	if tracked {
		target.Decision, target.Reason = TargetBlocked, "tracked_source"
		return target
	}
	info, err := os.Lstat(target.Path)
	if errors.Is(err, os.ErrNotExist) {
		target.Decision = TargetAbsent
		return target
	}
	if err != nil {
		target.Decision, target.Reason = TargetBlocked, "target_lstat_unavailable"
		return target
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target.Decision, target.Reason = TargetBlocked, "target_symlink"
		return target
	}
	if !info.IsDir() {
		target.Decision, target.Reason = TargetBlocked, "target_not_directory"
		return target
	}
	resolved, err := filepath.EvalSymlinks(target.Path)
	if err != nil || !containedPath(lane.Path, resolved) {
		target.Decision, target.Reason = TargetBlocked, "target_realpath_escape"
		return target
	}
	usage, err := g.Measure.Measure(target.Path, g.Policy.MaxScanEntries)
	if err != nil {
		target.Decision, target.Reason = TargetBlocked, "target_allocation_unavailable"
		return target
	}
	target.BeforeBytes = usage.Bytes
	if usage.Truncated {
		target.Decision, target.Reason = TargetBlocked, "target_allocation_truncated"
		return target
	}
	canonical, scanErr := containsCanonicalState(target.Path, g.Policy.MaxScanEntries)
	if scanErr != nil {
		target.Decision, target.Reason = TargetBlocked, "canonical_state_scan_unavailable"
		return target
	}
	if canonical {
		target.Decision, target.Reason = TargetBlocked, "nested_canonical_state"
		return target
	}
	target.Decision = TargetWouldReap
	return target
}

func laneBlockReason(lane RegisteredWorktree) string {
	if lane.PreserveReason != "" {
		return lane.PreserveReason
	}
	if lane.Category == LaneCurrent {
		return "canonical_checkout"
	}
	if (lane.Category == LaneReviewPool || lane.Category == LaneReviewSurface) && !lane.ReviewHandoffAdmitted {
		return "review_candidate_immutable"
	}
	if lane.FailedCandidate {
		return "failed_candidate_immutable"
	}
	if lane.Unmerged {
		return "unmerged_candidate"
	}
	if lane.Dirty {
		return "dirty_source"
	}
	if lane.Untracked {
		return "untracked_source"
	}
	if lane.ActiveLease {
		return "active_lease"
	}
	if lane.Held || lane.State == LaneHeld {
		return "held_lane"
	}
	if lane.ActiveCWD {
		return "active_process_cwd"
	}
	if lane.OpenFile {
		return "active_process_open_file"
	}
	if lane.State == LaneActive {
		return "active_lane"
	}
	if lane.State == LaneUnknown {
		return "lane_state_unknown"
	}
	return ""
}

func containedPath(root, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (g *Governor) applyTargets(ctx context.Context, report *GovernorReport, limit int) error {
	for i := range report.Targets {
		if report.Reaped >= limit || report.Targets[i].Decision != TargetWouldReap {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("resource governor apply timeout: %w", err)
		}
		fresh, err := g.Worktrees.List(ctx, g.Policy.RepositoryRoot, g.Policy.BaseRef)
		if err != nil {
			return fmt.Errorf("revalidate registered worktrees: %w", err)
		}
		lane, ok := exactLane(fresh, report.Targets[i].WorktreePath)
		if !ok {
			report.Targets[i].Decision, report.Targets[i].Reason = TargetBlocked, "worktree_no_longer_registered"
			continue
		}
		again := g.inspectTarget(ctx, lane, report.Targets[i].RelativePath)
		if again.Decision != TargetWouldReap || again.Path != report.Targets[i].Path {
			report.Targets[i] = again
			continue
		}
		if err := safeRemoveGeneratedTree(g.Policy.RepositoryRoot, again.Path, g.RemoveTree); err != nil {
			report.Targets[i].Decision, report.Targets[i].Reason = TargetBlocked, "remove_failed"
			return fmt.Errorf("remove exact generated target %q: %w", again.Path, err)
		}
		after, measureErr := g.Measure.Measure(again.Path, g.Policy.MaxScanEntries)
		if errors.Is(measureErr, os.ErrNotExist) {
			after = PhysicalUsage{}
			measureErr = nil
		}
		if measureErr != nil {
			return fmt.Errorf("post-reap physical-byte readback %q: %w", again.Path, measureErr)
		}
		report.Targets[i].Decision = TargetReaped
		report.Targets[i].BeforeBytes = again.BeforeBytes
		report.Targets[i].AfterBytes = after.Bytes
		report.Reaped++
		if again.BeforeBytes >= after.Bytes {
			report.ReclaimedBytes += again.BeforeBytes - after.Bytes
		}
	}
	return nil
}

func containsCanonicalState(root string, maxEntries int) (bool, error) {
	found := false
	entries := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if maxEntries > 0 && entries > maxEntries {
			return errors.New("canonical state scan exceeded configured bound")
		}
		if path != root && (entry.Name() == ".git" || entry.Name() == ".herd") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

// safeRemoveGeneratedTree turns the exact target into an unlinked quarantine
// entry before deleting it. The parent and target are realpath-checked again,
// so a symlink retarget cannot redirect RemoveTree into foreign state.
func safeRemoveGeneratedTree(repoRoot, target string, remove RemoveTreeFunc) error {
	root, err := filepath.EvalSymlinks(repoRoot)
	targetAbs, absErr := filepath.Abs(target)
	if err != nil || absErr != nil {
		return errors.New("generated target containment changed before removal")
	}
	parent := filepath.Dir(targetAbs)
	parentResolved, err := filepath.EvalSymlinks(parent)
	if err != nil || !containedPath(root, parentResolved) {
		return errors.New("generated target parent realpath changed before removal")
	}
	resolved, err := filepath.EvalSymlinks(targetAbs)
	expected := filepath.Join(parentResolved, filepath.Base(targetAbs))
	if err != nil || !containedPath(root, resolved) || filepath.Clean(resolved) != filepath.Clean(expected) {
		return errors.New("generated target realpath changed before removal")
	}
	info, err := os.Lstat(targetAbs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("generated target identity unavailable before removal")
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("stat generated target parent before removal: %w", err)
	}
	quarantine, err := os.MkdirTemp(parent, ".herd-resource-reap-")
	if err != nil {
		return fmt.Errorf("create removal quarantine: %w", err)
	}
	quarantined := filepath.Join(quarantine, filepath.Base(resolved))
	recovery := filepath.Join(root, ".herd", "resource-reap-recovery.jsonl")
	if err := appendRecoveryIntent(recovery, root, resolved, quarantine, info); err != nil {
		_ = os.Remove(quarantine)
		return fmt.Errorf("record generated target removal intent: %w", err)
	}
	if err := os.Rename(resolved, quarantined); err != nil {
		cleanupErr := os.Remove(quarantine)
		journalErr := appendRecoveryRecord(recovery, root, resolved, quarantine, err)
		return errors.Join(fmt.Errorf("quarantine generated target: %w", err), cleanupErr, journalErr)
	}
	currentParent, statErr := os.Stat(parent)
	if statErr != nil || !os.SameFile(parentInfo, currentParent) {
		if rollbackErr := os.Rename(quarantined, resolved); rollbackErr != nil {
			return fmt.Errorf("generated target parent changed and rollback failed: %v; quarantine retained at %s", statErr, quarantine)
		}
		if cleanupErr := os.Remove(quarantine); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return fmt.Errorf("generated target parent changed during removal; quarantine cleanup: %w", cleanupErr)
		}
		return errors.Join(errors.New("generated target parent changed during removal"), appendRecoveryRecord(recovery, root, resolved, quarantine, errors.New("parent changed")))
	}
	if err := remove(quarantine); err != nil {
		if rollbackErr := os.Rename(quarantined, resolved); rollbackErr != nil {
			recordErr := appendRecoveryRecord(recovery, root, resolved, quarantine, err)
			if recordErr != nil {
				return fmt.Errorf("remove quarantined generated target: %v; rollback failed: %v; recovery record failed: %v; quarantine retained at %s", err, rollbackErr, recordErr, quarantine)
			}
			return fmt.Errorf("remove quarantined generated target: %v; rollback failed: %v; recovery record=%s; quarantine retained at %s", err, rollbackErr, reportPath(root, recovery), quarantine)
		}
		if cleanupErr := os.Remove(quarantine); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return fmt.Errorf("remove quarantined generated target: %v; rollback cleanup failed: %w", err, cleanupErr)
		}
		return errors.Join(fmt.Errorf("remove quarantined generated target: %w", err), appendRecoveryRecord(recovery, root, resolved, quarantine, err))
	}
	if err := os.Remove(quarantine); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty quarantine: %w", err)
	}
	if err := appendRecoveryCompletion(recovery, root, resolved, quarantine); err != nil {
		return fmt.Errorf("record generated target removal completion: %w", err)
	}
	return nil
}

type recoveryRecord struct {
	Phase      string `json:"phase"`
	Target     string `json:"target"`
	Quarantine string `json:"quarantine"`
	Identity   string `json:"identity,omitempty"`
	Owner      string `json:"owner,omitempty"`
}

func appendRecoveryIntent(path, root, target, quarantine string, info os.FileInfo) error {
	owner, owned := fileOwnerID(info)
	if !owned {
		return errors.New("generated target owner identity unavailable")
	}
	return appendRecoveryLine(path, recoveryRecord{Phase: "intent", Target: reportPath(root, target), Quarantine: reportPath(root, quarantine), Identity: fileIdentity(info), Owner: owner})
}

func appendRecoveryCompletion(path, root, target, quarantine string) error {
	return appendRecoveryLine(path, recoveryRecord{Phase: "complete", Target: reportPath(root, target), Quarantine: reportPath(root, quarantine)})
}

func appendRecoveryLine(path string, record recoveryRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create recovery record directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open recovery record: %w", err)
	}
	data, marshalErr := json.Marshal(record)
	if marshalErr == nil {
		_, marshalErr = file.Write(append(data, '\n'))
	}
	if marshalErr == nil {
		marshalErr = file.Sync()
	}
	return errors.Join(marshalErr, file.Close())
}

func fileIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%T:%v:%v", stat, stat.Dev, stat.Ino)
	}
	return fmt.Sprintf("%T:%v:%s:%d:%d", info.Sys(), info.Sys(), info.Mode(), info.Size(), info.ModTime().UTC().UnixNano())
}

func (g *Governor) recoverInterruptedQuarantines(ctx context.Context, report *GovernorReport, limit int) (int, uint64, error) {
	path := filepath.Join(g.Policy.RepositoryRoot, ".herd", "resource-reap-recovery.jsonl")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read recovery journal: %w", err)
	}
	latest := make(map[string]recoveryRecord)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record recoveryRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.Target == "" || record.Quarantine == "" {
			return 0, 0, errors.New("recovery journal contains invalid proof")
		}
		latest[record.Target+"\x00"+record.Quarantine] = record
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var count int
	var reclaimed uint64
	for _, key := range keys {
		record := latest[key]
		if record.Phase != "intent" {
			continue
		}
		if count >= limit {
			break
		}
		target, quarantine, resolveErr := recoveryPaths(g.Policy.RepositoryRoot, record)
		if resolveErr != nil {
			return count, reclaimed, resolveErr
		}
		if _, statErr := os.Lstat(target); statErr == nil {
			return count, reclaimed, errors.New("recovery intent target was recreated; refusing quarantine cleanup")
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return count, reclaimed, fmt.Errorf("inspect recovery target: %w", statErr)
		}
		entries, readErr := os.ReadDir(quarantine)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				if err := appendRecoveryCompletion(path, g.Policy.RepositoryRoot, target, quarantine); err != nil {
					return count, reclaimed, err
				}
				continue
			}
			return count, reclaimed, fmt.Errorf("inspect recovery quarantine: %w", readErr)
		}
		if len(entries) != 1 || entries[0].Name() != filepath.Base(target) {
			return count, reclaimed, errors.New("recovery quarantine contents are not exact")
		}
		child := filepath.Join(quarantine, entries[0].Name())
		info, statErr := os.Lstat(child)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || fileIdentity(info) != record.Identity {
			return count, reclaimed, errors.New("recovery quarantine identity proof failed")
		}
		owner, owned := fileOwnerID(info)
		if !owned || owner != record.Owner {
			return count, reclaimed, errors.New("recovery quarantine owner proof failed")
		}
		usage, measureErr := g.Measure.Measure(child, g.Policy.MaxScanEntries)
		if measureErr != nil || usage.Truncated {
			return count, reclaimed, errors.New("recovery quarantine allocation proof unavailable")
		}
		process, processErr := g.Processes.InUse(ctx, child)
		if processErr != nil || process.MetadataUnavailable || process.CWD || process.OpenFile || process.ReferencedPath {
			return count, reclaimed, errors.New("recovery quarantine active process proof unavailable")
		}
		if err := g.RemoveTree(quarantine); err != nil {
			return count, reclaimed, fmt.Errorf("recover interrupted quarantine: %w", err)
		}
		if err := appendRecoveryCompletion(path, g.Policy.RepositoryRoot, target, quarantine); err != nil {
			return count, reclaimed, err
		}
		count++
		reclaimed += usage.Bytes
	}
	return count, uint64(reclaimed), nil
}

func recoveryPaths(root string, record recoveryRecord) (string, string, error) {
	paths := make([]string, 2)
	for i, reported := range []string{record.Target, record.Quarantine} {
		if !strings.HasPrefix(reported, "./") || strings.Contains(reported, "..") {
			return "", "", errors.New("recovery journal path is not repository-relative")
		}
		paths[i] = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(reported, "./")))
		resolved, err := filepath.EvalSymlinks(filepath.Dir(paths[i]))
		if err != nil || !containedPath(root, resolved) {
			return "", "", errors.New("recovery journal path escapes repository")
		}
	}
	return paths[0], paths[1], nil
}

func appendRecoveryRecord(path, root, target, quarantine string, reason error) error {
	return appendRecoveryLine(path, recoveryRecord{Phase: "failed", Target: reportPath(root, target), Quarantine: reportPath(root, quarantine), Identity: reason.Error()})
}

func exactLane(lanes []RegisteredWorktree, path string) (RegisteredWorktree, bool) {
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		return RegisteredWorktree{}, false
	}
	for _, lane := range lanes {
		got, resolveErr := filepath.EvalSymlinks(lane.Path)
		if resolveErr == nil && filepath.Clean(got) == filepath.Clean(want) {
			lane.Path = filepath.Clean(got)
			return lane, true
		}
	}
	return RegisteredWorktree{}, false
}

func (g *Governor) setConcurrency(report *GovernorReport) {
	free := report.CapacityAfter.FreeBytes
	reserve := report.EstimatedTaskReserveBytes
	if reserve == 0 {
		reserve = g.Policy.TaskReserveBytes
	}
	ceiling := 0
	if free > g.Policy.PressureBytes {
		slots := (free - g.Policy.PressureBytes) / reserve
		// Policy validation requires MaxDispatchConcurrency > 0. A positive
		// int is representable as uint64, and the reverse conversion below is
		// fenced by this same bound.
		maxSlots := uint64(g.Policy.MaxDispatchConcurrency) // #nosec G115 -- bounded by validated positive int
		if slots >= maxSlots {
			ceiling = g.Policy.MaxDispatchConcurrency
		} else {
			ceiling = int(slots) // #nosec G115 -- slots is strictly below validated MaxDispatchConcurrency
		}
	}
	active := 0
	for _, lane := range report.Worktrees {
		if lane.State == LaneActive && lane.Category != LaneCurrent && lane.Category != LaneHarvest {
			active++
		}
	}
	report.CapacityAwareConcurrency = ceiling
	report.AvailableDispatchConcurrency = ceiling - active
	if report.AvailableDispatchConcurrency < 0 {
		report.AvailableDispatchConcurrency = 0
	}
}

func (g *Governor) inspectForeign(targets []ForeignTarget) []ForeignTelemetry {
	out := make([]ForeignTelemetry, 0, len(targets))
	for _, target := range targets {
		row := ForeignTelemetry{ForeignTarget: target, Action: "observe_only_contact_owner"}
		row.ReportPath = reportPath(g.Policy.RepositoryRoot, target.Path)
		if strings.TrimSpace(target.Path) == "" || strings.TrimSpace(target.Owner) == "" || strings.TrimSpace(target.Kind) == "" {
			row.Error, row.Escalate = "foreign telemetry requires exact path, owner, and kind", true
			out = append(out, row)
			continue
		}
		usage, err := g.Measure.Measure(target.Path, g.Policy.MaxScanEntries)
		if err != nil {
			row.Error, row.Escalate = err.Error(), true
		} else {
			row.AllocatedBytes, row.Entries, row.Truncated = usage.Bytes, usage.Entries, usage.Truncated
			row.Escalate = usage.Truncated || (target.AlertBytes > 0 && usage.Bytes >= target.AlertBytes)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// AcquireDispatch serializes a disk-growing dispatch against reaping, attempts
// the configured safe reaper when pressure is observed, then decides from a
// fresh host-local statfs readback. The returned permit must remain held until
// worktree creation finishes.
func (g *Governor) AcquireDispatch(ctx context.Context) (permit io.Closer, report GovernorReport, err error) {
	if g == nil {
		return nil, report, errors.New("resource governor unavailable")
	}
	g.defaults()
	if err := g.Policy.validate(); err != nil {
		return nil, report, err
	}
	lock, err := g.Locks.Acquire(ctx, g.Policy.LockPath, g.Policy.LockTimeout, g.Policy.LockRetry)
	if err != nil {
		return nil, report, fmt.Errorf("resource governor dispatch lock: %w", err)
	}
	report, err = g.census(ctx)
	if err != nil {
		_ = lock.Close()
		return nil, report, err
	}
	reserve := report.EstimatedTaskReserveBytes
	if reserve == 0 {
		reserve = g.Policy.TaskReserveBytes
	}
	if g.Policy.PressureBytes > math.MaxUint64-reserve || g.Policy.RecoveryBytes > math.MaxUint64-reserve {
		_ = lock.Close()
		return nil, report, errors.New("estimated task reserve overflows host watermarks")
	}
	underPressure := report.CapacityBefore.FreeBytes < g.Policy.PressureBytes+reserve
	if underPressure {
		report, err = g.runLocked(ctx, RunOptions{Apply: g.Policy.ApplyBeforeDispatch, BatchLimit: g.Policy.ReapBatchLimit})
		if err != nil {
			_ = lock.Close()
			return nil, report, err
		}
	} else {
		fresh, freshErr := g.Capacity.StatFS(g.Policy.RepositoryRoot)
		if freshErr != nil {
			_ = lock.Close()
			return nil, report, fmt.Errorf("fresh dispatch statfs: %w", freshErr)
		}
		if validErr := validCapacity(fresh); validErr != nil {
			_ = lock.Close()
			return nil, report, fmt.Errorf("fresh dispatch statfs: %w", validErr)
		}
		if safeDiskIdentity(fresh.FilesystemID) != safeDiskIdentity(report.CapacityBefore.FilesystemID) {
			_ = lock.Close()
			return nil, report, errors.New("fresh dispatch statfs changed filesystem identity")
		}
		report.CapacityAfter = fresh
		g.setConcurrency(&report)
	}
	required := g.Policy.PressureBytes + reserve
	if underPressure {
		reserve = report.EstimatedTaskReserveBytes
		if reserve == 0 {
			reserve = g.Policy.TaskReserveBytes
		}
		if g.Policy.RecoveryBytes > math.MaxUint64-reserve {
			_ = lock.Close()
			return nil, report, errors.New("post-reap estimated task reserve overflows recovery watermark")
		}
		required = g.Policy.RecoveryBytes + reserve
	}
	if report.CapacityAfter.FreeBytes < required {
		_ = lock.Close()
		return nil, report, fmt.Errorf("host-local capacity blocked: free=%d required=%d host=%s", report.CapacityAfter.FreeBytes, required, g.Policy.HostID)
	}
	if report.AvailableDispatchConcurrency <= 0 {
		_ = lock.Close()
		return nil, report, fmt.Errorf("host-local dispatch concurrency exhausted: ceiling=%d host=%s", report.CapacityAwareConcurrency, g.Policy.HostID)
	}
	return lock, report, nil
}

func (r GovernorReport) JSON() string {
	data, err := json.Marshal(r)
	if err != nil {
		return `{"error":"resource governor report unavailable"}`
	}
	return string(data)
}
