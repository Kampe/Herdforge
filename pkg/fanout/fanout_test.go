package fanout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/deps"
)

func blk(src, tgt string) deps.DependencyEdge {
	return deps.DependencyEdge{SourceRef: deps.Ref(src), TargetRef: deps.Ref(tgt), Type: deps.EdgeBlocks}
}

func TestSelectIndependent_NoEdges(t *testing.T) {
	indep, dep := SelectIndependent([]string{"FAC-1", "FAC-2", "FAC-3"}, nil)
	if len(dep) != 0 {
		t.Fatalf("expected 0 dependent, got %d", len(dep))
	}
	if len(indep) != 3 {
		t.Fatalf("expected 3 independent, got %d", len(indep))
	}
}

func TestSelectIndependent_Chain(t *testing.T) {
	refs := []string{"FAC-1", "FAC-2", "FAC-3"}
	edges := []deps.DependencyEdge{
		blk("FAC-1", "FAC-2"),
		blk("FAC-2", "FAC-3"),
	}
	indep, dep := SelectIndependent(refs, edges)
	if len(indep) != 0 {
		t.Fatalf("chain: expected 0 independent, got %v", indep)
	}
	if len(dep) != 3 {
		t.Fatalf("chain: expected 3 dependent, got %v", dep)
	}
}

func TestSelectIndependent_Mixed(t *testing.T) {
	refs := []string{"FAC-1", "FAC-2", "FAC-3", "FAC-4", "FAC-5"}
	edges := []deps.DependencyEdge{
		blk("FAC-1", "FAC-2"),
	}
	indep, dep := SelectIndependent(refs, edges)
	if len(indep) != 3 {
		t.Fatalf("expected 3 independent (FAC-3,FAC-4,FAC-5), got %v", indep)
	}
	if len(dep) != 2 {
		t.Fatalf("expected 2 dependent (FAC-1,FAC-2), got %v", dep)
	}
}

func TestSelectIndependent_OnlyBlocksMatters(t *testing.T) {
	refs := []string{"FAC-1", "FAC-2"}
	edges := []deps.DependencyEdge{
		{SourceRef: deps.Ref("FAC-1"), TargetRef: deps.Ref("FAC-2"), Type: deps.EdgeRelated},
	}
	indep, dep := SelectIndependent(refs, edges)
	if len(indep) != 2 {
		t.Fatalf("related edges should not make tasks dependent, got indep=%v", indep)
	}
	if len(dep) != 0 {
		t.Fatalf("related edges should not produce dependent set, got dep=%v", dep)
	}
}

func TestSelectIndependent_EdgeOutsideSet(t *testing.T) {
	refs := []string{"FAC-1", "FAC-2"}
	edges := []deps.DependencyEdge{
		blk("FAC-1", "FAC-99"),
	}
	indep, dep := SelectIndependent(refs, edges)
	if len(indep) != 2 {
		t.Fatalf("edge to task outside set should not block, got indep=%v", indep)
	}
	if len(dep) != 0 {
		t.Fatalf("edge to task outside set should not produce dependent, got dep=%v", dep)
	}
}

func TestRun_ParallelDispatch(t *testing.T) {
	var dispatched int64
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		atomic.AddInt64(&dispatched, 1)
		time.Sleep(10 * time.Millisecond)
		return DispatchResult{TaskRef: ref, Worktree: "/tmp/wt-" + ref, Branch: "task-" + ref, Launched: true}, nil
	}
	rep, err := Run(context.Background(), []string{"FAC-1", "FAC-2", "FAC-3", "FAC-4"}, dispatch, Options{Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&dispatched) != 4 {
		t.Fatalf("expected 4 dispatches, got %d", dispatched)
	}
	if rep.Succeeded != 4 {
		t.Fatalf("expected 4 succeeded, got %d", rep.Succeeded)
	}
	if rep.Failed != 0 {
		t.Fatalf("expected 0 failed, got %d", rep.Failed)
	}
}

func TestRun_ConcurrencyBounded(t *testing.T) {
	var current, max int64
	var mu sync.Mutex
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		c := atomic.AddInt64(&current, 1)
		mu.Lock()
		if c > max {
			max = c
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&current, -1)
		return DispatchResult{TaskRef: ref, Launched: true}, nil
	}
	rep, err := Run(context.Background(), []string{"FAC-1", "FAC-2", "FAC-3", "FAC-4", "FAC-5", "FAC-6"}, dispatch, Options{Parallelism: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Succeeded != 6 {
		t.Fatalf("expected 6 succeeded, got %d", rep.Succeeded)
	}
	if max > 3 {
		t.Fatalf("concurrency exceeded limit: max=%d, limit=3", max)
	}
}

func TestRun_SkipsDependentTasks(t *testing.T) {
	var dispatched int64
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		atomic.AddInt64(&dispatched, 1)
		return DispatchResult{TaskRef: ref, Launched: true}, nil
	}
	edges := []deps.DependencyEdge{
		blk("FAC-1", "FAC-2"),
	}
	rep, err := Run(context.Background(), []string{"FAC-1", "FAC-2", "FAC-3"}, dispatch, Options{Edges: edges, Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&dispatched) != 1 {
		t.Fatalf("expected only 1 independent dispatch (FAC-3), got %d", dispatched)
	}
	if len(rep.Skipped) != 2 {
		t.Fatalf("expected 2 skipped (FAC-1, FAC-2), got %v", rep.Skipped)
	}
	if rep.Succeeded != 1 {
		t.Fatalf("expected 1 succeeded, got %d", rep.Succeeded)
	}
}

func TestRun_PropagatesErrors(t *testing.T) {
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		if ref == "FAC-2" {
			return DispatchResult{TaskRef: ref}, errors.New("agent launch failed")
		}
		return DispatchResult{TaskRef: ref, Launched: true}, nil
	}
	rep, err := Run(context.Background(), []string{"FAC-1", "FAC-2", "FAC-3"}, dispatch, Options{Parallelism: 4})
	if err == nil {
		t.Fatal("expected error from failed dispatch")
	}
	if rep.Failed != 1 {
		t.Fatalf("expected 1 failed, got %d", rep.Failed)
	}
	if rep.Succeeded != 2 {
		t.Fatalf("expected 2 succeeded, got %d", rep.Succeeded)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		return DispatchResult{TaskRef: ref, Launched: true}, nil
	}
	_, err := Run(ctx, []string{"FAC-1", "FAC-2"}, dispatch, Options{Parallelism: 4})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestRun_MidRunCancellationSetsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		cancel()
		<-ctx.Done()
		return DispatchResult{TaskRef: ref, Err: ctx.Err()}, ctx.Err()
	}
	rep, err := Run(ctx, []string{"FAC-1", "FAC-2", "FAC-3", "FAC-4"}, dispatch, Options{Parallelism: 1})
	if err == nil && rep.Failed > 0 {
		t.Fatalf("mid-run cancellation with failed tasks must return non-nil error: err=%v failed=%d", err, rep.Failed)
	}
}

func TestRun_NilDispatch(t *testing.T) {
	_, err := Run(context.Background(), []string{"FAC-1"}, nil, Options{})
	if err == nil {
		t.Fatal("expected error for nil dispatch function")
	}
}

func TestRun_EmptyRefs(t *testing.T) {
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		t.Fatal("dispatch should not be called for empty refs")
		return DispatchResult{}, nil
	}
	rep, err := Run(context.Background(), nil, dispatch, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Succeeded != 0 || rep.Failed != 0 {
		t.Fatalf("expected zero results, got succeeded=%d failed=%d", rep.Succeeded, rep.Failed)
	}
}

func TestRun_DefaultParallelism(t *testing.T) {
	var dispatched int64
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		atomic.AddInt64(&dispatched, 1)
		return DispatchResult{TaskRef: ref, Launched: true}, nil
	}
	rep, err := Run(context.Background(), []string{"FAC-1", "FAC-2"}, dispatch, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Parallelism != DefaultParallelism {
		t.Fatalf("expected default parallelism %d, got %d", DefaultParallelism, rep.Parallelism)
	}
}

func TestRun_DoesNotDeadlock(t *testing.T) {
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		time.Sleep(5 * time.Millisecond)
		return DispatchResult{TaskRef: ref, Launched: true}, nil
	}
	done := make(chan struct{})
	go func() {
		Run(context.Background(), []string{"FAC-1", "FAC-2", "FAC-3"}, dispatch, Options{Parallelism: 2})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fan-out deadlocked")
	}
}

func TestRun_AllDependent(t *testing.T) {
	var dispatched int64
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		atomic.AddInt64(&dispatched, 1)
		return DispatchResult{TaskRef: ref, Launched: true}, nil
	}
	edges := []deps.DependencyEdge{
		blk("FAC-1", "FAC-2"),
		blk("FAC-2", "FAC-3"),
	}
	rep, err := Run(context.Background(), []string{"FAC-1", "FAC-2", "FAC-3"}, dispatch, Options{Edges: edges, Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&dispatched) != 0 {
		t.Fatalf("expected 0 dispatches (all dependent), got %d", dispatched)
	}
	if len(rep.Skipped) != 3 {
		t.Fatalf("expected 3 skipped, got %v", rep.Skipped)
	}
}

func TestDispatchResult_ErrorPreserved(t *testing.T) {
	expectedErr := fmt.Errorf("boom")
	dispatch := func(ctx context.Context, ref string) (DispatchResult, error) {
		return DispatchResult{}, expectedErr
	}
	rep, _ := Run(context.Background(), []string{"FAC-1"}, dispatch, Options{Parallelism: 1})
	if len(rep.Dispatched) != 1 {
		t.Fatalf("expected 1 dispatched result, got %d", len(rep.Dispatched))
	}
	if rep.Dispatched[0].Err == nil || rep.Dispatched[0].Err.Error() != "boom" {
		t.Fatalf("expected error 'boom' preserved, got %v", rep.Dispatched[0].Err)
	}
}
