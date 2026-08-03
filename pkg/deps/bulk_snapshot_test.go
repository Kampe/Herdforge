package deps

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// delayedBulkProvider simulates a 166-task live board where each per-task
// ListRelations is expensive. ListProjectRelations is the bulk path (one
// logical op + bounded internal work). Memory tests without bulk would hide
// the sequential stampede.
type delayedBulkProvider struct {
	provider.MemoryProvider
	tasks []*provider.Task

	listTasksDelay time.Duration
	listRelDelay   time.Duration
	bulkDelay      time.Duration

	listTasksCalls atomic.Int64
	listRelCalls   atomic.Int64
	bulkCalls      atomic.Int64

	// bulkImpl when true implements ListProjectRelations without N sequential waits.
	useBulk bool

	// failAfterBulk lets tests inject stale revision by mutating after first bulk.
	mu        sync.Mutex
	relations []provider.Relation
}

func newDelayedBoard(n int, listRelDelay, bulkDelay time.Duration, useBulk bool) *delayedBulkProvider {
	mp := provider.NewMemoryProvider()
	d := &delayedBulkProvider{
		MemoryProvider: *mp,
		listRelDelay:   listRelDelay,
		bulkDelay:      bulkDelay,
		useBulk:        useBulk,
	}
	// Seed n tasks; chain of blocks edges so full closure is non-empty.
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("id-%d", i)
		ref := fmt.Sprintf("FAC-%d", i)
		t := &provider.Task{
			ID: id, Ref: ref, Status: "to-do", Priority: provider.PriorityMedium,
			ProjectID: "proj", Title: ref,
			Description: fmt.Sprintf("```herd-deps-v1\n{\"version\":1,\"task_ref\":%q,\"task_id\":%q,\"edges\":[]}\n```\n", ref, id),
		}
		if i == n {
			// Head task depends on FAC-1 (done) so it is launchable.
			t.Description = fmt.Sprintf(
				"```herd-deps-v1\n{\"version\":1,\"task_ref\":%q,\"task_id\":%q,\"edges\":[{\"source_ref\":\"FAC-1\",\"target_ref\":%q,\"type\":\"blocks\"}]}\n```\n",
				ref, id, ref)
		}
		d.tasks = append(d.tasks, t)
		d.AddTask(t)
	}
	// FAC-1 done; edge FAC-1 blocks FAC-n
	d.tasks[0].Status = "done"
	d.AddTask(d.tasks[0])
	rel := provider.Relation{
		ID: "rel-1-n", SourceTaskID: "id-1", TargetTaskID: fmt.Sprintf("id-%d", n),
		Type: provider.RelationBlocks,
	}
	d.mu.Lock()
	d.relations = []provider.Relation{rel}
	d.mu.Unlock()
	_, _ = d.CreateRelation(context.Background(), "id-1", fmt.Sprintf("id-%d", n), provider.RelationBlocks)
	return d
}

func (d *delayedBulkProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	d.listTasksCalls.Add(1)
	if d.listTasksDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d.listTasksDelay):
		}
	}
	return d.MemoryProvider.ListTasks(ctx, projectID, status)
}

func (d *delayedBulkProvider) ListRelations(ctx context.Context, taskID string) ([]provider.Relation, error) {
	d.listRelCalls.Add(1)
	if d.listRelDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d.listRelDelay):
		}
	}
	return d.MemoryProvider.ListRelations(ctx, taskID)
}

func (d *delayedBulkProvider) ListProjectRelations(ctx context.Context, projectID string) ([]provider.Relation, error) {
	if !d.useBulk {
		// Force the broken sequential path by not implementing bulk usefully —
		// still satisfy interface but call ListRelations for every task.
		tasks, err := d.ListTasks(ctx, projectID, "")
		if err != nil {
			return nil, err
		}
		seen := map[string]provider.Relation{}
		for _, t := range tasks {
			if t == nil {
				continue
			}
			rels, err := d.ListRelations(ctx, t.ID)
			if err != nil {
				return nil, err
			}
			for _, r := range rels {
				seen[r.ID] = r
			}
		}
		out := make([]provider.Relation, 0, len(seen))
		for _, r := range seen {
			out = append(out, r)
		}
		return out, nil
	}
	d.bulkCalls.Add(1)
	if d.bulkDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d.bulkDelay):
		}
	}
	return d.MemoryProvider.ListProjectRelations(ctx, projectID)
}

func (d *delayedBulkProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	return d.MemoryProvider.GetTask(ctx, id)
}

// TestSnapshotGraph_166TaskBulkBoundedCallCount proves the production bulk path
// does not sequential-stampede: with 5ms "CLI" delay, sequential would take
// ~830ms+; bulk+fence must stay bounded and use O(1) bulk calls per fence.
func TestSnapshotGraph_166TaskBulkBoundedCallCount(t *testing.T) {
	const n = 166
	// 5ms per ListRelations × 166 ≈ 830ms sequential floor; bulk is one 5ms call.
	p := newDelayedBoard(n, 5*time.Millisecond, 5*time.Millisecond, true)
	store := NewProviderStore(p, "proj")
	ctx, fence := WithSnapshotFence(context.Background())

	start := time.Now()
	snap1, err := store.SnapshotGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap1 == nil || len(snap1.Edges) < 1 {
		t.Fatalf("expected edges, got %+v", snap1)
	}
	// Reuse within fence — no second bulk.
	snap2, err := store.SnapshotGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap1.ProviderRevision != snap2.ProviderRevision {
		t.Fatal("fence reuse must return same provider revision")
	}
	elapsed := time.Since(start)

	if p.bulkCalls.Load() != 1 {
		t.Fatalf("want 1 bulk call for fence, got %d", p.bulkCalls.Load())
	}
	if p.listRelCalls.Load() != 0 {
		t.Fatalf("bulk path must not call per-task ListRelations, got %d", p.listRelCalls.Load())
	}
	if p.listTasksCalls.Load() != 1 {
		t.Fatalf("want 1 ListTasks hydration per fence, got %d", p.listTasksCalls.Load())
	}
	// Sequential would be >= 166*5ms = 830ms; allow generous headroom under 300ms.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("bulk snapshot too slow (%v); likely sequential stampede", elapsed)
	}
	_ = fence

	// ValidateLaunch twice within fence: still one bulk.
	headRef := Ref(fmt.Sprintf("FAC-%d", n))
	des, err := ExtractProvenanceFromText(p.tasks[n-1].Description)
	if err != nil || des == nil {
		t.Fatal(err)
	}
	// Need task_id in fence — bind.
	if err := des.BindAndValidate(headRef, TaskID(fmt.Sprintf("id-%d", n))); err != nil {
		t.Fatal(err)
	}
	g1, err := RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, g1.GraphRevision)
	if err != nil {
		t.Fatal(err)
	}
	if p.bulkCalls.Load() != 1 {
		t.Fatalf("pre-side-effect gate reuse: want still 1 bulk, got %d", p.bulkCalls.Load())
	}
	if p.listTasksCalls.Load() != 1 {
		t.Fatalf("pre-side-effect: want 1 ListTasks, got %d", p.listTasksCalls.Load())
	}

	// Invalidate → fresh bulk for post TOCTOU.
	fence.Invalidate(false)
	_, err = RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, g1.GraphRevision)
	if err != nil {
		t.Fatal(err)
	}
	if p.bulkCalls.Load() != 2 {
		t.Fatalf("after Invalidate want 2 bulk calls, got %d", p.bulkCalls.Load())
	}
}

// TestSnapshotGraph_CancelStopsBulkFanout ensures ctx cancel fails closed fast.
func TestSnapshotGraph_CancelStopsBulkFanout(t *testing.T) {
	p := newDelayedBoard(50, 0, 200*time.Millisecond, true)
	store := NewProviderStore(p, "proj")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ctx, _ = WithSnapshotFence(ctx)
	start := time.Now()
	_, err := store.SnapshotGraph(ctx)
	if err == nil {
		t.Fatal("expected cancel error")
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("cancel did not bound latency: %v", time.Since(start))
	}
}

// TestSnapshotGraph_StaleRevisionRejected after fence invalidate + relation change.
func TestSnapshotGraph_StaleRevisionRejected(t *testing.T) {
	p := newDelayedBoard(20, 0, 0, true)
	store := NewProviderStore(p, "proj")
	ctx, fence := WithSnapshotFence(context.Background())
	headRef := Ref("FAC-20")
	des, _ := ExtractProvenanceFromText(p.tasks[19].Description)
	_ = des.BindAndValidate(headRef, "id-20")

	g1, err := RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, "")
	if err != nil {
		t.Fatal(err)
	}
	// Mutate graph: add a new open blocker edge FAC-2 (to-do) blocks FAC-20.
	if _, err := p.CreateRelation(context.Background(), "id-2", "id-20", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}
	// Update desired to still only declare FAC-1 — board has extra edge → drift
	// after fresh snapshot. Also selection rev mismatch if only edges hash changes.
	fence.Invalidate(false)
	_, err = RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, g1.GraphRevision)
	if err == nil {
		t.Fatal("expected toctou or drift after relation mutation")
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("want BlockedError, got %v", err)
	}
	if be.Code != "toctou" && be.Code != "drift" && be.Code != "open_blocker" {
		t.Fatalf("want toctou/drift/open_blocker, got %s: %v", be.Code, err)
	}
}

// TestSnapshotGraph_NonBulkSequentialIsDetectablySlow is a mutation control:
// if bulk is disabled and per-task delay is high, SnapshotGraph exceeds budget.
// Documents why bulk is required (not vacuous).
func TestSnapshotGraph_NonBulkSequentialIsDetectablySlow(t *testing.T) {
	p := newDelayedBoard(40, 3*time.Millisecond, 0, false)
	store := NewProviderStore(p, "proj")
	// Non-bulk path still calls ListProjectRelations which internally sequential
	// ListRelations — ProviderStore prefers bulk interface which this implements
	// with sequential body.
	ctx, _ := WithSnapshotFence(context.Background())
	start := time.Now()
	_, err := store.SnapshotGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if p.listRelCalls.Load() < 30 {
		t.Fatalf("expected sequential ListRelations storm, got %d", p.listRelCalls.Load())
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("expected sequential delay to accumulate, got %v", elapsed)
	}
}
