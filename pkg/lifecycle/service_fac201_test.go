package lifecycle

import (
	"context"
	"errors"
	"testing"
)

var errScopeGateRefused = errors.New("fac-201: fixture admission scope refused")

func failingScopeGate(ctx context.Context, candidateID, candidateSHA string) error {
	return errScopeGateRefused
}

// TestServicePromotePullRequest_ScopeGateFailsClosed proves FAC-201's core
// wiring requirement: before an irreversible PR promotion, a configured
// scope gate is consulted and a refusal blocks promotion outright.
func TestServicePromotePullRequest_ScopeGateFailsClosed(t *testing.T) {
	fx := newServiceFixture(t, WithScopeGate(failingScopeGate))
	const task = "FAC-201-PROMOTE"
	fx.own(task)
	fx.approvePlan(task)
	fx.start(task)
	c := fx.prepareReviewed(task, "a")

	_, err := fx.service.PromotePullRequest(context.Background(), PromotePRRequest{
		Command: fx.command(task, task+":promote:a", fx.agent), CandidateID: c.id,
		CandidateSHA: c.sha, PullRequest: "pr://fac-201/a",
	})
	if !errors.Is(err, errScopeGateRefused) {
		t.Fatalf("err = %v, want scope gate refusal", err)
	}

	// The refusal must not have advanced state: promotion is retryable once
	// the scope gate would pass (proves the failed attempt left no partial
	// transition behind).
	fx.service.scopeGate = nil
	if _, err := fx.service.PromotePullRequest(context.Background(), PromotePRRequest{
		Command: fx.command(task, task+":promote:retry", fx.agent), CandidateID: c.id,
		CandidateSHA: c.sha, PullRequest: "pr://fac-201/a",
	}); err != nil {
		t.Fatalf("retry after clearing scope gate: %v", err)
	}
}

// TestServiceBeginIntegration_ScopeGateFailsClosed proves the same gate is
// consulted again before integration begins (merge), not only at PR
// promotion — a target-branch advance or force-push between promotion and
// merge must still be caught (FAC-201 "recompute and read back... before
// push, PR creation/update, reviewer delivery, merge, and Done").
func TestServiceBeginIntegration_ScopeGateFailsClosed(t *testing.T) {
	fx := newServiceFixture(t, WithScopeGate(failingScopeGate))
	const task = "FAC-201-INTEGRATE"
	fx.own(task)
	fx.approvePlan(task)
	fx.start(task)
	c := fx.prepareReviewed(task, "a")
	fx.service.scopeGate = nil // let promotion through so the gate under test is BeginIntegration's
	fx.promote(task, "a", c)
	fx.service.scopeGate = failingScopeGate
	approval := fx.approveMerge(task, c, "approved", "fac-201-approval", fx.human)

	_, err := fx.service.BeginIntegration(context.Background(), BeginIntegrationRequest{
		Command: fx.command(task, task+":integrate", fx.agent), CandidateID: c.id,
		CandidateSHA: c.sha, ApprovalID: approval.ID, TargetBranch: "main",
	})
	if !errors.Is(err, errScopeGateRefused) {
		t.Fatalf("err = %v, want scope gate refusal", err)
	}
}

// TestServiceScopeGate_NilIsNoOp proves existing callers that never opt into
// WithScopeGate see no behavior change: the full promote-approve-integrate
// path still works with no gate configured.
func TestServiceScopeGate_NilIsNoOp(t *testing.T) {
	fx := newServiceFixture(t)
	const task = "FAC-201-NILGATE"
	c := fx.prepareQueued(task, "a")
	approval := fx.approveMerge(task, c, "approved", "fac-201-nilgate-approval", fx.human)
	if _, err := fx.service.BeginIntegration(context.Background(), BeginIntegrationRequest{
		Command: fx.command(task, task+":integrate", fx.agent), CandidateID: c.id,
		CandidateSHA: c.sha, ApprovalID: approval.ID, TargetBranch: "main",
	}); err != nil {
		t.Fatalf("begin integration with no scope gate configured: %v", err)
	}
}
