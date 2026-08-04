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
//   FAC153-M1 remove Harvester.admitDisk before fetch or goroutine fan-out.
//   FAC153-M2 remove Integration.admitMergeDisk before prepareMain.
//   FAC153-M3 zero worktree_create headroom.
//   FAC153-M4 replace canonical ResolveExistingPath identities with caller labels.
//   FAC153-M5 allow invalid policy/probe/pressure decisions to continue.
//   FAC153-M6 collapse CapacityGate scopes across volumes.
