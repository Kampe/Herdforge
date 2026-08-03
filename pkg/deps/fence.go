package deps

import (
	"context"
	"sync"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// SnapshotFence holds an immutable graph snapshot + one task hydration for the
// duration of a launch fence (selection → re-read → post-check). Prevents
// thrashing ListTasks / ListProjectRelations on every ValidateLaunch call
// without introducing an unsafe long-lived cache.
//
// Freshness rules:
//   - First SnapshotGraph populates the fence.
//   - Subsequent SnapshotGraph calls reuse the immutable snap until Invalidate.
//   - Dispatch invalidates before the final post-side-effect TOCTOU check so
//     relation mutations during launch still fail closed.
//   - TaskStatus uses fence task maps for id resolution; still GetTask for the
//     individual card's live status (blocker Done check).
type SnapshotFence struct {
	mu        sync.Mutex
	snap      *GraphSnapshot
	tasksByRef map[string]*provider.Task
	tasksByID  map[string]*provider.Task
	// listTasksCalls / bulkCalls are test counters (optional hooks).
	hydrated bool
	// forceNext makes the next SnapshotGraph reload (post-invalidate).
	invalidated bool
}

type fenceCtxKey struct{}

// WithSnapshotFence attaches a new fence to ctx. Call once at the start of
// Dispatch / RunPulse claim path.
func WithSnapshotFence(ctx context.Context) (context.Context, *SnapshotFence) {
	f := &SnapshotFence{
		tasksByRef: map[string]*provider.Task{},
		tasksByID:  map[string]*provider.Task{},
	}
	return context.WithValue(ctx, fenceCtxKey{}, f), f
}

// FenceFrom returns the fence on ctx, or nil.
func FenceFrom(ctx context.Context) *SnapshotFence {
	if ctx == nil {
		return nil
	}
	f, _ := ctx.Value(fenceCtxKey{}).(*SnapshotFence)
	return f
}

// Invalidate drops the cached snapshot so the next SnapshotGraph reloads.
// Task hydration may be retained for id/ref maps unless clearTasks is true.
func (f *SnapshotFence) Invalidate(clearTasks bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = nil
	f.invalidated = true
	if clearTasks {
		f.tasksByRef = map[string]*provider.Task{}
		f.tasksByID = map[string]*provider.Task{}
		f.hydrated = false
	}
}

// GetSnap returns a clone of the cached snapshot, or nil.
func (f *SnapshotFence) GetSnap() *GraphSnapshot {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snap == nil {
		return nil
	}
	return cloneSnapshot(f.snap)
}

// setSnap stores an immutable snapshot on the fence.
func (f *SnapshotFence) setSnap(snap *GraphSnapshot) {
	if f == nil || snap == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = cloneSnapshot(snap)
	f.invalidated = false
}

func cloneSnapshot(s *GraphSnapshot) *GraphSnapshot {
	if s == nil {
		return nil
	}
	// Always non-nil Edges so Reconcile RequireFullClosure treats empty
	// boards as a real full closure (nil means "missing snapshot").
	edges := make([]DependencyEdge, 0, len(s.Edges))
	edges = append(edges, s.Edges...)
	return &GraphSnapshot{
		ProviderRevision: s.ProviderRevision,
		Edges:            edges,
	}
}
