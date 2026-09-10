package resources

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type staticWorktrees struct {
	mu    sync.Mutex
	lanes []RegisteredWorktree
	err   error
}

func (s *staticWorktrees) List(context.Context, string, string) ([]RegisteredWorktree, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RegisteredWorktree(nil), s.lanes...), s.err
}

type capacitySequence struct {
	mu     sync.Mutex
	values []Capacity
	index  int
}

type staticSourceInspector struct {
	tracked bool
	err     error
}

func (s staticSourceInspector) HasTrackedSource(context.Context, string, string) (bool, error) {
	return s.tracked, s.err
}

func (s *capacitySequence) StatFS(string) (Capacity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.values) == 0 {
		return Capacity{}, errors.New("no capacity")
	}
	idx := s.index
	if idx >= len(s.values) {
		idx = len(s.values) - 1
	}
	s.index++
	return s.values[idx], nil
}

type channelLocks struct {
	once sync.Once
	ch   chan struct{}
}

func (l *channelLocks) Acquire(ctx context.Context, _ string, _, _ time.Duration) (io.Closer, error) {
	l.once.Do(func() {
		l.ch = make(chan struct{}, 1)
		l.ch <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ch:
		return &lockCloser{release: func() { l.ch <- struct{}{} }}, nil
	}
}

type lockCloser struct {
	once    sync.Once
	release func()
}

func (c *lockCloser) Close() error {
	c.once.Do(c.release)
	return nil
}

func capacity(free uint64, id string) Capacity {
	return Capacity{FilesystemID: id, TotalBytes: 1 << 40, FreeBytes: free, TotalInodes: 1 << 20, FreeInodes: 1 << 19}
}

func governorFor(t *testing.T, host string, free ...uint64) (*Governor, string, *staticWorktrees) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "node_modules")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "artifact"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	lanes := &staticWorktrees{lanes: []RegisteredWorktree{{
		Path: root, Branch: "herd/fac-test", Head: strings.Repeat("a", 40), Category: LaneTask,
		State: LaneDone, LastUse: time.Unix(100, 0).UTC(),
	}}}
	values := make([]Capacity, 0, len(free))
	for _, n := range free {
		values = append(values, capacity(n, host+"-fs"))
	}
	if len(values) == 0 {
		values = append(values, capacity(900000, host+"-fs"))
	}
	g := &Governor{
		Policy: GovernorPolicy{
			HostID: host, RepositoryRoot: root, BaseRef: "origin/main", LockPath: filepath.Join(root, "governor.lock"),
			GeneratedDirectories: []string{"node_modules"}, PressureBytes: 1000, RecoveryBytes: 2000,
			TaskReserveBytes: 1000, MaxDispatchConcurrency: 4, ReapBatchLimit: 2, MaxScanEntries: 1000,
			LockTimeout: time.Second, LockRetry: time.Millisecond, AllowApply: true,
		},
		Capacity: &capacitySequence{values: values}, Worktrees: lanes, Source: staticSourceInspector{},
		Measure: OSPhysicalMeasurer{}, Locks: &channelLocks{}, Now: func() time.Time { return time.Unix(200, 0).UTC() },
	}
	return g, target, lanes
}

func TestGovernorNeverReapsTrackedSource(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000, 900000)
	g.Source = staticSourceInspector{tracked: true}
	report, err := g.Run(context.Background(), RunOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Reaped != 0 || report.Targets[0].Decision != TargetBlocked || report.Targets[0].Reason != "tracked_source" {
		t.Fatalf("tracked source target=%+v", report.Targets[0])
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("tracked source was mutated: %v", err)
	}
}

func TestGovernorCensusReportsUnregisteredLaneWithoutMutation(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000, 900000)
	orphanRoot := filepath.Join(g.Policy.RepositoryRoot, ".herd", "worktrees")
	orphan := filepath.Join(orphanRoot, "fac-683")
	cache := filepath.Join(orphan, ".herd", "bootstrap", "cache", strings.Repeat("a", 64))
	if err := os.MkdirAll(filepath.Join(cache, "go-mod"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "graph.db"), []byte("graph"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, ".herd", "bootstrap", "receipt.json"), []byte("receipt"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.Policy.OrphanRoots = []string{orphanRoot}
	g.Policy.OrphanDerivedTargets = []string{"graph.db", "bootstrap-go-mod"}
	g.Policy.OrphanCacheTTL = time.Hour
	g.Policy.OrphanCacheBudgetBytes = 1 << 20
	report, err := g.Run(context.Background(), RunOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Orphans) != 1 || report.Orphans[0].AllocatedBytes == 0 {
		t.Fatalf("orphan census=%+v", report.Orphans)
	}
	if report.Orphans[0].PreserveReason != "unregistered_worktree_authority_unavailable" || len(report.Orphans[0].DerivedTargets) != 2 {
		t.Fatalf("orphan authority=%+v", report.Orphans[0])
	}
	for _, target := range report.Orphans[0].DerivedTargets {
		if target.Decision != "blocked" || target.Reason == "" {
			t.Fatalf("orphan derived target=%+v", target)
		}
	}
	if _, err := os.Stat(filepath.Join(orphan, "graph.db")); err != nil {
		t.Fatalf("orphan source/derived evidence mutated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(orphan, ".herd", "bootstrap", "receipt.json")); err != nil {
		t.Fatalf("orphan bootstrap receipt mutated: %v", err)
	}
}

func TestGovernorReclaimsProofBackedOrphanCachesAfterTTL(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000, 900000)
	g.Processes = idleProcessInspector{}
	g.Policy.OrphanRoots = []string{filepath.Join(g.Policy.RepositoryRoot, ".herd", "worktrees")}
	g.Policy.OrphanDerivedTargets = []string{"graph.db", "bootstrap-go-mod"}
	g.Policy.OrphanCacheTTL = time.Hour
	g.Policy.OrphanCacheBudgetBytes = 1 << 20
	g.Policy.GeneratedDirectories = []string{"unused-generated-cache"}
	orphan := filepath.Join(g.Policy.OrphanRoots[0], "fac-683")
	digest := strings.Repeat("b", 64)
	cache := filepath.Join(orphan, ".herd", "bootstrap", "cache", digest, "go-mod")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	graph := filepath.Join(orphan, "graph.db")
	if err := os.WriteFile(graph, []byte("graph-index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "module.zip"), []byte("module-cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt := `{"version":1,"contract_digest":"contract","toolchain_digest":"` + digest + `","cache_dir":".herd/bootstrap/cache/` + digest + `"}`
	if err := os.MkdirAll(filepath.Join(orphan, ".herd", "bootstrap"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, ".herd", "bootstrap", "receipt.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(-10000, 0)
	if err := os.Chtimes(graph, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cache, old, old); err != nil {
		t.Fatal(err)
	}
	report, err := g.Run(context.Background(), RunOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Reaped != 2 || report.ReclaimedBytes == 0 || len(report.OrphanTargets) != 2 {
		t.Fatalf("proof-backed orphan cleanup=%+v", report)
	}
	if _, err := os.Stat(graph); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("graph cache was not reaped: %v", err)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go-mod cache was not reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(orphan, ".herd", "bootstrap", "receipt.json")); err != nil {
		t.Fatalf("bootstrap receipt was removed: %v", err)
	}
}

func TestGovernorRefusesOrphanCacheOnOwnershipOrOpenFileUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		owner  func(os.FileInfo) (string, bool)
		usage  ProcessUsage
	}{
		{name: "foreign ownership", reason: "derived_target_foreign_uid", owner: func(os.FileInfo) (string, bool) { return "foreign", true }},
		{name: "open file", reason: "derived_target_active_process", owner: func(os.FileInfo) (string, bool) { return strconv.Itoa(os.Getuid()), true }, usage: ProcessUsage{OpenFile: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _ := governorFor(t, "host", 900000, 900000)
			g.Processes = staticProcessInspector{usage: tc.usage}
			g.OwnerID = tc.owner
			g.Policy.GeneratedDirectories = []string{"unused-generated-cache"}
			g.Policy.OrphanRoots = []string{filepath.Join(g.Policy.RepositoryRoot, ".herd", "worktrees")}
			g.Policy.OrphanDerivedTargets = []string{"graph.db"}
			g.Policy.OrphanCacheTTL, g.Policy.OrphanCacheBudgetBytes = time.Hour, 1<<20
			orphan := filepath.Join(g.Policy.OrphanRoots[0], "fac-683")
			if err := os.MkdirAll(orphan, 0o700); err != nil {
				t.Fatal(err)
			}
			graph := filepath.Join(orphan, "graph.db")
			if err := os.WriteFile(graph, []byte("immutable"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Unix(-10000, 0)
			if err := os.Chtimes(graph, old, old); err != nil {
				t.Fatal(err)
			}
			report, err := g.Run(context.Background(), RunOptions{Apply: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Orphans) != 1 || report.Orphans[0].DerivedTargets[0].Reason != tc.reason {
				t.Fatalf("refusal report=%+v want=%s", report.Orphans, tc.reason)
			}
			if _, err := os.Stat(graph); err != nil {
				t.Fatalf("protected orphan cache mutated: %v", err)
			}
		})
	}
}

type staticProcessInspector struct{ usage ProcessUsage }

func (s staticProcessInspector) InUse(context.Context, string) (ProcessUsage, error) {
	return s.usage, nil
}

func TestGovernorSafetyReasonsCoverDestructiveAuthority(t *testing.T) {
	tests := []struct {
		name string
		edit func(*RegisteredWorktree)
		want string
	}{
		{name: "cwd", edit: func(l *RegisteredWorktree) { l.ActiveCWD = true }, want: "active_process_cwd"},
		{name: "open file", edit: func(l *RegisteredWorktree) { l.OpenFile = true }, want: "active_process_open_file"},
		{name: "dirty", edit: func(l *RegisteredWorktree) { l.Dirty = true }, want: "dirty_source"},
		{name: "untracked", edit: func(l *RegisteredWorktree) { l.Untracked = true }, want: "untracked_source"},
		{name: "lease", edit: func(l *RegisteredWorktree) { l.ActiveLease = true }, want: "active_lease"},
		{name: "held", edit: func(l *RegisteredWorktree) { l.Held = true }, want: "held_lane"},
		{name: "failed", edit: func(l *RegisteredWorktree) { l.FailedCandidate = true }, want: "failed_candidate_immutable"},
		{name: "unmerged", edit: func(l *RegisteredWorktree) { l.Unmerged = true }, want: "unmerged_candidate"},
		{name: "review", edit: func(l *RegisteredWorktree) { l.Category = LaneReviewSurface }, want: "review_candidate_immutable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, _, lanes := governorFor(t, "host", 900000, 900000)
			tc.edit(&lanes.lanes[0])
			report, err := g.Run(context.Background(), RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Targets) != 1 || report.Targets[0].Decision != TargetBlocked || report.Targets[0].Reason != tc.want {
				t.Fatalf("target=%+v, want blocked reason %q", report.Targets, tc.want)
			}
		})
	}
}

func TestGovernorRejectsCanonicalPolicyAndSymlinkEscape(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000)
	g.Policy.GeneratedDirectories = []string{".herd/receipts"}
	if _, err := g.Run(context.Background(), RunOptions{}); err == nil || !strings.Contains(err.Error(), ".herd") {
		t.Fatalf("canonical evidence policy err=%v", err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	g.Policy.GeneratedDirectories = []string{"node_modules"}
	report, err := g.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Targets[0].Reason != "target_symlink" || report.Targets[0].Decision != TargetBlocked {
		t.Fatalf("symlink target was not blocked: %+v", report.Targets[0])
	}
}

func TestGovernorPreservesNestedCanonicalState(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000, 900000)
	if err := os.Mkdir(filepath.Join(target, ".herd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".herd", "receipt.json"), []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := g.Run(context.Background(), RunOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Targets[0].Decision != TargetBlocked || report.Targets[0].Reason != "nested_canonical_state" {
		t.Fatalf("nested canonical state target=%+v", report.Targets[0])
	}
	if _, err := os.Stat(filepath.Join(target, ".herd", "receipt.json")); err != nil {
		t.Fatalf("canonical evidence changed: %v", err)
	}
}

func TestGovernorRejectsOverlappingTargetsWithoutMutation(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000)
	g.Policy.GeneratedDirectories = []string{"node_modules", "node_modules/cache"}
	if _, err := g.Run(context.Background(), RunOptions{Apply: true}); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlap err=%v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("overlap validation mutated target: %v", err)
	}
}

func TestReviewHydrationRequiresAdmittedHandoff(t *testing.T) {
	g, _, lanes := governorFor(t, "host", 900000, 900000, 900000, 900000)
	lanes.lanes[0].Category = LaneReviewSurface
	lanes.lanes[0].Unmerged = true
	blocked, err := g.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Targets[0].Reason != "review_candidate_immutable" {
		t.Fatalf("unadmitted review target=%+v", blocked.Targets[0])
	}
	lanes.lanes[0].ReviewHandoffAdmitted = true
	admitted, err := g.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Targets[0].Decision != TargetBlocked || admitted.Targets[0].Reason != "unmerged_candidate" {
		t.Fatalf("admitted review hydration=%+v", admitted.Targets[0])
	}
}

func TestAdmittedReviewStillHonorsEverySafetyGuard(t *testing.T) {
	guards := []struct {
		name string
		edit func(*RegisteredWorktree)
		want string
	}{
		{"failed", func(l *RegisteredWorktree) { l.FailedCandidate = true }, "failed_candidate_immutable"},
		{"unmerged", func(l *RegisteredWorktree) { l.Unmerged = true }, "unmerged_candidate"},
		{"dirty", func(l *RegisteredWorktree) { l.Dirty = true }, "dirty_source"},
		{"untracked", func(l *RegisteredWorktree) { l.Untracked = true }, "untracked_source"},
		{"lease", func(l *RegisteredWorktree) { l.ActiveLease = true }, "active_lease"},
		{"cwd", func(l *RegisteredWorktree) { l.ActiveCWD = true }, "active_process_cwd"},
		{"open file", func(l *RegisteredWorktree) { l.OpenFile = true }, "active_process_open_file"},
		{"held", func(l *RegisteredWorktree) { l.Held = true }, "held_lane"},
		{"unknown", func(l *RegisteredWorktree) { l.State = LaneUnknown }, "lane_state_unknown"},
	}
	for _, guard := range guards {
		t.Run(guard.name, func(t *testing.T) {
			g, target, lanes := governorFor(t, "host", 900000, 900000)
			lanes.lanes[0].Category = LaneReviewSurface
			lanes.lanes[0].ReviewHandoffAdmitted = true
			guard.edit(&lanes.lanes[0])
			report, err := g.Run(context.Background(), RunOptions{Apply: true})
			if err != nil {
				t.Fatal(err)
			}
			if report.Targets[0].Decision != TargetBlocked || report.Targets[0].Reason != guard.want {
				t.Fatalf("target=%+v want=%s", report.Targets[0], guard.want)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("guard allowed mutation: %v", err)
			}
		})
	}
}

func TestGovernorApplyIsBoundedIdempotentAndReadsPhysicalBytes(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000, 850000, 840000, 830000)
	report, err := g.Run(context.Background(), RunOptions{Apply: true, BatchLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Reaped != 1 || report.Targets[0].Decision != TargetReaped || report.Targets[0].BeforeBytes == 0 || report.Targets[0].AfterBytes != 0 {
		t.Fatalf("first apply=%+v", report)
	}
	if report.CapacityBefore.FreeBytes != 900000 || report.CapacityAfter.FreeBytes != 850000 {
		t.Fatalf("statfs readback=%d->%d", report.CapacityBefore.FreeBytes, report.CapacityAfter.FreeBytes)
	}
	if report.EstimatedTaskReserveBytes < report.TaskReserveBytes || report.EstimatedTaskReserveBytes < report.Targets[0].BeforeBytes {
		t.Fatalf("task reserve was not estimated from physical data: %+v", report)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact target still exists: %v", err)
	}
	again, err := g.Run(context.Background(), RunOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Reaped != 0 || again.Targets[0].Decision != TargetAbsent {
		t.Fatalf("second apply not idempotent: %+v", again)
	}
}

func TestLifecycleApplyRequiresBothPolicySwitches(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000)
	if g.LifecycleApply() {
		t.Fatal("observe-by-default governor admitted lifecycle mutation")
	}
	g.Policy.AllowApply = true
	if g.LifecycleApply() {
		t.Fatal("AllowApply alone admitted lifecycle mutation")
	}
	g.Policy.ApplyBeforeDispatch = true
	if !g.LifecycleApply() {
		t.Fatal("enabled lifecycle policy did not admit apply")
	}
}

func TestSafeRemoveGeneratedTreeDefaultAndRollbackFailures(t *testing.T) {
	t.Run("default remover", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "generated")
		if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "nested", "data"), []byte("keep no longer"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := safeRemoveGeneratedTree(root, target, os.RemoveAll); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target remains: %v", err)
		}
	})

	t.Run("remove failure rolls back", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "generated")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		want := errors.New("injected remover failure")
		if err := safeRemoveGeneratedTree(root, target, func(string) error { return want }); !errors.Is(err, want) {
			t.Fatalf("error=%v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("rollback lost target: %v", err)
		}
	})

	t.Run("rollback failure records durable evidence", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "generated")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		removeErr := errors.New("injected remover failure")
		err := safeRemoveGeneratedTree(root, target, func(quarantine string) error {
			if removeErr := os.RemoveAll(quarantine); removeErr != nil {
				t.Fatal(removeErr)
			}
			return removeErr
		})
		if err == nil || !strings.Contains(err.Error(), "recovery record") {
			t.Fatalf("error=%v", err)
		}
		data, readErr := os.ReadFile(filepath.Join(root, ".herd", "resource-reap-recovery.jsonl"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(data), `"target":"./generated"`) {
			t.Fatalf("recovery record=%q", data)
		}
	})
}

func TestGovernorMixedCensusAndCrossMachineIndependence(t *testing.T) {
	gA, _, lanesA := governorFor(t, "host-a", 700000, 700000)
	rootA := gA.Policy.RepositoryRoot
	lanesA.lanes = []RegisteredWorktree{
		{Path: rootA, Head: "a", Category: LaneNew, State: LaneIdle},
		{Path: t.TempDir(), Head: "b", Category: LaneLegacyStanding, State: LaneHeld, Held: true},
		{Path: t.TempDir(), Head: "c", Category: LaneCurrent, State: LaneActive},
		{Path: t.TempDir(), Head: "d", Category: LaneTask, State: LaneDone},
		{Path: t.TempDir(), Head: "e", Category: LaneReviewPool, State: LaneIdle},
		{Path: t.TempDir(), Head: "f", Category: LaneReviewSurface, State: LaneIdle},
		{Path: t.TempDir(), Head: "g", Category: LaneHarvest, State: LaneActive},
	}
	for _, lane := range lanesA.lanes {
		if err := os.MkdirAll(filepath.Join(lane.Path, "node_modules"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gB, _, _ := governorFor(t, "host-b", 2500, 2500)
	reportA, err := gA.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reportB, err := gB.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reportA.Worktrees) != 7 {
		t.Fatalf("mixed census lost lanes: %d", len(reportA.Worktrees))
	}
	if reportA.HostID == reportB.HostID || reportA.CapacityAwareConcurrency <= reportB.CapacityAwareConcurrency {
		t.Fatalf("host capacity leaked: A=%+v B=%+v", reportA, reportB)
	}
}

func TestGovernorSerializesReapAndDispatch(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000, 900000, 900000, 900000, 900000)
	entered := make(chan struct{})
	release := make(chan struct{})
	g.RemoveTree = func(path string) error {
		close(entered)
		<-release
		return os.RemoveAll(path)
	}
	reapDone := make(chan error, 1)
	go func() {
		_, err := g.Run(context.Background(), RunOptions{Apply: true})
		reapDone <- err
	}()
	<-entered
	dispatchDone := make(chan error, 1)
	go func() {
		permit, _, err := g.AcquireDispatch(context.Background())
		if permit != nil {
			err = errors.Join(err, permit.Close())
		}
		dispatchDone <- err
	}()
	select {
	case err := <-dispatchDone:
		t.Fatalf("dispatch escaped reap lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-reapDone; err != nil {
		t.Fatal(err)
	}
	if err := <-dispatchDone; err != nil {
		t.Fatal(err)
	}
}

func TestAcquireDispatchAttemptsReaperThenUsesFreshHostReadback(t *testing.T) {
	g, _, _ := governorFor(t, "host", 1500, 1500, 100000)
	g.Policy.ApplyBeforeDispatch = true
	permit, report, err := g.AcquireDispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()
	if report.Mode != "apply" || report.Reaped != 1 || report.CapacityAfter.FreeBytes != 100000 {
		t.Fatalf("dispatch recovery report=%+v", report)
	}
}

func TestGovernorFailsClosedWhenFreshReadbackChangesFilesystem(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000, 900000)
	g.Capacity = &capacitySequence{values: []Capacity{capacity(900000, "fs-a"), capacity(900000, "fs-b")}}
	if _, err := g.Run(context.Background(), RunOptions{}); err == nil || !strings.Contains(err.Error(), "filesystem identity") {
		t.Fatalf("filesystem drift err=%v", err)
	}
}

func TestForeignTelemetryIsObserveOnlyAndOwnershipExplicit(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000, 900000)
	foreign := filepath.Join(t.TempDir(), "runtime.sqlite")
	if err := os.WriteFile(foreign, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	report, err := g.Run(context.Background(), RunOptions{ForeignTargets: []ForeignTarget{{Path: foreign, Owner: "codex", Kind: "history_sqlite", AlertBytes: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(report.Foreign) != 1 || !report.Foreign[0].Escalate || report.Foreign[0].Action != "observe_only_contact_owner" {
		t.Fatalf("foreign state or telemetry changed: %+v", report.Foreign)
	}
}

func TestActiveTaskReceipt(t *testing.T) {
	root := t.TempDir()
	receipt := map[string]any{"lease_id": "claim:1", "lease_generation": 2, "session_id": "worker-1", "expires_at": time.Now().Add(time.Hour).UTC()}
	data, _ := json.Marshal(receipt)
	if err := os.WriteFile(filepath.Join(root, "TASK-CONTEXT.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := activeTaskReceipt(root, time.Now())
	if err != nil || !active {
		t.Fatalf("active receipt=%t err=%v", active, err)
	}
}

type idleProcessInspector struct{}

func (idleProcessInspector) InUse(context.Context, string) (ProcessUsage, error) {
	return ProcessUsage{}, nil
}

func TestGitWorktreeEnumeratorDetectsDirtyAndUntrackedSource(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	enumerator := GitWorktreeEnumerator{Processes: idleProcessInspector{}, Now: time.Now}
	lanes, err := enumerator.List(context.Background(), root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || !lanes[0].Dirty || !lanes[0].Untracked {
		t.Fatalf("git safety evidence=%+v", lanes)
	}
}

func TestLSOFProcessInspectorDetectsCWDAndOpenFile(t *testing.T) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		t.Skipf("lsof unavailable: %v", err)
	}
	root := t.TempDir()
	file := filepath.Join(root, "open")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `cd "$1" && exec 3<open && sleep 5`, "sh", root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	inspector := LSOFProcessInspector{Executable: lsof, Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20}
	deadline := time.Now().Add(2 * time.Second)
	for {
		usage, probeErr := inspector.InUse(context.Background(), root)
		if probeErr == nil && usage.OpenFile && usage.CWD {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open file process not detected: usage=%+v err=%v", usage, probeErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestParseWorktreePorcelainHeldLane(t *testing.T) {
	data := []byte("worktree /repo/.worktrees/lane\nHEAD deadbeef\nbranch refs/heads/lane\nlocked operator\n\n")
	lanes, err := parseWorktreePorcelain(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || !lanes[0].Held || lanes[0].State != LaneHeld {
		t.Fatalf("held lane parse=%+v", lanes)
	}
}

func TestListRegisteredWorktreesUsesPortablePorcelainCommand(t *testing.T) {
	var gotRoot string
	var gotArgs []string
	run := func(_ context.Context, root string, args ...string) ([]byte, error) {
		gotRoot = root
		gotArgs = append([]string(nil), args...)
		return []byte("worktree /repo/lane with spaces\nHEAD deadbeef\nbranch refs/heads/lane\n\n"), nil
	}
	lanes, err := listRegisteredWorktrees(context.Background(), "/repo", run)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"worktree", "list", "--porcelain"}
	if gotRoot != "/repo" || strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("worktree list invocation root=%q args=%q, want root=/repo args=%q", gotRoot, gotArgs, wantArgs)
	}
	if len(lanes) != 1 || lanes[0].Path != "/repo/lane with spaces" || lanes[0].Head != "deadbeef" || lanes[0].Branch != "lane" {
		t.Fatalf("portable porcelain parse=%+v", lanes)
	}
}

func TestParseWorktreePorcelainDecodesGitQuotedPath(t *testing.T) {
	data := []byte("worktree \"/repo/lane\\nname\"\nHEAD deadbeef\nbranch refs/heads/lane\n\n")
	lanes, err := parseWorktreePorcelain(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || lanes[0].Path != "/repo/lane\nname" {
		t.Fatalf("quoted worktree path=%+v", lanes)
	}
}
