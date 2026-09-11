package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

var errCensusReleasedByTest = errors.New("census dependency released by test")

// lifecycleCapacityBackend satisfies the census's first stage without
// touching a live filesystem identity probe.
type lifecycleCapacityBackend struct{}

func (lifecycleCapacityBackend) StatFS(string) (resources.Capacity, error) {
	return resources.Capacity{
		FilesystemID: "lifecycle-test-fs", TotalBytes: 1 << 40, FreeBytes: 900000,
		TotalInodes: 1 << 20, FreeInodes: 1 << 19,
	}, nil
}

// lifecycleBlockingWorktrees is the cooperative blocking dependency: List
// records the deadline the sweep boundary handed it, proves the global
// governor lock is held by contending on it, and then blocks until the
// sweep context deadline or an explicit test release.
type lifecycleBlockingWorktrees struct {
	mu          sync.Mutex
	lockPath    string
	release     chan struct{}
	observedOK  bool
	observedDL  time.Time
	contentErr  error
	listEntered chan struct{}
	once        sync.Once
}

func newLifecycleBlockingWorktrees(lockPath string) *lifecycleBlockingWorktrees {
	return &lifecycleBlockingWorktrees{
		lockPath: lockPath, release: make(chan struct{}), listEntered: make(chan struct{}),
	}
}

func (w *lifecycleBlockingWorktrees) List(ctx context.Context, _, _ string) ([]resources.RegisteredWorktree, error) {
	// The sweep must hold the global governor lock while the census blocks:
	// a short contending acquire from inside the census has to fail. This
	// runs before listEntered so the observing test reads settled evidence.
	_, err := (resources.FileLockProvider{}).Acquire(context.Background(), w.lockPath, 50*time.Millisecond, 10*time.Millisecond)
	w.mu.Lock()
	w.observedDL, w.observedOK = ctx.Deadline()
	w.contentErr = err
	w.mu.Unlock()
	w.once.Do(func() { close(w.listEntered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.release:
		return nil, errCensusReleasedByTest
	}
}

func (w *lifecycleBlockingWorktrees) observation() (bool, time.Time, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.observedOK, w.observedDL, w.contentErr
}

func lifecycleTestPolicy(t *testing.T, allowApply bool) resources.GovernorPolicy {
	t.Helper()
	root := t.TempDir()
	return resources.GovernorPolicy{
		HostID: "lifecycle-test-host", RepositoryRoot: root,
		BaseRef: "origin/main", LockPath: filepath.Join(root, "governor.lock"),
		GeneratedDirectories:   []string{"node_modules"},
		PressureBytes:          1000, RecoveryBytes: 2000, TaskReserveBytes: 1000,
		MaxDispatchConcurrency: 4, ReapBatchLimit: 2, MaxScanEntries: 1000,
		LockTimeout: 2 * time.Second, LockRetry: time.Millisecond, AllowApply: allowApply,
	}
}

// TestLifecycleSweepGivesUnboundedCallerAFiniteDeadline proves the lifecycle
// boundary hands a cooperative blocking dependency a finite deadline when the
// caller passed context.Background(), propagates the dependency's failure
// fail-closed, and leaves the global governor lock released after return.
func TestLifecycleSweepGivesUnboundedCallerAFiniteDeadline(t *testing.T) {
	policy := lifecycleTestPolicy(t, false)
	blocker := newLifecycleBlockingWorktrees(policy.LockPath)
	governor := &resources.Governor{
		Policy: policy, Capacity: lifecycleCapacityBackend{},
		Worktrees: blocker, Locks: resources.FileLockProvider{},
	}
	done := make(chan error, 1)
	go func() {
		_, err := runLifecycleGovernorSweep(context.Background(), governor, resources.SweepReviewBeforeRefusal)
		done <- err
	}()
	select {
	case <-blocker.listEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("census dependency never observed the sweep context")
	}
	ok, observed, contentionErr := blocker.observation()
	if !ok {
		t.Fatal("lifecycle sweep handed the blocking dependency an unbounded context (no deadline)")
	}
	horizon := time.Until(observed)
	// The boundary hands the sweep the full 45s budget; the census then gives
	// its registered phase 4/5 of the remainder, so the dependency observes
	// roughly 36s here. Anything at or beyond the budget, gone entirely, or
	// collapsed to a fraction of the phase split is a boundary defect.
	if horizon <= 0 || horizon > lifecycleSweepBudget+time.Second || horizon < lifecycleSweepBudget/2 {
		t.Fatalf("lifecycle sweep deadline outside the %s operational budget: %v", lifecycleSweepBudget, horizon)
	}
	if contentionErr == nil {
		t.Fatal("global governor lock was not held during the blocked census; held-then-released evidence would be vacuous")
	}
	close(blocker.release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lifecycle sweep swallowed the blocking dependency's failure; must propagate fail-closed")
		}
		if !errors.Is(err, errCensusReleasedByTest) {
			t.Fatalf("sweep error lost the causal dependency failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sweep did not return after the blocking dependency stopped")
	}
	lock, err := (resources.FileLockProvider{}).Acquire(context.Background(), policy.LockPath, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("global governor lock not released after the sweep returned: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release test lock: %v", err)
	}
}

// TestLifecycleSweepPreservesEarlierCallerDeadline proves an already-shorter
// caller deadline survives the boundary verbatim and the dependency stops on
// it, instead of being silently stretched to the operational budget.
func TestLifecycleSweepPreservesEarlierCallerDeadline(t *testing.T) {
	policy := lifecycleTestPolicy(t, false)
	callerDeadline := time.Now().Add(300 * time.Millisecond)
	parent, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()
	blocker := newLifecycleBlockingWorktrees(policy.LockPath)
	governor := &resources.Governor{
		Policy: policy, Capacity: lifecycleCapacityBackend{},
		Worktrees: blocker, Locks: resources.FileLockProvider{},
	}
	_, err := runLifecycleGovernorSweep(parent, governor, resources.SweepReviewBeforeRefusal)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline must stop the sweep fail-closed with context.DeadlineExceeded, got %v", err)
	}
	ok, observed, _ := blocker.observation()
	if !ok {
		t.Fatal("census dependency observed no deadline despite the caller supplying one")
	}
	if observed.After(callerDeadline) {
		t.Fatalf("boundary did not preserve the earlier caller deadline: caller=%s observed=%s", callerDeadline, observed)
	}
}

// TestLifecycleSweepCanceledCallerPreventsDownstreamMutation proves a
// canceled caller context reaches the dependency fail-closed and no reap
// mutation runs after cancellation.
func TestLifecycleSweepCanceledCallerPreventsDownstreamMutation(t *testing.T) {
	policy := lifecycleTestPolicy(t, true)
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	var mu sync.Mutex
	removals := 0
	governor := &resources.Governor{
		Policy: policy, Capacity: lifecycleCapacityBackend{},
		Worktrees: lifecycleCanceledAwareWorktrees{}, Locks: resources.FileLockProvider{},
		RemoveTree: func(string) error {
			mu.Lock()
			defer mu.Unlock()
			removals++
			return nil
		},
	}
	_, err := runLifecycleGovernorSweep(parent, governor, resources.SweepReviewBeforeRefusal)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled caller must fail closed with context.Canceled, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if removals != 0 {
		t.Fatalf("post-cancellation mutation observed: %d RemoveTree calls", removals)
	}
}

// lifecycleCanceledAwareWorktrees refuses to proceed once its context is
// canceled, mirroring a cooperative dependency at the census seam.
type lifecycleCanceledAwareWorktrees struct{}

func (lifecycleCanceledAwareWorktrees) List(ctx context.Context, _, _ string) ([]resources.RegisteredWorktree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}
