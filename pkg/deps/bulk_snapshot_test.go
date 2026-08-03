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
	// Post-selection check: still one project fan-out; closure re-read only
	// (launch + transitive prereqs — here FAC-n and FAC-1), not O(board).
	relBefore := p.listRelCalls.Load()
	_, err = RequireTaskLaunch(ctx, store, EntryDispatch, headRef, des, g1.GraphRevision)
	if err != nil {
		t.Fatal(err)
	}
	if p.bulkCalls.Load() != 1 {
		t.Fatalf("post check must not re-run ListProjectRelations, got %d", p.bulkCalls.Load())
	}
	// Closure = {FAC-n, FAC-1} → 2 ListRelations (not 166).
	delta := p.listRelCalls.Load() - relBefore
	if delta < 1 || delta > 4 {
		t.Fatalf("want small closure re-read (≈2), got delta %d", delta)
	}
	if delta >= int64(n/2) {
		t.Fatalf("closure refresh must not re-read half the board, delta=%d n=%d", delta, n)
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
// launch target is caught by closure refresh without ListProjectRelations×2.
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

// TestPrerequisiteClosure_IndirectEdgeChange_CompensatesOnce: A→B→T chain;
// after fence, mutate A→B (indirect prereq). Launch-task-only incident check
// would miss this; closure refresh must TOCTOU and FencedClaim compensates
// exactly once.
func TestPrerequisiteClosure_IndirectEdgeChange_CompensatesOnce(t *testing.T) {
	mp := provider.NewMemoryProvider()
	// A (done) blocks B (done) blocks T (to-do).
	for _, tk := range []*provider.Task{
		{ID: "id-A", Ref: "FAC-A", Status: "done", ProjectID: "proj", Priority: provider.PriorityHigh,
			Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-A\",\"task_id\":\"id-A\",\"edges\":[]}\n```\n"},
		{ID: "id-B", Ref: "FAC-B", Status: "done", ProjectID: "proj", Priority: provider.PriorityHigh,
			Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-B\",\"task_id\":\"id-B\",\"edges\":[]}\n```\n"},
		{ID: "id-T", Ref: "FAC-T", Status: "to-do", ProjectID: "proj", Priority: provider.PriorityUrgent,
			Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-T\",\"task_id\":\"id-T\",\"edges\":[{\"source_ref\":\"FAC-B\",\"target_ref\":\"FAC-T\",\"type\":\"blocks\"}]}\n```\n"},
	} {
		mp.AddTask(tk)
	}
	if _, err := mp.CreateRelation(context.Background(), "id-A", "id-B", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.CreateRelation(context.Background(), "id-B", "id-T", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}

	store := NewProviderStore(mp, "proj")
	ctx, _ := WithSnapshotFence(context.Background())
	des, err := ExtractProvenanceFromText(
		"```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-T\",\"task_id\":\"id-T\",\"edges\":[{\"source_ref\":\"FAC-B\",\"target_ref\":\"FAC-T\",\"type\":\"blocks\"}]}\n```\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := des.BindAndValidate("FAC-T", "id-T"); err != nil {
		t.Fatal(err)
	}

	pre, err := RequireTaskLaunch(ctx, store, EntryClaim, "FAC-T", des, "")
	if err != nil {
		t.Fatalf("pre-claim must pass: %v", err)
	}

	// Capture relation A→B for deletion (indirect edge).
	snap, _ := store.SnapshotGraph(ctx)
	var relAB string
	for _, e := range snap.Edges {
		if e.SourceID == "id-A" && e.TargetID == "id-B" {
			relAB = e.RelationID
			break
		}
	}
	if relAB == "" {
		t.Fatal("expected A→B edge in snapshot")
	}

	var comps int
	_, err = FencedClaim(ctx, store, "FAC-T", "id-T", des, pre.GraphRevision,
		func(context.Context) error {
			// Concurrent mutation on indirect prerequisite edge during claim.
			if err := mp.DeleteRelation(context.Background(), relAB, "id-A", "id-B"); err != nil {
				return err
			}
			return nil
		},
		func(context.Context, TaskID, string) error {
			comps++
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected post-claim TOCTOU after indirect prereq edge change")
	}
	if !errors.Is(err, ErrPostClaimDrift) {
		t.Fatalf("want ErrPostClaimDrift, got %v", err)
	}
	if comps != 1 {
		t.Fatalf("want exactly-one compensation, got %d", comps)
	}
	// Must not have re-run full project ListProjectRelations (Memory path: bulkCalls).
	// ProviderStore uses Memory ListProjectRelations — fence still holds first snap.
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
