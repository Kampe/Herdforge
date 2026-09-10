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
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
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
	if len(p.GeneratedDirectories) == 0 {
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
		if target != "graph.db" && target != "bootstrap-go-mod" {
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
	Now        func() time.Time
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
// act. Observe is the safe default; both switches are required before a
// daemon-triggered sweep can mutate generated data.
func (g *Governor) LifecycleApply() bool {
	return g != nil && g.Policy.AllowApply && g.Policy.ApplyBeforeDispatch
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
	return g.runLocked(ctx, options)
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

func (g *Governor) census(ctx context.Context) (GovernorReport, error) {
	before, err := g.Capacity.StatFS(g.Policy.RepositoryRoot)
	if err != nil {
		return GovernorReport{}, fmt.Errorf("resource governor statfs: %w", err)
	}
	if err := validCapacity(before); err != nil {
		return GovernorReport{}, fmt.Errorf("resource governor statfs: %w", err)
	}
	lanes, err := g.Worktrees.List(ctx, g.Policy.RepositoryRoot, g.Policy.BaseRef)
	if err != nil {
		return GovernorReport{}, fmt.Errorf("resource governor registered-worktree census: %w", err)
	}
	seen := make(map[string]struct{}, len(lanes))
	for i := range lanes {
		resolved, resolveErr := filepath.EvalSymlinks(lanes[i].Path)
		if resolveErr != nil {
			lanes[i].State = LaneUnknown
			lanes[i].PreserveReason = "worktree_realpath_unavailable"
			continue
		}
		lanes[i].Path = filepath.Clean(resolved)
		if _, exists := seen[lanes[i].Path]; exists {
			return GovernorReport{}, fmt.Errorf("duplicate registered worktree realpath %q", lanes[i].Path)
		}
		seen[lanes[i].Path] = struct{}{}
		usage, measureErr := g.Measure.Measure(lanes[i].Path, g.Policy.MaxScanEntries)
		if measureErr != nil {
			lanes[i].State = LaneUnknown
			lanes[i].PreserveReason = "worktree_allocation_unavailable"
			continue
		}
		lanes[i].AllocatedBytes = usage.Bytes
		lanes[i].AllocationTruncated = usage.Truncated
		if usage.Truncated && lanes[i].PreserveReason == "" {
			lanes[i].PreserveReason = "worktree_allocation_truncated"
		}
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Path < lanes[j].Path })
	report := GovernorReport{
		HostID: g.Policy.HostID, RepositoryID: g.Policy.RepositoryID, ObservedAt: g.Now().UTC(), PressureBytes: g.Policy.PressureBytes,
		RecoveryBytes: g.Policy.RecoveryBytes, TaskReserveBytes: g.Policy.TaskReserveBytes,
		CapacityBefore: before, CapacityAfter: before, Worktrees: lanes,
	}
	for i := range report.Worktrees {
		report.Worktrees[i].ReportPath = reportPath(g.Policy.RepositoryRoot, report.Worktrees[i].Path)
	}
	orphans, orphanErr := g.censusOrphans(ctx, report.Worktrees, before)
	if orphanErr != nil {
		return GovernorReport{}, fmt.Errorf("resource governor unregistered-orphan census: %w", orphanErr)
	}
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
	for _, lane := range lanes {
		for _, rel := range g.Policy.GeneratedDirectories {
			target := g.inspectTarget(ctx, lane, rel)
			target.ReportPath = reportPath(g.Policy.RepositoryRoot, target.Path)
			target.ReportWorktreePath = reportPath(g.Policy.RepositoryRoot, target.WorktreePath)
			report.Targets = append(report.Targets, target)
		}
	}
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

func (g *Governor) censusOrphans(ctx context.Context, registered []RegisteredWorktree, before Capacity) ([]OrphanWorktree, error) {
	if len(g.Policy.OrphanRoots) == 0 {
		return nil, nil
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
		return nil, fmt.Errorf("resolve repository root for orphan census: %w", err)
	}
	var out []OrphanWorktree
	seenRoots := make(map[string]struct{}, len(g.Policy.OrphanRoots))
	for _, rawRoot := range g.Policy.OrphanRoots {
		root, err := filepath.EvalSymlinks(rawRoot)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolve known orphan root: %w", err)
		}
		root = filepath.Clean(root)
		if !containedPath(repoRoot, root) {
			return nil, errors.New("known orphan root escapes repository root")
		}
		if _, duplicate := seenRoots[root]; duplicate {
			continue
		}
		seenRoots[root] = struct{}{}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("read known orphan root: %w", err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
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
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				continue
			}
			usage, measureErr := g.Measure.Measure(path, g.Policy.MaxScanEntries)
			if measureErr != nil {
				out = append(out, OrphanWorktree{Path: path, ReportPath: reportPath(g.Policy.RepositoryRoot, path), PreserveReason: "orphan_allocation_unavailable"})
				continue
			}
			orphan := OrphanWorktree{Path: path, ReportPath: reportPath(g.Policy.RepositoryRoot, path), AllocatedBytes: usage.Bytes, Entries: usage.Entries, AllocationTruncated: usage.Truncated, PreserveReason: "unregistered_worktree_authority_unavailable"}
			for _, rel := range g.Policy.OrphanDerivedTargets {
				target, targetRel, resolveErr := orphanTargetPath(path, rel)
				row := OrphanDerivedTarget{Path: target, RelativePath: targetRel, ReportPath: reportPath(g.Policy.RepositoryRoot, target), Decision: "blocked", Reason: "orphan_derived_target_evidence_unavailable"}
				if resolveErr != nil {
					row.Reason = resolveErr.Error()
					orphan.DerivedTargets = append(orphan.DerivedTargets, row)
					continue
				}
				usage, eligible, reason := g.orphanTargetProof(ctx, path, target, rel, before)
				row.AllocatedBytes, row.Decision, row.Reason = usage.Bytes, "blocked", reason
				if eligible {
					row.Decision = string(TargetWouldReap)
				}
				orphan.DerivedTargets = append(orphan.DerivedTargets, row)
			}
			out = append(out, orphan)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
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
	case "bootstrap-go-mod":
		data, err := os.ReadFile(filepath.Join(orphan, ".herd", "bootstrap", "receipt.json"))
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
		return filepath.Join(orphan, filepath.FromSlash(cache), "go-mod"), filepath.ToSlash(filepath.Join(cache, "go-mod")), nil
	default:
		return "", "", errors.New("orphan_derived_target_policy_invalid")
	}
}

func (g *Governor) orphanTargetProof(ctx context.Context, orphan, target, policyTarget string, before Capacity) (PhysicalUsage, bool, string) {
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
	if policyTarget == "bootstrap-go-mod" && !info.IsDir() {
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
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint32(stat.Uid) != uint32(os.Getuid()) {
		return PhysicalUsage{}, false, "derived_target_foreign_uid"
	}
	usage, err := g.Measure.Measure(target, g.Policy.MaxScanEntries)
	if err != nil || usage.Truncated {
		return usage, false, "derived_target_allocation_unavailable"
	}
	process, err := g.Processes.InUse(ctx, target)
	if err != nil {
		return usage, false, "derived_target_process_evidence_unavailable"
	}
	if process.CWD || process.OpenFile {
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
		}
		usage, eligible, reason := g.orphanTargetProof(ctx, orphan, report.OrphanTargets[i].Path, policyTarget, report.CapacityBefore)
		if !eligible {
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, reason
			continue
		}
		if used > g.Policy.OrphanCacheBudgetBytes-usage.Bytes {
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, "orphan_cache_budget_exhausted"
			continue
		}
		if err := safeRemoveGeneratedTree(g.Policy.RepositoryRoot, report.OrphanTargets[i].Path, g.RemoveTree); err != nil {
			report.OrphanTargets[i].Decision, report.OrphanTargets[i].Reason = TargetBlocked, "orphan_cache_remove_failed"
			return fmt.Errorf("remove exact orphan cache %q: %w", report.OrphanTargets[i].Path, err)
		}
		after, err := g.Measure.Measure(report.OrphanTargets[i].Path, g.Policy.MaxScanEntries)
		if errors.Is(err, os.ErrNotExist) {
			after = PhysicalUsage{}
			err = nil
		}
		if err != nil {
			return fmt.Errorf("orphan cache post-reap readback: %w", err)
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
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("stat generated target parent before removal: %w", err)
	}
	quarantine, err := os.MkdirTemp(parent, ".herd-resource-reap-")
	if err != nil {
		return fmt.Errorf("create removal quarantine: %w", err)
	}
	quarantined := filepath.Join(quarantine, filepath.Base(resolved))
	if err := os.Rename(resolved, quarantined); err != nil {
		return fmt.Errorf("quarantine generated target: %w", err)
	}
	currentParent, statErr := os.Stat(parent)
	if statErr != nil || !os.SameFile(parentInfo, currentParent) {
		if rollbackErr := os.Rename(quarantined, resolved); rollbackErr != nil {
			return fmt.Errorf("generated target parent changed and rollback failed: %v; quarantine retained at %s", statErr, quarantine)
		}
		if cleanupErr := os.Remove(quarantine); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return fmt.Errorf("generated target parent changed during removal; quarantine cleanup: %w", cleanupErr)
		}
		return errors.New("generated target parent changed during removal")
	}
	if err := remove(quarantine); err != nil {
		if rollbackErr := os.Rename(quarantined, resolved); rollbackErr != nil {
			recovery := filepath.Join(root, ".herd", "resource-reap-recovery.jsonl")
			recordErr := appendRecoveryRecord(recovery, root, resolved, quarantine, err)
			if recordErr != nil {
				return fmt.Errorf("remove quarantined generated target: %v; rollback failed: %v; recovery record failed: %v; quarantine retained at %s", err, rollbackErr, recordErr, quarantine)
			}
			return fmt.Errorf("remove quarantined generated target: %v; rollback failed: %v; recovery record=%s; quarantine retained at %s", err, rollbackErr, reportPath(root, recovery), quarantine)
		}
		if cleanupErr := os.Remove(quarantine); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return fmt.Errorf("remove quarantined generated target: %v; rollback cleanup failed: %w", err, cleanupErr)
		}
		return fmt.Errorf("remove quarantined generated target: %w", err)
	}
	if err := os.Remove(quarantine); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty quarantine: %w", err)
	}
	return nil
}

func appendRecoveryRecord(path, root, target, quarantine string, reason error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create recovery record directory: %w", err)
	}
	record, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open recovery record: %w", err)
	}
	_, writeErr := fmt.Fprintf(record, "{\"target\":%q,\"quarantine\":%q,\"reason\":%q}\n", reportPath(root, target), reportPath(root, quarantine), reason.Error())
	if writeErr == nil {
		writeErr = record.Sync()
	}
	closeErr := record.Close()
	return errors.Join(writeErr, closeErr)
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
