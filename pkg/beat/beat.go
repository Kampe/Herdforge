// Package beat bounds a standing lane's exact-action beat.
//
// FAC-705. Observed live: a freshly restarted review-harvest supervisor, whose
// beat was "launch this one exact queued review", instead ran a repository-wide
// `herd drain --json` scan. It burned minutes and had to be killed from the
// coordinator pane. The exact review was never launched.
//
// Nothing was broken. drain is phase-timed and bounded; it did what it was
// asked. The defect is that a broad scan was reachable AT ALL from a beat whose
// action was already known -- an exact queued SHA needs no discovery, and time
// spent discovering is time not spent launching.
//
// Two things this makes mechanical:
//
//   - An exact beat DECLARES itself, so a discovery command can refuse rather
//     than silently succeed slowly. A command that is merely slow cannot be told
//     apart from one that is wrong.
//   - A beat that produces no transition inside its budget emits a STRUCTURED
//     blocked reason and hands control back, instead of occupying the lane.
//     Presence is not activity, which is what this whole class of failure keeps
//     reducing to.
package beat

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// ExactBeatEnv marks the current process as executing an exact-action beat.
const ExactBeatEnv = "HERD_EXACT_BEAT"

// DefaultBudget is how long an exact beat may run without producing a
// transition. Sixty seconds is the operator's stated SLO: a launch, verdict or
// merge either happens inside it, or the lane says why it cannot.
const DefaultBudget = 60 * time.Second

// InExactBeat reports whether the caller is inside a declared exact beat.
func InExactBeat() bool {
	v := strings.TrimSpace(os.Getenv(ExactBeatEnv))
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// RefuseBroadScan returns the refusal a discovery command must emit when it is
// invoked inside an exact beat, or nil when it may proceed.
//
// The message names the alternative, because a refusal that does not name one
// is just a slower way to stall the lane.
func RefuseBroadScan(command, exactAlternative string) error {
	if !InExactBeat() {
		return nil
	}
	return fmt.Errorf("%s is a broad scan and this is an exact-action beat (%s is set): "+
		"the action is already known, so discovery cannot improve it and can only spend the budget. "+
		"Run %s instead, or unset %s if this beat is genuinely exploratory",
		command, ExactBeatEnv, exactAlternative, ExactBeatEnv)
}

// Transition is what an exact beat must produce to count as productive.
type Transition string

const (
	TransitionLaunch  Transition = "launch"
	TransitionVerdict Transition = "verdict"
	TransitionMerge   Transition = "merge"
	// TransitionNone means the beat ended having moved nothing.
	TransitionNone Transition = "none"
)

// Outcome is a beat's structured result. A beat that produced nothing still
// produces THIS, so a lane hands back a reason rather than silence.
type Outcome struct {
	Lane       string     `json:"lane"`
	Transition Transition `json:"transition"`
	Elapsed    string     `json:"elapsed"`
	WithinSLO  bool       `json:"within_slo"`
	// Blocked is required whenever Transition is none. An unexplained
	// unproductive beat cannot be told apart from a hung one.
	Blocked string `json:"blocked,omitempty"`
}

// Close ends a beat and returns its structured outcome.
//
// A beat that moved nothing and names no reason is rejected outright. That is
// the exact shape of an agent occupying a lane while appearing busy, and the
// point of this package is that it must be impossible to report.
func Close(lane string, transition Transition, elapsed, budget time.Duration, blocked string) (Outcome, error) {
	o := Outcome{
		Lane:       lane,
		Transition: transition,
		Elapsed:    elapsed.Round(time.Second).String(),
		WithinSLO:  elapsed <= budget,
		Blocked:    strings.TrimSpace(blocked),
	}
	if transition == TransitionNone && o.Blocked == "" {
		return o, fmt.Errorf("beat for lane %q moved nothing and named no blocker: "+
			"an unproductive beat MUST state why, or it cannot be told apart from a hung one", lane)
	}
	return o, nil
}
