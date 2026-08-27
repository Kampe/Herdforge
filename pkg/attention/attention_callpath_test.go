package attention

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// callPathReader is a hold reader that admits everything and supplies the
// generation source RunWithHoldReaderAndTasks requires.
type callPathReader struct{}

func (callPathReader) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{}, nil
}

func (callPathReader) CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) {
	return 1, nil
}

// fleetOf is the injected census: exactly these agents are live.
func fleetOf(names ...string) func() ([]kick.AgentEntry, error) {
	return func() ([]kick.AgentEntry, error) {
		out := make([]kick.AgentEntry, 0, len(names))
		for _, n := range names {
			out = append(out, kick.AgentEntry{Name: n, Status: "working"})
		}
		return out, nil
	}
}

// FAC-593 (review repair): the first attempt at this pin called degradedReason
// directly. That test stayed GREEN with the shipped call site reverted, so it
// pinned the helper and not the behaviour an operator triggers -- vacuous in
// exactly the shape FAC-578 exists to prevent.
//
// This drives the real entry point, RunWithHoldReaderAndTasks, with the live
// agent list and standing roster stubbed at their existing seams. Reverting
// `degraded[name] = degradedReason(err)` back to the unconditional
// "hold authority unavailable: " prefix turns this RED.
func TestAmbiguousBindingReachesTheReportThroughTheRealCallPath(t *testing.T) {
	kick.SetStandingOverride([]string{"forge-docs-custodian"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "docs-custodian", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(context.Context, string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: "docs-custodian", Task: "CHA-1863", Scope: "task"},
			{Repository: "repo", Owner: "worker", Lane: "docs-custodian", Task: "CHA-1784", Scope: "task"},
		}, nil
	}

	result, err := runWithFleet(fleetOf("forge-docs-custodian"), callPathReader{}, "repo", resolver, registry)
	if err != nil {
		t.Fatalf("attention run: %v", err)
	}
	if result == nil || len(result.Items) == 0 {
		t.Fatal("the scan produced no items, so nothing was pinned")
	}

	var reason string
	var found bool
	for _, item := range result.Items {
		if strings.Contains(item.Name, "docs-custodian") {
			reason, found = item.Reason, true
			if item.Level != LevelCritical {
				t.Fatalf("an ambiguous lane is a failed beat, not a resting one: level=%s", item.Level)
			}
		}
	}
	if !found {
		t.Fatalf("the ambiguous lane never reached the report: %+v", result.Items)
	}

	// The shipped behaviour: the binding error is self-describing and must
	// reach the operator intact.
	if strings.Contains(reason, "hold authority unavailable") {
		t.Fatalf("ambiguous binding reported as an infrastructure failure: %s", reason)
	}
	for _, want := range []string{"CHA-1784", "CHA-1863", "docs-custodian"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("report does not name %q: %s", want, reason)
		}
	}
}

// The other direction, also through the real call path: a genuine authority
// failure must KEEP its label, so the fix cannot be "delete the prefix".
func TestGenuineAuthorityFailureKeepsItsLabelThroughTheRealCallPath(t *testing.T) {
	kick.SetStandingOverride([]string{"forge-docs-custodian"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "docs-custodian", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	resolver := func(context.Context, string) ([]lifecycle.HoldIdentity, error) {
		return nil, errors.New("board unreachable")
	}

	result, err := runWithFleet(fleetOf("forge-docs-custodian"), callPathReader{}, "repo", resolver, registry)
	if err != nil {
		t.Fatalf("attention run: %v", err)
	}
	for _, item := range result.Items {
		if !strings.Contains(item.Name, "docs-custodian") {
			continue
		}
		// A resolver failure is NOT ambiguity (FAC-702) and it is NOT an
		// authority failure either -- it must say so.
		if !strings.Contains(item.Reason, "NOT an ambiguous lane") {
			t.Fatalf("resolver failure lost its discriminator: %s", item.Reason)
		}
		return
	}
	t.Fatal("the degraded lane never reached the report")
}
