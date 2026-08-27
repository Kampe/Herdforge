package deps

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
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
	// relations, and we reached the provider.
	return &GraphSnapshot{Edges: nil, ProviderRevision: "task-scoped-nonzero"}, nil
}

// A zero-edge exact card must never invoke the bulk project fetch. That is the
// residual the coordinator named: RejectEmptyProviderGraph stays, but it must
// run on the task snapshot.
func TestExactCardGraphSnapshotNeverInvokesBulkWhenScoped(t *testing.T) {
	store := &countingScopedStore{MemoryStore: NewMemoryStore()}
	snap, err := ExactCardGraphSnapshot(context.Background(), store, "FAC-1", "id-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RejectEmptyProviderGraph(snap); err != nil {
		t.Fatalf("zero-edge task with real revision must not look like empty-provider: %v", err)
	}
	if store.bulk.Load() != 0 {
		t.Fatalf("bulk SnapshotGraph called %d times; exact-card check must not fan out the project", store.bulk.Load())
	}
	if store.scoped.Load() != 1 {
		t.Fatalf("scoped SnapshotGraphForTask called %d times, want 1", store.scoped.Load())
	}
}

// The non-vacuity guard survives: empty edges PLUS the empty-provider sentinel
// still fail closed on the task-scoped path.
func TestExactCardEmptyProviderSentinelStillRejected(t *testing.T) {
	store := &countingScopedStore{
		MemoryStore: NewMemoryStore(),
		snap: &GraphSnapshot{
			Edges:            nil,
			ProviderRevision: emptyProviderGraphRevision,
		},
	}
	snap, err := ExactCardGraphSnapshot(context.Background(), store, "FAC-2", "id-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = RejectEmptyProviderGraph(snap)
	if err == nil || !strings.Contains(err.Error(), "empty provider graph") {
		t.Fatalf("sentinel empty-provider must still be refused, got %v", err)
	}
	if store.bulk.Load() != 0 {
		t.Fatalf("bulk fetch invoked while refusing empty-provider sentinel")
	}
}
