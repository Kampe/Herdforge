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

// TestSnapshotGraph_FenceReuse_NoSecondProjectFanout proves fence reuse:
// one ListProjectRelations per fence; pre+post launch checks do not re-fanout
// the board (post TOCTOU is O(1) incident ListRelations on the target).
// Note: delayedBulkProvider is NOT Kaneo production proof — see
// provider.TestKaneoListProjectRelations_* for real KaneoProvider paths.
func TestSnapshotGraph_FenceReuse_NoSecondProjectFanout(t *testing.T) {
	const n = 166
	p := newDelayedBoard(n, 5*time.Millisecond, 5*time.Millisecond, true)
	store := NewProviderStore(p, "proj")
	ctx, _ := WithSnapshotFence(context.Background())

	start := time.Now()
	snap1, err := store.SnapshotGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap1 == nil || len(snap1.Edges) < 1 {
		t.Fatalf("expected edges, got %+v", snap1)
	}
	snap2, err := store.SnapshotGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap1.ProviderRevision != snap2.ProviderRevision {
		t.Fatal("fence reuse must return same provider revision")
	}
	elapsed := time.Since(start)

	if p.bulkCalls.Load() != 1 {
		t.Fatalf("want 1 ListProjectRelations for fence, got %d", p.bulkCalls.Load())
	}
	if p.listTasksCalls.Load() != 1 {
		t.Fatalf("want 1 ListTasks hydration per fence, got %d", p.listTasksCalls.Load())
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("fence path too slow (%v)", elapsed)
	}

	headRef := Ref(fmt.Sprintf("FAC-%d", n))
	des, err := ExtractProvenanceFromText(p.tasks[n-1].Description)
	if err != nil || des == nil {
		t.Fatal(err)
	}
	if err := des.BindAndValidate(headRef, TaskID(fmt.Sprintf("id-%d", n))); err != nil {
		t.Fatal(err)
	}
	g1, err := RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, "")
	if err != nil {
		t.Fatal(err)
	}
	// Post-selection check: still one project fan-out; +incident ListRelations only.
	relBefore := p.listRelCalls.Load()
	_, err = RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, g1.GraphRevision)
	if err != nil {
		t.Fatal(err)
	}
	if p.bulkCalls.Load() != 1 {
		t.Fatalf("post check must not re-run ListProjectRelations, got %d", p.bulkCalls.Load())
	}
	// AssertIncidentEdgesFresh → exactly one ListRelations on the target.
	if p.listRelCalls.Load()-relBefore != 1 {
		t.Fatalf("want 1 incident ListRelations on post check, got delta %d", p.listRelCalls.Load()-relBefore)
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

// TestSnapshotGraph_IncidentTOCTOU_WithoutFullRefanout: mutating edges on the
// launch target is caught by O(1) incident refresh without ListProjectRelations×2.
func TestSnapshotGraph_IncidentTOCTOU_WithoutFullRefanout(t *testing.T) {
	p := newDelayedBoard(20, 0, 0, true)
	store := NewProviderStore(p, "proj")
	ctx, _ := WithSnapshotFence(context.Background())
	headRef := Ref("FAC-20")
	des, _ := ExtractProvenanceFromText(p.tasks[19].Description)
	_ = des.BindAndValidate(headRef, "id-20")

	g1, err := RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, "")
	if err != nil {
		t.Fatal(err)
	}
	bulkAfterPre := p.bulkCalls.Load()
	// Mutate graph: add edge FAC-2 blocks FAC-20 (incident on target).
	if _, err := p.CreateRelation(context.Background(), "id-2", "id-20", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}
	_, err = RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, g1.GraphRevision)
	if err == nil {
		t.Fatal("expected toctou after incident relation mutation")
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("want BlockedError, got %v", err)
	}
	if be.Code != "toctou" {
		t.Fatalf("want toctou, got %s: %v", be.Code, err)
	}
	if p.bulkCalls.Load() != bulkAfterPre {
		t.Fatalf("post TOCTOU must not re-run project fan-out, bulk %d→%d", bulkAfterPre, p.bulkCalls.Load())
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
