// Package broker is the one place that decides what a lane does next.
//
// FAC-663 (CHA-3174): selection, dependency readiness, builder admission,
// progress classification and event-wait were five shallow modules that each
// exposed a separate count and a separate stop condition. Nothing owned the
// decision, so the seams between them leaked:
//
//   - review saturation leaked into BUILDER admission, so a full review queue
//     blocked independent builders. Measured live: dispatch blocked with 6
//     healthy-idle lanes and 2 reviews in flight against a cap of 3.
//   - selectors reported counts without identities -- "4 claimable" that could
//     be counted but not dispatched.
//   - goal-guard rewarded polling, so a lane with nothing to claim could only
//     spin or die. Both happened: near-identical reports every 1-2 minutes, and
//     continuation 42 waiting on a review cap.
//
// The fix is not another counter. It is making one decision that must NAME its
// outcome: a task to work, or an explicit reason it is waiting. There is no
// third answer, and in particular there is no bare count -- a Decision that
// cannot be acted on is rejected by construction rather than by a caller
// remembering to check.
//
// Review admission stays a SEPARATE adapter and is consulted only for review
// work. That separation is the whole point of the seam: it is what stops a
// review cap from ever again deciding whether a builder may work.
package broker

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/progress"
)

// Kind is what a lane is being asked to do.
type Kind string

const (
	KindBuild  Kind = "build"
	KindReview Kind = "review"
)

// Task is a candidate unit of work with an EXACT identity.
type Task struct {
	Ref      string `json:"ref"`
	Kind     Kind   `json:"kind"`
	Priority int    `json:"priority"`
	// DependsOn are task refs that must be closed first. A dependency that is
	// not closed blocks this task and the block is REPORTED, never silent.
	DependsOn []string `json:"depends_on,omitempty"`
	// OwnedPaths lets the broker refuse two lanes colliding on one file.
	OwnedPaths []string `json:"owned_paths,omitempty"`
}

// Outcome is the shape of a decision. Exactly one of Task or Wait is meaningful.
type Outcome string

const (
	// OutcomeWork names an exact task to start.
	OutcomeWork Outcome = "work"
	// OutcomeWait names the event being waited on. Legitimate, and not failure.
	OutcomeWait Outcome = "wait"
)

// Decision is the broker's answer, and it always explains itself.
type Decision struct {
	Outcome Outcome `json:"outcome"`
	Task    *Task   `json:"task,omitempty"`
	// WaitReason is required when Outcome is OutcomeWait. A wait with no named
	// event cannot be told apart from a spin.
	WaitReason string `json:"wait_reason,omitempty"`
	// Blocked records why each rejected task was rejected, so a queue that looks
	// empty can always be explained. This is what "4 claimable" never carried.
	Blocked map[string]string `json:"blocked,omitempty"`
	// Progress is the lane's work classification for this beat.
	Progress progress.Record `json:"progress"`
}

// Validate rejects a decision that cannot be acted on.
//
// This is the guarantee the package exists to provide: a decision either names
// an exact task or names the event it waits on. A bare count is not
// representable, so the selector defect cannot recur through this seam.
func (d Decision) Validate() error {
	switch d.Outcome {
	case OutcomeWork:
		if d.Task == nil || strings.TrimSpace(d.Task.Ref) == "" {
			return fmt.Errorf("broker: a work decision must name an EXACT task ref; a count without an identity can be reported but not dispatched")
		}
		return nil
	case OutcomeWait:
		if strings.TrimSpace(d.WaitReason) == "" {
			return fmt.Errorf("broker: a wait decision must name the event it is waiting on; an unnamed wait is indistinguishable from a spin")
		}
		return nil
	default:
		return fmt.Errorf("broker: decision has no outcome")
	}
}

// Inputs is everything the decision depends on, passed explicitly so the whole
// seam is testable without a live fleet.
type Inputs struct {
	Lane string
	// Kinds this lane may take. A builder lane never receives review work.
	Accepts []Kind
	Queue   []Task
	// ClosedTasks are refs known closed, for dependency readiness.
	ClosedTasks map[string]bool
	// ClaimedPaths are paths another live lane already owns.
	ClaimedPaths map[string]string
	// ReviewSaturated is consulted ONLY for review work. It is deliberately not
	// a global gate: that leak is the defect this seam removes.
	ReviewSaturated bool
	// ReviewWaitReason explains saturation when it applies.
	ReviewWaitReason string
	Progress         progress.Record
}

// Decide is the single decision. It returns a Decision that always validates.
func Decide(in Inputs) Decision {
	blocked := map[string]string{}
	accepts := map[Kind]bool{}
	for _, k := range in.Accepts {
		accepts[k] = true
	}

	candidates := append([]Task(nil), in.Queue...)
	// Deterministic: priority descending, then ref ascending. A selector whose
	// order depends on map iteration cannot be reasoned about or reproduced.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].Ref < candidates[j].Ref
	})

	for _, t := range candidates {
		if !accepts[t.Kind] {
			blocked[t.Ref] = fmt.Sprintf("lane does not accept %s work", t.Kind)
			continue
		}
		// Review saturation applies to REVIEW work only. A builder is never
		// blocked by a full review queue -- the leak this seam exists to close.
		if t.Kind == KindReview && in.ReviewSaturated {
			reason := in.ReviewWaitReason
			if strings.TrimSpace(reason) == "" {
				reason = "review capacity is full"
			}
			blocked[t.Ref] = reason
			continue
		}
		if unmet := unmetDependencies(t, in.ClosedTasks); len(unmet) > 0 {
			blocked[t.Ref] = "blocked by " + strings.Join(unmet, ", ")
			continue
		}
		if owner, path := pathCollision(t, in.ClaimedPaths, in.Lane); owner != "" {
			blocked[t.Ref] = fmt.Sprintf("path %s is owned by %s", path, owner)
			continue
		}
		task := t
		return Decision{
			Outcome:  OutcomeWork,
			Task:     &task,
			Blocked:  blocked,
			Progress: in.Progress,
		}
	}

	return Decision{
		Outcome:    OutcomeWait,
		WaitReason: waitReasonFor(blocked),
		Blocked:    blocked,
		Progress:   in.Progress,
	}
}

func unmetDependencies(t Task, closed map[string]bool) []string {
	var unmet []string
	for _, dep := range t.DependsOn {
		dep = strings.TrimSpace(dep)
		if dep != "" && !closed[dep] {
			unmet = append(unmet, dep)
		}
	}
	sort.Strings(unmet)
	return unmet
}

func pathCollision(t Task, claimed map[string]string, lane string) (string, string) {
	for _, p := range t.OwnedPaths {
		if owner, ok := claimed[p]; ok && owner != lane {
			return owner, p
		}
	}
	return "", ""
}

// waitReasonFor turns the block map into ONE sentence naming what would unblock
// the lane. An empty queue and a fully-blocked queue are different states and
// must read differently: the first is idle, the second is waiting on something
// specific.
func waitReasonFor(blocked map[string]string) string {
	if len(blocked) == 0 {
		return "no claimable work in the queue (the queue is empty, not blocked)"
	}
	refs := make([]string, 0, len(blocked))
	for ref := range blocked {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, ref+": "+blocked[ref])
	}
	return fmt.Sprintf("%d task(s) present but none claimable — %s", len(refs), strings.Join(parts, "; "))
}
