package lifecycle

import "testing"

func TestValidTransition_HappyPath(t *testing.T) {
	path := []State{
		StateDraft, StateEligible, StateClaimed, StateDispatched, StateBuilding,
		StateVerifying, StateReviewing, StateIntegrationQueued, StateIntegrated,
		StateReconciled, StateCleaned,
	}
	for i := 0; i < len(path)-1; i++ {
		from, to := path[i], path[i+1]
		if !ValidTransition(from, to) {
			t.Errorf("expected %s -> %s to be valid", from, to)
		}
	}
}

func TestValidTransition_AnyNonTerminalCanRecoverOrBlock(t *testing.T) {
	for _, s := range NonTerminalStates() {
		if !ValidTransition(s, StateRecovering) && s != StateRecovering {
			t.Errorf("expected %s -> Recovering to be valid", s)
		}
		if !ValidTransition(s, StateBlocked) && s != StateBlocked {
			t.Errorf("expected %s -> Blocked to be valid", s)
		}
	}
}

func TestValidTransition_TerminalStateHasNoOutgoing(t *testing.T) {
	if ValidTransition(StateCleaned, StateEligible) {
		t.Error("expected Cleaned to be terminal (no outgoing transitions)")
	}
	if len(transitions[StateCleaned]) != 0 {
		t.Errorf("expected Cleaned to have zero outgoing transitions, got %v", transitions[StateCleaned])
	}
}

func TestValidTransition_RejectsSkippedStates(t *testing.T) {
	cases := []struct{ from, to State }{
		{StateDraft, StateDispatched},
		{StateClaimed, StateReviewing},
		{StateEligible, StateIntegrated},
		{StateBuilding, StateCleaned},
	}
	for _, c := range cases {
		if ValidTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be rejected", c.from, c.to)
		}
	}
}

func TestValidTransition_RejectsUnknownStates(t *testing.T) {
	if ValidTransition(State("bogus"), StateEligible) {
		t.Error("expected unknown from-state to be rejected")
	}
	if ValidTransition(StateDraft, State("bogus")) {
		t.Error("expected unknown to-state to be rejected")
	}
}

func TestValidTransition_ReviewingCanReturnToBuildingOnChangesRequested(t *testing.T) {
	if !ValidTransition(StateReviewing, StateBuilding) {
		t.Error("expected Reviewing -> Building (changes requested) to be valid")
	}
}

func TestValidTransition_RecoveringResumesIntoOwnedStates(t *testing.T) {
	resumable := []State{
		StateEligible, StateClaimed, StateDispatched, StateBuilding,
		StateVerifying, StateReviewing, StateIntegrationQueued, StateBlocked,
	}
	for _, to := range resumable {
		if !ValidTransition(StateRecovering, to) {
			t.Errorf("expected Recovering -> %s to be valid", to)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	if !IsTerminal(StateCleaned) {
		t.Error("expected Cleaned to be terminal")
	}
	if IsTerminal(StateBuilding) {
		t.Error("expected Building to not be terminal")
	}
}
