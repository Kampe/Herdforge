package backfill

import (
	"context"
	"fmt"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// FleetGate composes the fleet's real backpressure sources into the single
// Capacity reading the Watcher plans against: lane occupancy, review queue
// depth, host memory headroom, disk admission, and spend budget.
//
// The distinction it exists to preserve is the one a scheduler gets wrong by
// default: a source that says "blocked" and a source that says "I could not
// tell" are NOT the same answer. A real refusal (queue full, volume below
// threshold, budget spent) is honest zero builder capacity. An indeterminate
// probe (capacity unavailable, invalid policy, a verdict string nothing
// produced) returns an error, which the Watcher turns into ErrUnknownState —
// the affected pass is blocked, the cursor does not move, and nothing anywhere
// records that there was no work.
//
// Wiring, in the caller (this package deliberately does not import the fleet's
// supervisors, so the dependency edge stays one-way):
//
//	g := &backfill.FleetGate{
//		Lanes:   func(context.Context) (int, error) { return freeLanes, nil },
//		Reviews: func(context.Context) (int, error) { return sv.PendingCount(), nil },
//		Memory:  func() (string, error) { return resources.TakeSnapshot().Verdict, nil },
//		Budget:  func() (bool, error) { return bm.IsExhausted(), nil },
//		Disk:    capacityGate, // *resources.CapacityGate
//		DiskRequest: resources.DiskRequest{
//			Operation:     "worktree_create",
//			Path:          repoRoot,
//			RequiredBytes: req.RequiredBytes,
//		},
//	}
type FleetGate struct {
	// Lanes reports free builder lanes. Required.
	Lanes func(ctx context.Context) (int, error)
	// Reviews reports how many reviews are pending. Required — review
	// pressure is what the plan drains before launching excess builders.
	Reviews func(ctx context.Context) (int, error)
	// Memory reports a resources verdict (OK/TIGHT/ALERT). Optional; a
	// verdict outside that set is unknown, not permission.
	Memory func() (string, error)
	// Disk admits one builder-launch-shaped request. Optional.
	Disk resources.DiskAdmission
	// DiskRequest is what Disk is asked to admit. Required when Disk is set.
	DiskRequest resources.DiskRequest
	// Budget reports whether spend is exhausted. Optional.
	Budget func() (bool, error)
}

// Capacity implements Gate.
func (g *FleetGate) Capacity(ctx context.Context) (Capacity, error) {
	if g == nil || g.Lanes == nil || g.Reviews == nil {
		return Capacity{}, fmt.Errorf("backfill: fleet gate requires lane and review sources")
	}

	lanes, err := g.Lanes(ctx)
	if err != nil {
		return Capacity{}, fmt.Errorf("free builder lanes: %w", err)
	}
	reviews, err := g.Reviews(ctx)
	if err != nil {
		return Capacity{}, fmt.Errorf("pending reviews: %w", err)
	}
	// A negative count is a broken source, not an empty fleet. Clamping it
	// would launch builders on a reading nobody can defend.
	if lanes < 0 || reviews < 0 {
		return Capacity{}, fmt.Errorf("impossible reading: lanes=%d reviews=%d", lanes, reviews)
	}

	// Every refusal below zeroes BUILDER lanes only. Review drain survives on
	// purpose: draining the review queue is what releases the memory, disk and
	// lanes these gates are refusing over, so blocking it would deadlock the
	// pressure it is reacting to.
	if g.Memory != nil {
		verdict, err := g.Memory()
		if err != nil {
			return Capacity{}, fmt.Errorf("memory verdict: %w", err)
		}
		switch verdict {
		case resources.VerdictOK, resources.VerdictTight:
		case resources.VerdictAlert:
			lanes = 0
		default:
			return Capacity{}, fmt.Errorf("unrecognized memory verdict %q", verdict)
		}
	}

	if g.Disk != nil {
		decision := g.Disk.Admit(g.DiskRequest)
		if !decision.Allowed {
			if diskReasonIsIndeterminate(decision.Evidence.Reason) {
				return Capacity{}, fmt.Errorf("disk admission is indeterminate: %s (next action: %s)",
					decision.Evidence.Reason, decision.Evidence.NextAction)
			}
			lanes = 0
		}
	}

	if g.Budget != nil {
		exhausted, err := g.Budget()
		if err != nil {
			return Capacity{}, fmt.Errorf("budget state: %w", err)
		}
		if exhausted {
			lanes = 0
		}
	}

	return Capacity{FreeBuilderLanes: lanes, PendingReviews: reviews}, nil
}

// diskReasonIsIndeterminate separates "the volume genuinely lacks room" from
// "the probe could not establish the volume's state". Only the first is zero
// capacity; the second is unknown.
func diskReasonIsIndeterminate(reason string) bool {
	switch reason {
	case resources.DiskReasonUnavailable,
		resources.DiskReasonInvalid,
		resources.DiskReasonInvalidPolicy,
		resources.DiskReasonAdditionalUnavailable,
		resources.DiskReasonAdditionalInvalid:
		return true
	}
	return false
}
