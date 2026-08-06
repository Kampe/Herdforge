// Package stop ports bin/herd-stop: bring the herd to rest WITHOUT destroying
// work. It never deletes worktrees or branches.
//
// The safety model is the point. An agent that is mid-edit holds the only copy
// of its uncommitted work, so "stop" asks it to stop and preserves it rather
// than closing its tab. Destroying an active agent's tab is reserved for an
// explicit operator "kill them all" instruction (ForceWorking), and the
// coordinator is protected separately because closing the tab issuing the stop
// would orphan the whole fleet mid-wind-down.
package stop

import "strings"

// Action is what stop decided to do with one agent.
type Action string

const (
	// Close means the agent is settled and its tab can be closed.
	Close Action = "CLOSE"
	// Preserve means the agent is active: a stop was requested, but its tab
	// stays open so uncommitted source work is not destroyed.
	Preserve Action = "PRESERVE"
	// Protect means the coordinator was skipped.
	Protect Action = "PROTECT"
)

// Agent is the subset of fleet state stop reasons about.
type Agent struct {
	Name   string
	Status string
	PaneID string
	TabID  string
}

// Decision is the planned action for one agent.
type Decision struct {
	Agent       Agent
	Action      Action
	RequestStop bool   // send the immediate-stop message to this pane
	Hold        bool   // durably hold a standing lane out of the kick loop
	Reason      string
}

// Options mirror the shell flags.
type Options struct {
	ForceWorking       bool
	IncludeCoordinator bool
	StandingLanes      map[string]bool
}

// IsCoordinator matches the shell's `*orchestrator*` / `coordinator` test.
func IsCoordinator(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(n, "orchestrator") || n == "coordinator"
}

// IsActive reports an agent that may hold uncommitted work in flight.
func IsActive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "working", "starting":
		return true
	}
	return false
}

// Plan decides what to do with each agent. It is pure so the destructive
// half is fully testable without a live fleet, and so --execute and the
// default dry run provably plan the same thing.
func Plan(agents []Agent, opts Options) []Decision {
	out := make([]Decision, 0, len(agents))
	for _, a := range agents {
		if IsCoordinator(a.Name) && !opts.IncludeCoordinator {
			out = append(out, Decision{Agent: a, Action: Protect,
				Reason: "coordinator; use --include-coordinator only after handoff"})
			continue
		}
		d := Decision{Agent: a, Hold: opts.StandingLanes[a.Name]}
		if IsActive(a.Status) {
			d.RequestStop = true
			if !opts.ForceWorking {
				d.Action = Preserve
				d.Reason = "stop requested; active source not destroyed"
				out = append(out, d)
				continue
			}
		}
		d.Action = Close
		out = append(out, d)
	}
	return out
}

// Summary counts a plan.
type Summary struct{ Close, Preserved, Protected int }

func Summarize(plan []Decision) Summary {
	var s Summary
	for _, d := range plan {
		switch d.Action {
		case Close:
			s.Close++
		case Preserve:
			s.Preserved++
		case Protect:
			s.Protected++
		}
	}
	return s
}

// StopMessage is the exact text sent to an active agent.
const StopMessage = "IMMEDIATE STOP from operator. Start nothing else. Preserve source work; report exact branch/HEAD/dirty state, then idle."
