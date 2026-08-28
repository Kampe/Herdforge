package spin

// FAC-90: durable progress assessment.
//
// The fingerprint detector in spin.go answers "did this pane's text change".
// That is a diagnostic, not a progress measure: a pane can redraw forever
// while the task stands still, and a pane can sit silent while a tool call
// runs for ten minutes. So the authority for "is this agent making durable
// progress" is this file, and it reads only durable, non-textual evidence —
// lifecycle event sequence, candidate SHA, herdr's own state-change counter,
// git HEAD and dirty count. Terminal text enters only as Observation.Findings
// and Observation.Diagnostic, which may name a cause but can never establish
// progress and can never authorize an act.

import (
	"fmt"
	"strings"
	"time"
)

// Tri is a three-valued answer. Unknown is not No: every gate in this file
// treats Unknown as the unsafe case, because "we could not tell" and "there
// is nothing there" are the exact pair a recovery action must not confuse.
type Tri string

const (
	TriYes     Tri = "yes"
	TriNo      Tri = "no"
	TriUnknown Tri = "unknown"
)

// Progress is the durable evidence that an agent moved. Every field comes
// from a source that only advances when work actually happened.
type Progress struct {
	// LifecycleSeq and LifecycleState come from the FAC-119/120 lifecycle
	// event store: the task's materialized sequence number and state.
	LifecycleSeq   int64  `json:"lifecycle_seq,omitempty"`
	LifecycleState string `json:"lifecycle_state,omitempty"`
	// CandidateSHA is the task's recorded candidate commit.
	CandidateSHA string `json:"candidate_sha,omitempty"`
	// StateChangeSeq is herdr's own monotonic agent state counter — the
	// closest thing to "the harness did something" that is not screen text.
	StateChangeSeq uint64 `json:"state_change_seq,omitempty"`
	// Head and Dirty are the worktree git snapshot.
	Head  string `json:"head,omitempty"`
	Dirty int    `json:"dirty,omitempty"`
	// Continuations is the durable stop-hook continuation counter
	// (goalguard.Goal.Continuations) for this lane. FAC-628: a lane in an
	// EMPTY continuation loop increments this every cycle while producing no
	// output change, no lifecycle movement, and no git delta -- it reports
	// agent_status=working in every census while doing nothing, consuming a
	// concurrency slot and quota. It is deliberately NOT one of the "moved"
	// signals below: an advancing continuation count with nothing else
	// moving is the ABSENCE of progress, not evidence of it.
	Continuations int64 `json:"continuations,omitempty"`
}

// Known reports whether at least one progress signal is actually observable.
// Dirty is deliberately excluded: 0 is indistinguishable from "not measured",
// so it can never on its own make an observation trustworthy.
//
// When no signal is known, no-progress is unprovable and Assess fails closed.
func (p Progress) Known() bool {
	return p.LifecycleSeq > 0 || p.CandidateSHA != "" || p.StateChangeSeq > 0 || p.Head != ""
}

// moved returns the names of the signals that advanced between prev and now.
// An empty result means nothing durable moved.
func moved(prev, now Progress) []string {
	var out []string
	if now.LifecycleSeq > prev.LifecycleSeq {
		out = append(out, fmt.Sprintf("lifecycle_seq %d->%d", prev.LifecycleSeq, now.LifecycleSeq))
	}
	if now.LifecycleState != prev.LifecycleState && now.LifecycleState != "" {
		out = append(out, fmt.Sprintf("lifecycle_state %q->%q", prev.LifecycleState, now.LifecycleState))
	}
	if now.CandidateSHA != prev.CandidateSHA && now.CandidateSHA != "" {
		out = append(out, "candidate_sha changed")
	}
	if now.StateChangeSeq > prev.StateChangeSeq {
		out = append(out, fmt.Sprintf("state_change_seq %d->%d", prev.StateChangeSeq, now.StateChangeSeq))
	}
	if now.Head != prev.Head && now.Head != "" {
		out = append(out, "head advanced")
	}
	if now.Dirty != prev.Dirty {
		out = append(out, fmt.Sprintf("dirty %d->%d", prev.Dirty, now.Dirty))
	}
	return out
}

// Observation is one sample of a live agent, gathered from durable sources.
type Observation struct {
	PaneID      string   `json:"pane_id"`
	Name        string   `json:"name"`
	AgentStatus string   `json:"agent_status"`
	Progress    Progress `json:"progress"`
	// PID is the agent's foreground process. 0 means not observed.
	PID int `json:"pid,omitempty"`
	// ProcAlive answers "is the agent process observably running". Unknown
	// blocks every act: an unattributable nudge is worse than none.
	ProcAlive Tri `json:"proc_alive"`
	// UniqueWork answers "does this worktree hold commits not upstream".
	// Anything but TriNo forbids a recovery transition.
	UniqueWork Tri `json:"unique_work"`
	// RecoveryAvailable reports whether a recovery transition can actually
	// be recorded for this agent — i.e. a durable lifecycle task state
	// exists to transition. When it does not, the recovery escalates to the
	// operator rather than booking act budget for something that cannot
	// happen.
	RecoveryAvailable bool `json:"recovery_available"`
	// Diagnostic is a text-derived classification (e.g. process.Quota).
	// Advisory only.
	Diagnostic string `json:"diagnostic,omitempty"`
	// Findings are the fingerprint detector's STALL/SPIN/LONG output,
	// carried for the operator. They never drive Cause or NextAction.
	Findings []Finding `json:"findings,omitempty"`
	// RecordedModel is the launch receipt pin; LiveModel is parsed from the
	// pane's own model identification. A mismatch is a hard stop.
	RecordedModel string `json:"recorded_model,omitempty"`
	LiveModel     string `json:"live_model,omitempty"`
}

// Cause is why an agent looks the way it does. These are mutually exclusive
// by construction; the ordering in Assess encodes their priority.
type Cause string

const (
	CauseProgressing   Cause = "PROGRESSING"
	CauseSlowWork      Cause = "SLOW_WORK"
	CauseAwaitingInput Cause = "AWAITING_INPUT"
	CauseBlocked       Cause = "BLOCKED"
	CauseRateLimited   Cause = "RATE_LIMITED"
	CauseCrashLoop     Cause = "CRASH_LOOP"
	CauseNoProgress    Cause = "NO_PROGRESS"
	CauseUnknownState  Cause = "UNKNOWN_STATE"
	// CauseEmptyLoop is a lane whose stop-hook continuation counter is
	// advancing while no other durable signal moves (FAC-628). Distinct from
	// CauseNoProgress: silence alone is ambiguous (a slow tool call looks
	// identical for a few cycles), but a climbing continuation count with
	// nothing else moving is direct, immediate proof the hook keeps firing
	// on nothing -- it does not wait for Policy.NoProgressCycles.
	CauseEmptyLoop  Cause = "EMPTY_LOOP"
	CauseModelDrift Cause = "MODEL_DRIFT"
)

// Action is the bounded next step.
type Action string

const (
	// ActionNone means nothing is wrong enough to touch.
	ActionNone Action = "none"
	// ActionObserve means keep sampling; we cannot yet justify a side effect.
	ActionObserve Action = "observe"
	// ActionNudge re-prompts the agent. Attributable and reversible.
	ActionNudge Action = "nudge"
	// ActionRecover folds the task into the lifecycle Recovering state.
	ActionRecover Action = "recover"
	// ActionOperator escalates to a human. Never performed automatically —
	// this is where a recovery lands when a fail-closed gate refuses it.
	ActionOperator Action = "operator"
)

// Policy tunes detection and bounds how often spin may act.
type Policy struct {
	// NoProgressCycles is how many consecutive samples with zero durable
	// progress are required before NO_PROGRESS is declared.
	NoProgressCycles int
	// RestartCycles is how many PID changes without progress declare a
	// crash loop.
	RestartCycles int
	// ActCooldown is the minimum gap between two acts on one pane.
	ActCooldown time.Duration
	// ActWindow / ActsPerWindow bound the total acts on one pane.
	ActWindow     time.Duration
	ActsPerWindow int
}

// DefaultPolicy is deliberately conservative: three quiet samples at the
// 180s default interval is nine minutes of provable stillness before spin
// says a word, and at most two acts an hour on any one pane.
func DefaultPolicy() Policy {
	return Policy{
		NoProgressCycles: 3,
		RestartCycles:    2,
		ActCooldown:      15 * time.Minute,
		ActWindow:        time.Hour,
		ActsPerWindow:    2,
	}
}

// Assessment is the structured verdict for one agent. Evidence names every
// signal the verdict rests on, so an operator never has to trust the label.
type Assessment struct {
	PaneID           string    `json:"pane_id"`
	Name             string    `json:"name"`
	Cause            Cause     `json:"cause"`
	Evidence         []string  `json:"evidence"`
	NextAction       Action    `json:"next_action"`
	Permitted        bool      `json:"permitted"`
	Acted            bool      `json:"acted"`
	Withheld         string    `json:"withheld,omitempty"`
	NoProgressCycles int       `json:"no_progress_cycles"`
	RestartCycles    int       `json:"restart_cycles"`
	Diagnostics      []Finding `json:"diagnostics,omitempty"`
}

// Assess folds one observation into the prior sample and returns both the
// sample to persist and the verdict to report.
//
// now is passed in rather than read from the clock so fixtures can drive
// cooldowns and windows deterministically.
//
// act selects report-only (false) or bounded-action (true) mode. Assess never
// performs the action itself — it decides and books the budget; the caller
// executes. Booking before execution is deliberate: a failed nudge still
// consumes its slot, because retrying a side effect we cannot confirm is how
// a rate limit becomes decorative.
func Assess(prev *Sample, obs Observation, pol Policy, now time.Time, act bool) (Sample, Assessment) {
	out := Sample{
		PaneID:      obs.PaneID,
		Name:        obs.Name,
		AgentStatus: obs.AgentStatus,
		Progress:    obs.Progress,
		PID:         obs.PID,
		Head:        obs.Progress.Head,
		Dirty:       obs.Progress.Dirty,
	}
	a := Assessment{
		PaneID:      obs.PaneID,
		Name:        obs.Name,
		NextAction:  ActionNone,
		Diagnostics: obs.Findings,
	}

	// Carry the durable budget forward first: it must survive both a
	// restart (it is persisted with the sample) and every early return
	// below, or a fail-closed path would silently refund a spent act.
	if prev != nil {
		out.ActsUnix = trimActs(prev.ActsUnix, now, pol.ActWindow)
		out.LastActionTaken = prev.LastActionTaken
	}

	switch {
	case prev == nil:
		a.Evidence = append(a.Evidence, "first observation of this pane")
	default:
		if advanced := moved(prev.Progress, obs.Progress); len(advanced) > 0 {
			a.Evidence = append(a.Evidence, "progressed: "+strings.Join(advanced, ", "))
		} else {
			out.NoProgressCycles = prev.NoProgressCycles + 1
			a.Evidence = append(a.Evidence, fmt.Sprintf(
				"no durable progress: lifecycle_seq=%d candidate_sha=%s state_change_seq=%d head=%s dirty=%d unchanged",
				obs.Progress.LifecycleSeq, shortSHA(obs.Progress.CandidateSHA),
				obs.Progress.StateChangeSeq, shortSHA(obs.Progress.Head), obs.Progress.Dirty))
			if prev.PID != 0 && obs.PID != 0 && prev.PID != obs.PID {
				out.RestartCycles = prev.RestartCycles + 1
				a.Evidence = append(a.Evidence,
					fmt.Sprintf("agent process restarted: pid %d->%d", prev.PID, obs.PID))
			} else {
				out.RestartCycles = prev.RestartCycles
			}
		}
	}
	a.NoProgressCycles = out.NoProgressCycles
	a.RestartCycles = out.RestartCycles

	// --- fail-closed gates -------------------------------------------------
	// These run before any classification, because a verdict built on
	// evidence we do not actually have is worse than admitting the gap.
	switch {
	case obs.RecordedModel != "" && obs.LiveModel != "" && !strings.EqualFold(obs.RecordedModel, obs.LiveModel):
		a.Cause = CauseModelDrift
		a.NextAction = ActionOperator
		a.Withheld = "model drift is a hard stop; do not nudge or relaunch this lane"
		a.Evidence = append(a.Evidence, fmt.Sprintf("live model drift: launch receipt=%q live pane=%q", obs.RecordedModel, obs.LiveModel))
		out.LastActionTaken = ""
		return out, a
	case obs.ProcAlive == TriUnknown || obs.ProcAlive == "":
		a.Cause = CauseUnknownState
		a.NextAction = ActionObserve
		a.Withheld = "process state unknown"
		a.Evidence = append(a.Evidence, "process liveness could not be observed")
		out.LastActionTaken = ""
		return out, a
	case !obs.Progress.Known():
		a.Cause = CauseUnknownState
		a.NextAction = ActionObserve
		a.Withheld = "no durable progress signal available"
		a.Evidence = append(a.Evidence,
			"no lifecycle seq, candidate sha, state_change_seq or head to measure")
		out.LastActionTaken = ""
		return out, a
	}

	// --- classification ----------------------------------------------------
	status := strings.ToLower(strings.TrimSpace(obs.AgentStatus))
	switch {
	case status == "blocked":
		a.Cause = CauseBlocked
		a.Evidence = append(a.Evidence, "herdr agent_status=blocked")
	case status == "idle" || status == "done":
		a.Cause = CauseAwaitingInput
		a.Evidence = append(a.Evidence, "herdr agent_status="+status+" — not consuming a turn")
	case status == "starting":
		a.Cause = CauseSlowWork
		a.Evidence = append(a.Evidence, "herdr agent_status=starting")
	case status != "working":
		a.Cause = CauseUnknownState
		a.NextAction = ActionObserve
		a.Withheld = "unrecognized agent status " + fmt.Sprintf("%q", obs.AgentStatus)
		out.LastActionTaken = ""
		return out, a
	case out.NoProgressCycles == 0:
		a.Cause = CauseProgressing
	case out.RestartCycles >= pol.RestartCycles:
		a.Cause = CauseCrashLoop
		a.NextAction = ActionRecover
	case prev != nil && obs.Progress.Continuations > prev.Progress.Continuations:
		// FAC-628: the stop-hook fired and consumed a continuation, and
		// NOTHING else durable moved this cycle (out.NoProgressCycles > 0 is
		// guaranteed here, since CauseProgressing already claimed the
		// zero case above). This is stronger evidence than silence: it
		// proves the loop is actively re-triggering on an unchanged state,
		// not merely slow. Fires immediately rather than waiting for
		// Policy.NoProgressCycles quiet samples.
		a.Cause = CauseEmptyLoop
		a.Evidence = append(a.Evidence, fmt.Sprintf(
			"stop-hook continuation advanced %d->%d with no other durable signal moving",
			prev.Progress.Continuations, obs.Progress.Continuations))
		a.NextAction = ActionRecover
	case obs.Diagnostic == "QUOTA":
		// Provider exhaustion burns a turn per nudge and fixes nothing;
		// the only correct move is to wait for the reset.
		a.Cause = CauseRateLimited
		a.Evidence = append(a.Evidence, "diagnostic: provider quota exhaustion (advisory)")
	case out.NoProgressCycles >= pol.NoProgressCycles:
		a.Cause = CauseNoProgress
		if prev != nil && prev.LastActionTaken == ActionNudge {
			a.Evidence = append(a.Evidence, "a nudge was already delivered and progress still did not resume")
			a.NextAction = ActionRecover
		} else {
			a.NextAction = ActionNudge
		}
	default:
		a.Cause = CauseSlowWork
		a.Evidence = append(a.Evidence, fmt.Sprintf(
			"%d/%d quiet samples — below the no-progress threshold",
			out.NoProgressCycles, pol.NoProgressCycles))
	}

	if a.NextAction == ActionNone || a.NextAction == ActionObserve {
		if a.Cause == CauseProgressing || a.Cause == CauseSlowWork {
			// Live work resets the escalation ladder: the next stall starts
			// from a nudge again, not from where the last one left off.
			out.LastActionTaken = ""
		}
		return out, a
	}

	// --- act gate ----------------------------------------------------------
	if a.NextAction == ActionRecover {
		switch {
		case obs.UniqueWork != TriNo:
			a.NextAction = ActionOperator
			a.Withheld = "unique work is " + string(triOrUnknown(obs.UniqueWork)) +
				"; a recovery transition may release a lease or end a session holding the only copy"
			a.Evidence = append(a.Evidence, "unique_work="+string(triOrUnknown(obs.UniqueWork)))
			return out, a
		case !obs.RecoveryAvailable:
			a.NextAction = ActionOperator
			a.Withheld = "no durable lifecycle task state; a recovery transition cannot be recorded"
			return out, a
		}
	}

	if ok, why := budgetAllows(out.ActsUnix, now, pol); !ok {
		a.Permitted = false
		a.Withheld = why
		return out, a
	}
	a.Permitted = true

	if act {
		out.ActsUnix = append(out.ActsUnix, now.Unix())
		out.LastActionTaken = a.NextAction
		a.Acted = true
	}
	return out, a
}

func triOrUnknown(t Tri) Tri {
	if t == "" {
		return TriUnknown
	}
	return t
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "none"
	}
	return s
}

// trimActs drops act timestamps that have aged out of the rate-limit window.
// It also drops timestamps in the future, which is the shape a clock jump or
// a hand-edited state file takes; keeping them would let a forged future
// stamp suppress every act forever.
func trimActs(acts []int64, now time.Time, window time.Duration) []int64 {
	if window <= 0 {
		return nil
	}
	cutoff := now.Add(-window).Unix()
	out := make([]int64, 0, len(acts))
	for _, ts := range acts {
		if ts >= cutoff && ts <= now.Unix() {
			out = append(out, ts)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// budgetAllows enforces the per-pane rate limit. The timestamps it reads are
// persisted with the sample, so the limit is not reset by restarting herd.
func budgetAllows(acts []int64, now time.Time, pol Policy) (bool, string) {
	if pol.ActsPerWindow <= 0 {
		return false, "policy permits no acts"
	}
	if len(acts) >= pol.ActsPerWindow {
		return false, fmt.Sprintf("rate limit: %d acts already taken in the last %s (max %d)",
			len(acts), pol.ActWindow, pol.ActsPerWindow)
	}
	if len(acts) > 0 && pol.ActCooldown > 0 {
		last := time.Unix(acts[len(acts)-1], 0)
		if next := last.Add(pol.ActCooldown); now.Before(next) {
			return false, fmt.Sprintf("cooldown: last act %s ago, next allowed at %s",
				now.Sub(last).Truncate(time.Second), next.UTC().Format(time.RFC3339))
		}
	}
	return true, ""
}
