package deps

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// countingScopedStore records whether a caller paid for a project-wide snapshot.
type countingScopedStore struct {
	*MemoryStore
	bulk   atomic.Int64
	scoped atomic.Int64
	snap   *GraphSnapshot
}

func (c *countingScopedStore) SnapshotGraph(ctx context.Context) (*GraphSnapshot, error) {
	c.bulk.Add(1)
	return c.MemoryStore.SnapshotGraph(ctx)
}

func (c *countingScopedStore) SnapshotGraphForTask(ctx context.Context, ref Ref, id TaskID, desired []DependencyEdge) (*GraphSnapshot, error) {
	c.scoped.Add(1)
	if c.snap != nil {
		return c.snap, nil
	}
	// Zero-edge task-scoped answer with a real revision: the card has no
	// relations, and we reached the provider. Edges must be non-nil empty:
	// Reconcile treats FullClosure=nil as "missing snapshot".
	return &GraphSnapshot{Edges: []DependencyEdge{}, ProviderRevision: "task-scoped-nonzero"}, nil
}

// A zero-edge exact card must never invoke the bulk project fetch. RejectEmpty
// runs inside ValidateLaunch on the scoped snapshot (not a caller pre-check).
func TestValidateLaunchNeverInvokesBulkWhenScoped(t *testing.T) {
	store := &countingScopedStore{MemoryStore: NewMemoryStore()}
	store.EnsureTask("FAC-1", provider.StatusToDo, provider.PriorityHigh)
	des := EmptyProvenanceBound("FAC-1", "id-fac-1")

	gr, err := RequireTaskLaunch(context.Background(), store, EntryDispatch, "FAC-1", des, "")
	if err != nil {
		t.Fatalf("zero-edge task with real revision must launch: %v", err)
	}
	if gr == nil || !gr.OK {
		t.Fatalf("want OK gate result, got %+v", gr)
	}
	if store.bulk.Load() != 0 {
		t.Fatalf("bulk SnapshotGraph called %d times; ValidateLaunch must not fan out the project", store.bulk.Load())
	}
	if store.scoped.Load() != 1 {
		t.Fatalf("scoped SnapshotGraphForTask called %d times, want 1", store.scoped.Load())
	}
}

// The non-vacuity guard survives inside ValidateLaunch: empty edges PLUS the
// empty-provider sentinel still fail closed on the task-scoped path.
func TestValidateLaunchEmptyProviderSentinelStillRejected(t *testing.T) {
	store := &countingScopedStore{
		MemoryStore: NewMemoryStore(),
		snap: &GraphSnapshot{
			Edges:            []DependencyEdge{},
			ProviderRevision: emptyProviderGraphRevision,
		},
	}
	store.EnsureTask("FAC-2", provider.StatusToDo, provider.PriorityHigh)
	des := EmptyProvenanceBound("FAC-2", "id-fac-2")

	_, err := RequireTaskLaunch(context.Background(), store, EntryDispatch, "FAC-2", des, "")
	if err == nil {
		t.Fatal("sentinel empty-provider must still be refused")
	}
	var be *BlockedError
	if !errors.As(err, &be) || be.Code != "stale" {
		t.Fatalf("want BlockedError code=stale, got %v", err)
	}
	if !strings.Contains(be.Reason, "empty provider graph") {
		t.Fatalf("reason must name empty provider graph, got %q", be.Reason)
	}
	if store.bulk.Load() != 0 {
		t.Fatalf("bulk fetch invoked while refusing empty-provider sentinel")
	}
	if store.scoped.Load() != 1 {
		t.Fatalf("scoped SnapshotGraphForTask called %d times, want 1", store.scoped.Load())
	}
}

// ProviderStore must not emit the vacuous empty-stream digest for a real
// zero-edge observation. ec01bdd moved RejectEmptyProviderGraph into
// ValidateLaunch; without a domain separator every no-deps launch died.
func TestProviderStoreObservedEmptySurvivesValidateLaunch(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-196", Title: "no deps", Priority: provider.PriorityUrgent,
		Status: provider.StatusToDo, ProjectID: "p1",
	})
	store := NewProviderStore(mp, "p1")
	des := EmptyProvenanceBound("FAC-196", "t-1")

	snap, err := store.SnapshotGraphForTask(context.Background(), "FAC-196", "t-1", nil)
	if err != nil {
		t.Fatalf("SnapshotGraphForTask: %v", err)
	}
	if err := RejectEmptyProviderGraph(snap); err != nil {
		t.Fatalf("observed empty must not look vacuous: %v (rev=%q)", err, snap.ProviderRevision)
	}
	if snap.ProviderRevision == emptyProviderGraphRevision {
		t.Fatal("ProviderStore observed-empty collided with empty-stream sentinel")
	}

	gr, err := RequireTaskLaunch(context.Background(), store, EntryDispatch, "FAC-196", des, "")
	if err != nil {
		t.Fatalf("zero-edge ProviderStore launch must succeed: %v", err)
	}
	if gr == nil || !gr.OK {
		t.Fatalf("want OK gate result, got %+v", gr)
	}
}
