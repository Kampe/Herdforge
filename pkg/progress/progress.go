// Package progress distinguishes a lane that is DOING something from a lane
// whose terminal is merely busy.
//
// FAC-661: every fleet counter derived "working" from foreground process state,
// so a lane sleeping in a poll loop, re-emitting an unchanged report, or
// acknowledging a handoff counted exactly the same as one producing a candidate.
// Measured on the live fleet: perf-cost-guard emitted near-identical reports
// every one to two minutes and herd-smith reached continuation 42 waiting on a
// review cap, and both were reported as busy. Meanwhile herd status said
// working=1 while pulse said busy=9 -- two counters disagreeing because neither
// was measuring work.
//
// The unit of progress here is a STATE TRANSITION backed by an artifact, not an
// elapsed interval and not a process being alive. A lane advances when it
// produces something another stage can consume: a commit, a candidate, a
// verdict, an admission, a merge. It does not advance by observing that the
// world is unchanged, however diligently.
package progress

import (
	"strings"
	"time"
)

// Class is what a lane is doing, as opposed to whether its terminal is active.
type Class string

const (
	// ClassBuild produced or advanced a candidate.
	ClassBuild Class = "build"
	// ClassReview produced a verdict.
	ClassReview Class = "review"
	// ClassMerge landed work.
	ClassMerge Class = "merge"
	// ClassProbe observed state without changing it. Never progress on its own.
	ClassProbe Class = "probe"
	// ClassWait is an explicit, declared wait on a named event. Legitimate for a
	// standing lane and deliberately NOT a failure -- but not progress either.
	ClassWait Class = "wait"
)

// Record is one lane's work identity and its last real advance.
type Record struct {
	Lane string `json:"lane"`
	// TaskRef is the exact task. A record without one cannot be acted on: it
	// says a lane is busy without saying what it is busy WITH, which is the
	// selector defect this package exists to stop reproducing.
	TaskRef string `json:"task_ref,omitempty"`
	Action  Class  `json:"action_class"`
	// CandidateSHA is the exact commit under work, when there is one.
	CandidateSHA string `json:"candidate_sha,omitempty"`
	// LastArtifact names the thing most recently produced. Empty means the lane
	// has never produced anything, which is a fact worth reporting, not a gap
	// to paper over.
	LastArtifact   string    `json:"last_artifact,omitempty"`
	LastAdvance    time.Time `json:"last_advance,omitempty"`
	UnchangedBeats int       `json:"unchanged_beats"`
	// WaitReason is required whenever Action is ClassWait. A wait with no named
	// event is indistinguishable from a spin.
	WaitReason  string `json:"wait_reason,omitempty"`
	BlockReason string `json:"block_reason,omitempty"`
}

// Observe folds one beat into a record and reports whether it ADVANCED.
//
// The rule is deliberately narrow: an artifact that differs from the last one
// advances the lane. Everything else increments the unchanged count. That makes
// "I looked again and nothing changed" cost a beat rather than earn one, which
// is the behaviour that let lanes burn quota looking productive.
func (r Record) Observe(now time.Time, action Class, artifact string) (Record, bool) {
	artifact = strings.TrimSpace(artifact)
	out := r
	out.Action = action

	switch action {
	case ClassProbe, ClassWait:
		// Observing the world never advances it, whatever it observed.
		out.UnchangedBeats = r.UnchangedBeats + 1
		return out, false
	}
	if artifact == "" || artifact == r.LastArtifact {
		// A build/review/merge beat that produced nothing new is not progress
		// either. Re-emitting the same candidate or verdict is the same
		// unchanged report wearing a more productive label.
		out.UnchangedBeats = r.UnchangedBeats + 1
		return out, false
	}
	out.LastArtifact = artifact
	out.LastAdvance = now
	out.UnchangedBeats = 0
	return out, true
}

// PlateauAfter is the shared threshold for "this lane is looping, not
// progressing". One continuation is normal, two can be a slow beat, and by the
// third an unchanged lane is repeating itself.
//
// FAC-665: goal-guard defined its own copy of this number. Two definitions of
// one rule is how they drift, and this codebase has spent the session fixing
// exactly that shape -- a workspace pinned in four places, a task written from
// two sources, a hygiene threshold that did not match the gate it served. The
// threshold lives here because this package owns what progress MEANS.
const PlateauAfter = 3

// Plateaued reports whether a lane has gone this many beats without advancing.
func (r Record) Plateaued(after int) bool {
	return after > 0 && r.UnchangedBeats >= after
}

// ProgressAge is how long since the lane last produced anything. Zero time means
// it never has, which reports as the maximum rather than as "just now" -- a lane
// that has never advanced must never look freshly productive.
func (r Record) ProgressAge(now time.Time) time.Duration {
	if r.LastAdvance.IsZero() {
		return now.Sub(time.Time{})
	}
	return now.Sub(r.LastAdvance)
}

// Actionable reports whether this record can be acted on, and why not.
//
// A record naming no task is the selector defect in miniature: "four claimable"
// with no identities cannot be dispatched, only counted.
func (r Record) Actionable() (bool, string) {
	if strings.TrimSpace(r.Lane) == "" {
		return false, "no lane identity"
	}
	if strings.TrimSpace(r.TaskRef) == "" {
		return false, "no exact task ref: a lane reported busy without naming what it is busy with"
	}
	if r.Action == ClassWait && strings.TrimSpace(r.WaitReason) == "" {
		return false, "waiting with no named event: indistinguishable from a spin"
	}
	return true, ""
}
