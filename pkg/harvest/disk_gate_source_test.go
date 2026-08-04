package harvest

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// diskAdmissionFixture is a source-only callback recorder. It is used by the
// later unit generation to prove probe/policy/pressure denials invoke zero Git,
// verifier, push, or cleanup callbacks.
type diskAdmissionFixture struct {
	Requests  []resources.DiskRequest
	Decisions []resources.DiskDecision
}

func (f *diskAdmissionFixture) Admit(request resources.DiskRequest) resources.DiskDecision {
	f.Requests = append(f.Requests, request)
	if len(f.Decisions) == 0 {
		return resources.DiskDecision{State: resources.DiskBlocked, Evidence: resources.DiskEvidence{
			Reason: resources.DiskReasonUnavailable, NextAction: resources.DiskActionRetryProbe,
		}}
	}
	decision := f.Decisions[0]
	f.Decisions = f.Decisions[1:]
	return decision
}

func TestDiskAdmissionFixtureHasNoMutationCallbacks(t *testing.T) {
	fixture := &diskAdmissionFixture{}
	decision := fixture.Admit(resources.DiskRequest{Operation: "merge_gate", RequiredBytes: 1, RequiredInodes: 1})
	if decision.Allowed || len(fixture.Requests) != 1 {
		t.Fatalf("fixture = %+v, requests=%d", decision, len(fixture.Requests))
	}
}

// Later single-fault guard identities:
// Independent RED mutants:
//   FAC153-M1 re-add per-goroutine admission in checkUnmergedMode.
//   FAC153-M2 aggregate only the first worktree instead of the complete batch.
//   FAC153-M3 bypass uint64 overflow in batch aggregation.
//   FAC153-M4 move fetch before h.admitHarvestBatch.
//   FAC153-M5 swallow harvest denial/probe/policy failure into HarvestResult.Errors.
//   FAC153-M6 let Integration.Run continue after Harvester.Harvest returns an error.
//   FAC153-M7 replace canonical volume identities with caller path labels.
//   FAC153-M8 collapse CapacityGate hysteresis scopes across volumes.
//   FAC153-M9 remove direct admission from UnmergedFor/UnmergedForStrict.
//   FAC153-M10 map batch tokens by original instead of canonical worktree path.
