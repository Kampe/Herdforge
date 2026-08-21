package provider

import (
	"context"
	"sort"
	"sync"
)

// ListActiveTasks returns every non-terminal card without ever reading the
// terminal columns.
//
// An unfiltered ListTasks walks all six columns, and on a real board the
// terminal columns dominate: the FAC board carried 525 done cards across 7
// pages costing ~45s to page through, against 6 active cards. That single
// column exceeded the OpList deadline on its own, so every caller that
// discards done/archived -- deps migrate, dependency fences -- failed with a
// provider timeout for work it was going to throw away.
//
// This deliberately fans out over the single-status API that every adapter
// already implements rather than introducing an "active" sentinel status. A
// sentinel would be silently misread as an unknown status by adapters that
// have not been taught about it, and would return zero tasks instead of
// failing -- an empty active board is indistinguishable from a broken query,
// which is exactly the failure mode a dependency fence must never hit.
//
// Results are sorted by ref so callers get a deterministic order regardless of
// which column returned first.
func ListActiveTasks(ctx context.Context, tp TaskProvider, projectID string) ([]*Task, error) {
	if tp == nil {
		return nil, nil
	}
	statuses := ActiveStatuses()
	per := make([][]*Task, len(statuses))
	errs := make([]error, len(statuses))

	var wg sync.WaitGroup
	for i, status := range statuses {
		wg.Add(1)
		go func(i int, status string) {
			defer wg.Done()
			per[i], errs[i] = tp.ListTasks(ctx, projectID, status)
		}(i, status)
	}
	wg.Wait()

	// Fail closed on the first error in column order so the message is
	// deterministic no matter which goroutine finished first. A partial active
	// set would let a dependency fence pass on missing edges.
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	var out []*Task
	seen := map[string]struct{}{}
	for _, tasks := range per {
		for _, t := range tasks {
			if t == nil || t.ID == "" {
				continue
			}
			// A card that moved columns mid-fan-out can land in two reads.
			if _, dup := seen[t.ID]; dup {
				continue
			}
			seen[t.ID] = struct{}{}
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return CompareRefs(out[i].Ref, out[j].Ref) < 0
	})
	return out, nil
}
