// Package fanout dispatches independent tasks in parallel, bounded by a
// concurrency semaphore. It is the graph-engineering fan-out layer: tasks
// with no EdgeBlocks between them run simultaneously, each in its own
// worktree, while the serial tail (tasks with real dependencies) waits.
//
// Fan-out does not bypass the dependency gate — it selects only tasks
// that are already eligible AND mutually independent. The existing
// per-task dispatch flow (worktree, board, agent launch) is unchanged;
// fan-out wraps it with a semaphore and result collector.
package fanout

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Kampe/Herdforge/pkg/deps"
)

// DefaultParallelism is the hard cap on concurrent agents. The article's
// ceiling is sixteen at once; this matches and is overridable by the caller.
const DefaultParallelism = 16

// DispatchFunc is the per-task dispatch boundary. Production wires this to
// dispatch.Dispatcher.Dispatch; tests inject a fake. Each call gets its own
// worktree and agent — fan-out never shares a checkout between tasks.
type DispatchFunc func(ctx context.Context, taskRef string) (DispatchResult, error)

// DispatchResult is the minimal result fan-out collects from each task.
// The real dispatch.DispatchResult is richer; this is the subset fan-out
// needs for its report. Callers can wrap the real result.
type DispatchResult struct {
	TaskRef  string
	Worktree string
	Branch   string
	Launched bool
	Err      error
}

// Options configure a fan-out run.
type Options struct {
	// Parallelism is the max concurrent dispatches. Defaults to
	// DefaultParallelism when zero.
	Parallelism int
	// Edges is the full set of dependency edges. Fan-out uses only
	// EdgeBlocks to determine mutual independence.
	Edges []deps.DependencyEdge
}

// Report is the structured outcome of a fan-out run.
type Report struct {
	// Dispatched is the list of tasks that were dispatched (or attempted).
	Dispatched []DispatchResult `json:"dispatched"`
	// Succeeded is the count of tasks with nil errors.
	Succeeded int `json:"succeeded"`
	// Failed is the count of tasks with non-nil errors.
	Failed int `json:"failed"`
	// Skipped is the count of tasks that were not independent and were
	// held back for serial dispatch.
	Skipped []string `json:"skipped,omitempty"`
	// Parallelism is the effective concurrency used.
	Parallelism int `json:"parallelism"`
}

// SelectIndependent partitions a set of task refs into two groups:
// independent (no EdgeBlocks between any pair) and dependent (at least
// one EdgeBlocks edge connects them to another ref in the set).
//
// Independent tasks can all fan out simultaneously. Dependent tasks must
// be dispatched serially in dependency order.
func SelectIndependent(refs []string, edges []deps.DependencyEdge) (independent, dependent []string) {
	if len(refs) == 0 {
		return nil, nil
	}
	refSet := make(map[string]bool, len(refs))
	for _, r := range refs {
		refSet[r] = true
	}

	dependentSet := map[string]bool{}
	for _, e := range edges {
		if e.Type != deps.EdgeBlocks {
			continue
		}
		src := strings.TrimSpace(string(e.SourceRef))
		tgt := strings.TrimSpace(string(e.TargetRef))
		if refSet[src] && refSet[tgt] {
			dependentSet[src] = true
			dependentSet[tgt] = true
		}
	}

	for _, r := range refs {
		if dependentSet[r] {
			dependent = append(dependent, r)
		} else {
			independent = append(independent, r)
		}
	}
	sort.Strings(independent)
	sort.Strings(dependent)
	return independent, dependent
}

// Run dispatches independent tasks in parallel, bounded by the concurrency
// semaphore. Tasks that have EdgeBlocks dependencies on other tasks in the
// set are skipped (returned in Report.Skipped) — the caller dispatches
// those serially after the parallel wave completes.

// Run is the fan-out entry point. It never dispatches a task that has an
// unresolved EdgeBlocks dependency on another task in the same batch.
func Run(ctx context.Context, refs []string, dispatch DispatchFunc, opts Options) (*Report, error) {
	if ctx == nil {
		return nil, fmt.Errorf("fanout: nil context")
	}
	if dispatch == nil {
		return nil, fmt.Errorf("fanout: nil dispatch function")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	parallelism := opts.Parallelism
	if parallelism <= 0 {
		parallelism = DefaultParallelism
	}

	independent, skipped := SelectIndependent(refs, opts.Edges)

	rep := &Report{
		Skipped:     skipped,
		Parallelism: parallelism,
	}

	if len(independent) == 0 {
		return rep, nil
	}

	if parallelism > len(independent) {
		parallelism = len(independent)
	}

	type result struct {
		idx int
		res DispatchResult
	}

	sem := make(chan struct{}, parallelism)
	results := make([]DispatchResult, len(independent))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex

	for i, ref := range independent {
		wg.Add(1)
		go func(idx int, taskRef string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[idx] = DispatchResult{TaskRef: taskRef, Err: ctx.Err()}
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("fanout: dispatch %s: %w", taskRef, ctx.Err())
				}
				errMu.Unlock()
				return
			}
			defer func() { <-sem }()

			r, err := dispatch(ctx, taskRef)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("fanout: dispatch %s: %w", taskRef, err)
				}
				errMu.Unlock()
			}
			results[idx] = DispatchResult{
				TaskRef:  taskRef,
				Worktree: r.Worktree,
				Branch:   r.Branch,
				Launched: r.Launched,
				Err:      err,
			}
		}(i, ref)
	}
	wg.Wait()

	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}

	rep.Dispatched = results
	for _, r := range results {
		if r.Err != nil {
			rep.Failed++
		} else {
			rep.Succeeded++
		}
	}

	if firstErr != nil {
		return rep, firstErr
	}
	return rep, nil
}
