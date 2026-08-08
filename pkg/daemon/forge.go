package daemon

import (
	"context"
	"sort"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-107: the async forge orchestration step. ForgeStep computes ONE
// orchestration action from current board state without blocking, so a
// continuous loop (herd forge --loop) can drive cards to-do → in-progress →
// in-review → done by calling it repeatedly. It never reviews or merges
// itself — it emits the ACTION the coordinator/loop should take next, keeping
// N builder lanes saturated.

// ForgeActionKind is the kind of next orchestration action.
type ForgeActionKind string

const (
	// ActionApprove: an in-review card is ready — merge + approve it.
	ActionApprove ForgeActionKind = "approve"
	// ActionReview: an in-progress card finished building — send to reviewer.
	ActionReview ForgeActionKind = "review"
	// ActionDispatch: a builder lane is free — dispatch the next to-do card.
	ActionDispatch ForgeActionKind = "dispatch"
	// ActionRenudge: a builder reported done but FAILED the completion gate
	// (herd verify) — re-drive it instead of routing garbage to review.
	// FAC-116: an agent's "done" is only real once verify passes.
	ActionRenudge ForgeActionKind = "renudge"
	// ActionReject: an in-review card carries an unrepaired FAIL verdict —
	// route the reviewer's numbered rejection back to its authoring worker.
	// FAC-140: reviewers posted FAIL and went idle, so the worker sat holding
	// nothing and the card stalled in-review with no coordinator in the loop.
	ActionReject ForgeActionKind = "reject"
	// ActionIdle: nothing to do right now.
	ActionIdle ForgeActionKind = "idle"
)

// Rejection is a reviewer FAIL that has not been superseded by a PASS: the
// exact candidate SHA that failed and the numbered findings the worker must
// repair. Findings is the reviewer's evidence body, not a summary of it — the
// worker repairs against what the reviewer actually wrote.
type Rejection struct {
	Ref      string
	SHA      string // the FAILed candidate SHA, and the idempotency key
	Reviewer string
	Artifact string // path to the verdict artifact Findings came from
	Findings string
}

// ForgeAction is a single orchestration instruction.
type ForgeAction struct {
	Kind ForgeActionKind
	Ref  string // ticket ref the action targets ("" for idle)
	Task *provider.Task
	// Rejection is set on ActionReject only.
	Rejection *Rejection
}

// LaneState reports how many builder lanes are busy vs the max, so the loop
// backfills only when capacity exists.
type LaneState struct {
	Busy int
	Max  int
}

// ForgeStep picks the single next action, in strict precedence so work drains
// right-to-left across the board (finish before starting):
//  1. APPROVE any card already in-review that carries no outstanding FAIL.
//  2. REJECT — route an outstanding FAIL back to the card's author.
//  3. REVIEW any in-progress card whose builder has finished (caller supplies
//     the set of refs that reported complete).
//  4. DISPATCH the next to-do card when a builder lane is free.
//  5. IDLE.
//
// completed is the set of in-progress refs whose builder called back done;
// verified is the subset that PASSED herd verify (real commits + build +
// tests). A completed-but-unverified build is re-nudged, never reviewed —
// this is the self-gate (FAC-116) that stops whiffed/stalled work from
// reaching the reviewer.
//
// rejections maps ref → the reviewer's unrepaired FAIL. FAC-140: an in-review
// card used to be approved on nothing but its column, so a FAILed candidate
// went to the merge gate while its rejection went nowhere. A rejected card is
// now never approvable, and the FAIL routes to the worker instead.
func (e *Engine) ForgeStep(ctx context.Context, lanes LaneState, completed, verified map[string]bool, rejections map[string]Rejection) (*ForgeAction, error) {
	projectID := e.Config.TaskProvider.ProjectID

	// BLOCKED(provider_timeout): stay responsive, claim nothing. Caller
	// (ForgeLoop) will beginRecovery on the next tick before probing again.
	if e.health.isBlocked() {
		return &ForgeAction{Kind: ActionIdle, Ref: e.ProviderStatus()}, nil
	}

	// 1. Approve in-review first — always finish before starting new work,
	//    but only cards no reviewer has rejected. A FAIL is a hard veto on the
	//    merge gate, not a hint: an approve attempt on a rejected candidate is
	//    the exact "merge on FAIL" this card exists to prevent.
	inReview, err := e.listTasksBound(ctx, projectID, "in-review")
	if err != nil {
		return nil, formatProviderStepError("list in-review", err)
	}
	var approvable, rejected []*provider.Task
	for _, t := range inReview {
		if _, failed := rejections[t.Ref]; failed {
			rejected = append(rejected, t)
			continue
		}
		approvable = append(approvable, t)
	}
	if t := firstByPriority(approvable); t != nil {
		return &ForgeAction{Kind: ActionApprove, Ref: t.Ref, Task: t}, nil
	}

	// 2. Route an outstanding rejection back to its author. The card stays
	//    in-review until a fresh candidate earns a fresh PASS.
	if t := firstByPriority(rejected); t != nil {
		r := rejections[t.Ref]
		return &ForgeAction{Kind: ActionReject, Ref: t.Ref, Task: t, Rejection: &r}, nil
	}

	// 3. Review any completed in-progress build.
	if len(completed) > 0 {
		inProgress, err := e.listTasksBound(ctx, projectID, "in-progress")
		if err != nil {
			return nil, formatProviderStepError("list in-progress", err)
		}
		var ready, failed []*provider.Task
		for _, t := range inProgress {
			if !completed[t.Ref] {
				continue
			}
			if verified[t.Ref] {
				ready = append(ready, t)
			} else {
				failed = append(failed, t)
			}
		}
		// Verified builds go to review; unverified "done" builds get re-nudged
		// (self-gate) — review only sees work that actually passed verify.
		if t := firstByPriority(ready); t != nil {
			return &ForgeAction{Kind: ActionReview, Ref: t.Ref, Task: t}, nil
		}
		if t := firstByPriority(failed); t != nil {
			return &ForgeAction{Kind: ActionRenudge, Ref: t.Ref, Task: t}, nil
		}
	}

	// 4. Dispatch the next to-do when a lane is free.
	if lanes.Busy < lanes.Max {
		next, err := e.SelectNextTask(ctx, "worker")
		if err != nil {
			return nil, err
		}
		if next != nil {
			return &ForgeAction{Kind: ActionDispatch, Ref: next.Ref, Task: next}, nil
		}
	}

	return &ForgeAction{Kind: ActionIdle}, nil
}

// firstByPriority returns the highest-priority task (Priority DESC, Ref ASC),
// or nil when the slice is empty.
func firstByPriority(tasks []*provider.Task) *provider.Task {
	if len(tasks) == 0 {
		return nil
	}
	rank := map[provider.Priority]int{
		provider.PriorityUrgent: 4,
		provider.PriorityHigh:   3,
		provider.PriorityMedium: 2,
		provider.PriorityLow:    1,
	}
	s := append([]*provider.Task(nil), tasks...)
	sort.SliceStable(s, func(i, j int) bool {
		pi, pj := rank[s[i].Priority], rank[s[j].Priority]
		if pi != pj {
			return pi > pj
		}
		return provider.CompareRefs(s[i].Ref, s[j].Ref) < 0
	})
	return s[0]
}
