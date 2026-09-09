package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	Path                  string       `json:"path"`
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
	HostID                 string
	RepositoryRoot         string
	BaseRef                string
	LockPath               string
	GeneratedDirectories   []string
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
	Path         string         `json:"path"`
	WorktreePath string         `json:"worktree_path"`
	RelativePath string         `json:"relative_path"`
	Decision     TargetDecision `json:"decision"`
	Reason       string         `json:"reason,omitempty"`
	BeforeBytes  uint64         `json:"before_bytes"`
	AfterBytes   uint64         `json:"after_bytes"`
	LastUse      time.Time      `json:"last_use"`
}

type ForeignTarget struct {
	Path       string `json:"path"`
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
	Targets                      []TargetReport       `json:"targets"`
	Foreign                      []ForeignTelemetry   `json:"foreign,omitempty"`
	Reaped                       int                  `json:"reaped"`
	ReclaimedBytes               uint64               `json:"reclaimed_bytes"`
}

type RunOptions struct {
	Apply          bool
	BatchLimit     int
	ForeignTargets []ForeignTarget
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
		HostID: g.Policy.HostID, ObservedAt: g.Now().UTC(), PressureBytes: g.Policy.PressureBytes,
		RecoveryBytes: g.Policy.RecoveryBytes, TaskReserveBytes: g.Policy.TaskReserveBytes,
		CapacityBefore: before, CapacityAfter: before, Worktrees: lanes,
	}
	for _, lane := range lanes {
		for _, rel := range g.Policy.GeneratedDirectories {
			report.Targets = append(report.Targets, g.inspectTarget(ctx, lane, rel))
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
	target.Decision = TargetWouldReap
	return target
}

func laneBlockReason(lane RegisteredWorktree) string {
	switch {
	case lane.PreserveReason != "":
		return lane.PreserveReason
	case lane.Category == LaneCurrent:
		return "canonical_checkout"
	case lane.Category == LaneReviewPool || lane.Category == LaneReviewSurface:
		if !lane.ReviewHandoffAdmitted {
			return "review_candidate_immutable"
		}
	case lane.FailedCandidate:
		return "failed_candidate_immutable"
	case lane.Unmerged && !((lane.Category == LaneReviewPool || lane.Category == LaneReviewSurface) && lane.ReviewHandoffAdmitted):
		return "unmerged_candidate"
	case lane.Dirty:
		return "dirty_source"
	case lane.Untracked:
		return "untracked_source"
	case lane.ActiveLease:
		return "active_lease"
	case lane.Held || lane.State == LaneHeld:
		return "held_lane"
	case lane.ActiveCWD:
		return "active_process_cwd"
	case lane.OpenFile:
		return "active_process_open_file"
	case lane.State == LaneActive:
		return "active_lane"
	case lane.State == LaneUnknown:
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
		if err := g.RemoveTree(again.Path); err != nil {
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
		if slots >= uint64(g.Policy.MaxDispatchConcurrency) {
			ceiling = g.Policy.MaxDispatchConcurrency
		} else {
			ceiling = int(slots)
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
