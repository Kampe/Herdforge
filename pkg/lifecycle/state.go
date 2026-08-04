package lifecycle

// State is a canonical task-lifecycle state. Every mutating verb (pulse,
// dispatch, daemon, forge, review, approve, harvest, cleanup) advances a
// task through these states via Machine.Transition instead of a
// command-local shortcut.
type State string

const (
	StateDraft             State = "draft"
	StateEligible          State = "eligible"
	StateClaimed           State = "claimed"
	StateDispatched        State = "dispatched"
	StateBuilding          State = "building"
	StateVerifying         State = "verifying"
	StateReviewing         State = "reviewing"
	StateIntegrationQueued State = "integration_queued"
	StateIntegrated        State = "integrated"
	StateReconciled        State = "reconciled"
	StateCleaned           State = "cleaned"
	StateRecovering        State = "recovering"
	StateBlocked           State = "blocked"
)

// transitions is the canonical edge set. Every non-terminal state can also
// fold into Recovering or Blocked (added by init) so a crash or fencing
// failure at any external boundary always has a durable, explicit landing
// state instead of an undefined one.
var transitions = map[State][]State{
	StateDraft:             {StateEligible},
	StateEligible:          {StateClaimed},
	StateClaimed:           {StateDispatched},
	StateDispatched:        {StateBuilding},
	StateBuilding:          {StateVerifying},
	StateVerifying:         {StateReviewing, StateBuilding},
	StateReviewing:         {StateIntegrationQueued, StateBuilding},
	StateIntegrationQueued: {StateIntegrated},
	StateIntegrated:        {StateReconciled},
	StateReconciled:        {StateCleaned},
	StateCleaned:           {},
	// Recovering resumes into whatever state the reconciler determines the
	// durable evidence supports.
	StateRecovering: {
		StateEligible, StateClaimed, StateDispatched, StateBuilding,
		StateVerifying, StateReviewing, StateIntegrationQueued, StateIntegrated,
		StateReconciled, StateBlocked,
	},
	// Blocked can be lifted back to Recovering (retry), reset to Eligible
	// (operator override), or abandoned via Cleaned.
	StateBlocked: {StateRecovering, StateEligible, StateCleaned},
}

func init() {
	for _, s := range NonTerminalStates() {
		if s == StateRecovering || s == StateBlocked {
			continue
		}
		transitions[s] = appendMissing(transitions[s], StateRecovering, StateBlocked)
	}
}

func appendMissing(edges []State, add ...State) []State {
	for _, a := range add {
		found := false
		for _, e := range edges {
			if e == a {
				found = true
				break
			}
		}
		if !found {
			edges = append(edges, a)
		}
	}
	return edges
}

// allStates is the full, ordered state set, used for validation and
// terminal-state derivation.
var allStates = []State{
	StateDraft, StateEligible, StateClaimed, StateDispatched, StateBuilding,
	StateVerifying, StateReviewing, StateIntegrationQueued, StateIntegrated,
	StateReconciled, StateCleaned, StateRecovering, StateBlocked,
}

func isKnownState(s State) bool {
	for _, k := range allStates {
		if k == s {
			return true
		}
	}
	return false
}

// IsTerminal reports whether a state has no valid outgoing transitions.
func IsTerminal(s State) bool {
	edges, ok := transitions[s]
	return ok && len(edges) == 0
}

// NonTerminalStates returns every state that is not terminal.
func NonTerminalStates() []State {
	out := make([]State, 0, len(allStates))
	for _, s := range allStates {
		if !IsTerminal(s) {
			out = append(out, s)
		}
	}
	return out
}

// ValidTransition reports whether to is a legal next state from from.
// Unknown states are always rejected (fail-closed).
func ValidTransition(from, to State) bool {
	if !isKnownState(from) || !isKnownState(to) {
		return false
	}
	for _, edge := range transitions[from] {
		if edge == to {
			return true
		}
	}
	return false
}
