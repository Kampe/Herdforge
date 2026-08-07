package backfill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

func lanes(n int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return n, nil }
}

func reviews(n int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return n, nil }
}

func diskDecision(allowed bool, reason string) resources.DiskAdmissionFunc {
	return func(resources.DiskRequest) resources.DiskDecision {
		state := resources.DiskReady
		if !allowed {
			state = resources.DiskBlocked
		}
		return resources.DiskDecision{
			State:    state,
			Allowed:  allowed,
			Evidence: resources.DiskEvidence{Reason: reason, NextAction: resources.DiskActionRetryProbe},
		}
	}
}

func TestFleetGate_PassesHealthyReadingThrough(t *testing.T) {
	g := &FleetGate{
		Lanes:   lanes(3),
		Reviews: reviews(1),
		Memory:  func() (string, error) { return resources.VerdictOK, nil },
		Disk:    diskDecision(true, resources.DiskReasonNone),
		Budget:  func() (bool, error) { return false, nil },
	}
	got, err := g.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if got != (Capacity{FreeBuilderLanes: 3, PendingReviews: 1}) {
		t.Fatalf("capacity = %+v", got)
	}
}

// TIGHT is a warning, not a refusal — a fleet at 2% free with zero swap is
// macOS steady state (see pkg/resources).
func TestFleetGate_TightMemoryStillLaunches(t *testing.T) {
	g := &FleetGate{Lanes: lanes(2), Reviews: reviews(0), Memory: func() (string, error) { return resources.VerdictTight, nil }}
	got, err := g.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if got.FreeBuilderLanes != 2 {
		t.Fatalf("TIGHT refused builders: %+v", got)
	}
}

// Every real refusal zeroes builders but preserves review drain: draining is
// what releases the pressure being refused over.
func TestFleetGate_RefusalsZeroBuildersAndKeepReviewDrain(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate *FleetGate
	}{
		{"memory alert", &FleetGate{Lanes: lanes(4), Reviews: reviews(2), Memory: func() (string, error) { return resources.VerdictAlert, nil }}},
		{"disk below threshold", &FleetGate{Lanes: lanes(4), Reviews: reviews(2), Disk: diskDecision(false, resources.DiskReasonBelowThreshold)}},
		{"disk hysteresis", &FleetGate{Lanes: lanes(4), Reviews: reviews(2), Disk: diskDecision(false, resources.DiskReasonHysteresis)}},
		{"inode exhaustion", &FleetGate{Lanes: lanes(4), Reviews: reviews(2), Disk: diskDecision(false, resources.DiskReasonInodeExhaustion)}},
		{"budget exhausted", &FleetGate{Lanes: lanes(4), Reviews: reviews(2), Budget: func() (bool, error) { return true, nil }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.gate.Capacity(context.Background())
			if err != nil {
				t.Fatalf("a real refusal must be honest zero, not unknown: %v", err)
			}
			if got.FreeBuilderLanes != 0 {
				t.Errorf("FreeBuilderLanes = %d, want 0", got.FreeBuilderLanes)
			}
			if got.PendingReviews != 2 {
				t.Errorf("review drain was suppressed: %+v", got)
			}
		})
	}
}

// An indeterminate probe is unknown, never zero.
func TestFleetGate_IndeterminateSourcesAreUnknownNotZero(t *testing.T) {
	boom := errors.New("probe exploded")
	for _, tc := range []struct {
		name string
		gate *FleetGate
		want string
	}{
		{"lane source failed", &FleetGate{Lanes: func(context.Context) (int, error) { return 0, boom }, Reviews: reviews(0)}, "free builder lanes"},
		{"review source failed", &FleetGate{Lanes: lanes(1), Reviews: func(context.Context) (int, error) { return 0, boom }}, "pending reviews"},
		{"negative lanes", &FleetGate{Lanes: lanes(-1), Reviews: reviews(0)}, "impossible reading"},
		{"negative reviews", &FleetGate{Lanes: lanes(1), Reviews: reviews(-2)}, "impossible reading"},
		{"memory probe failed", &FleetGate{Lanes: lanes(1), Reviews: reviews(0), Memory: func() (string, error) { return "", boom }}, "memory verdict"},
		{"unknown verdict string", &FleetGate{Lanes: lanes(1), Reviews: reviews(0), Memory: func() (string, error) { return "MAYBE", nil }}, "unrecognized memory verdict"},
		{"disk capacity unavailable", &FleetGate{Lanes: lanes(1), Reviews: reviews(0), Disk: diskDecision(false, resources.DiskReasonUnavailable)}, "indeterminate"},
		{"disk request invalid", &FleetGate{Lanes: lanes(1), Reviews: reviews(0), Disk: diskDecision(false, resources.DiskReasonInvalid)}, "indeterminate"},
		{"disk policy invalid", &FleetGate{Lanes: lanes(1), Reviews: reviews(0), Disk: diskDecision(false, resources.DiskReasonInvalidPolicy)}, "indeterminate"},
		{"additional volume unavailable", &FleetGate{Lanes: lanes(1), Reviews: reviews(0), Disk: diskDecision(false, resources.DiskReasonAdditionalUnavailable)}, "indeterminate"},
		{"budget state failed", &FleetGate{Lanes: lanes(1), Reviews: reviews(0), Budget: func() (bool, error) { return false, boom }}, "budget state"},
		{"no sources", &FleetGate{}, "requires lane and review sources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.gate.Capacity(context.Background())
			if err == nil {
				t.Fatalf("indeterminate source reported capacity %+v", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the source (%q)", err, tc.want)
			}
			if got != (Capacity{}) {
				t.Fatalf("a failed gate returned a usable capacity: %+v", got)
			}
		})
	}
}

// The composed gate must reach the Watcher as ErrUnknownState: work stays
// pending, the cursor does not move, and the same events run once the probe
// recovers.
func TestFleetGate_IndeterminateDiskBlocksTheWatcher(t *testing.T) {
	h := newHarness(t, Capacity{})
	unavailable := true
	h.w.Gate = &FleetGate{
		Lanes:   lanes(2),
		Reviews: reviews(0),
		Disk: resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
			if unavailable {
				return diskDecision(false, resources.DiskReasonUnavailable)(resources.DiskRequest{})
			}
			return diskDecision(true, resources.DiskReasonNone)(resources.DiskRequest{})
		}),
	}
	h.source.append(Event{Sequence: 5, Repo: "herdforge", TaskRef: "FAC-9", State: "verified"})

	if _, err := h.w.Step(context.Background()); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("indeterminate disk did not block the watcher: %v", err)
	}
	if st := h.w.Stats(); st.LastSequence != 0 || !st.Unknown {
		t.Fatalf("blocked pass moved the cursor or hid the unknown: %+v", st)
	}
	if plans := h.exec.snapshot(); len(plans) != 0 {
		t.Fatalf("executed under an indeterminate disk probe: %+v", plans)
	}

	unavailable = false
	h.clock.advance(time.Hour)
	res := h.step(t)
	if !res.Executed || res.Plan.HighestSequence != 5 || res.Plan.LaunchBuilders != 2 {
		t.Fatalf("recovered probe did not release the pending work: %+v", res)
	}
}
