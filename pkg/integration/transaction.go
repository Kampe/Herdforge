// Package integration serializes a ready PASS through to cleanup as ONE
// lifecycle transaction.
//
// FAC-709. Review, ingest, merge, runtime bind and worktree cleanup were split
// across panes with no enforced owner. Builders handed off and nobody consumed
// the handoff, so reviewed heads and stale worktrees accumulated: 403 worktree
// registrations at the high-water mark, and ~210 cards left in-progress because
// nothing closed the loop after a verdict.
//
// The steps were never the problem. The ORDER was, and specifically one edge in
// it: cleanup destroys the source worktree and branch, and it was reachable
// without proof that the work had landed. A cleanup that runs on an unproven
// landing is indistinguishable from data loss.
//
// So this models the transaction rather than performing it. It owns the order,
// the receipt, and the one refusal that matters.
package integration

import (
	"fmt"
	"strings"
	"time"
)

// Step is one stage of the integration lifecycle, in the only order they may run.
type Step string

const (
	// StepPass is the exact reviewed candidate entering integration.
	StepPass Step = "exact-pass"
	// StepHarvest rebases/extracts the candidate onto current trunk.
	StepHarvest Step = "harvest-current-main"
	// StepIntegrationPR opens the integration pull request.
	StepIntegrationPR Step = "integration-pr"
	// StepMerge lands it.
	StepMerge Step = "merge"
	// StepRuntimeBind points the canonical runtime at the merged revision.
	StepRuntimeBind Step = "runtime-bind"
	// StepPatchProof proves the merged content IS the reviewed content.
	StepPatchProof Step = "patch-identity-proof"
	// StepCleanup retires the source worktree and branch. DESTRUCTIVE.
	StepCleanup Step = "cleanup"
)

// Order is the lifecycle, and it is the contract. Cleanup is last and
// patch-identity proof immediately precedes it, because proof is the only thing
// that makes destroying the source safe.
var Order = []Step{
	StepPass,
	StepHarvest,
	StepIntegrationPR,
	StepMerge,
	StepRuntimeBind,
	StepPatchProof,
	StepCleanup,
}

// Record is one completed step, appended before the next begins so a crash
// leaves a diagnosable trail rather than an ambiguous half-state.
type Record struct {
	Step       Step   `json:"step"`
	Candidate  string `json:"candidate"`
	Evidence   string `json:"evidence"`
	RecordedAt string `json:"recorded_at"`
}

// Transaction is the serialized lifecycle for exactly one candidate.
type Transaction struct {
	Candidate string   `json:"candidate"`
	Done      []Record `json:"done"`
}

// New starts a transaction for an exact candidate SHA.
func New(candidate string) (*Transaction, error) {
	if len(strings.TrimSpace(candidate)) < 12 {
		return nil, fmt.Errorf("integration requires an exact candidate sha (>=12 chars), got %q: "+
			"a transaction keyed on anything less cannot prove which content it landed", candidate)
	}
	return &Transaction{Candidate: strings.TrimSpace(candidate)}, nil
}

// Completed reports whether a step has already been recorded.
func (t *Transaction) Completed(s Step) bool {
	for _, r := range t.Done {
		if r.Step == s {
			return true
		}
	}
	return false
}

// Next returns the step this transaction may perform now.
func (t *Transaction) Next() (Step, bool) {
	for _, s := range Order {
		if !t.Completed(s) {
			return s, true
		}
	}
	return "", false
}

// Complete records a finished step.
//
// Two refusals, and they are the whole safety argument.
//
// OUT OF ORDER is refused because the lifecycle is a sequence, not a set. A
// merge recorded before a harvest describes a candidate that was never rebased
// onto current trunk.
//
// CLEANUP WITHOUT PATCH PROOF is refused specifically and loudly. Cleanup
// destroys the source worktree and branch; patch-identity proof is what
// establishes that the merged content is the reviewed content. Running cleanup
// first is not an ordering nicety -- it is destroying the only copy of work that
// may not have landed. This session watched a reaper classify a coordinator's
// own home as removable, and 154 commits sit stranded on a standing branch
// right now, so the cost of getting this edge wrong is measured, not theoretical.
func (t *Transaction) Complete(s Step, evidence string) error {
	if t.Completed(s) {
		return fmt.Errorf("integration %s: step %q already recorded", short(t.Candidate), s)
	}
	next, ok := t.Next()
	if !ok {
		return fmt.Errorf("integration %s: transaction is already complete", short(t.Candidate))
	}
	// Checked BEFORE the generic ordering refusal on purpose. Cleanup out of
	// order is not just a sequencing mistake, and a message that says only
	// "out of order" invites someone to satisfy the sequence and proceed. The
	// most dangerous condition gets the clearest refusal.
	if s == StepCleanup && !t.Completed(StepPatchProof) {
		return fmt.Errorf("integration %s: REFUSING cleanup without %q. "+
			"Cleanup destroys the source worktree and branch, and patch-identity proof is what establishes that the merged content IS the reviewed content. "+
			"Without it this is not tidying, it is destroying the only copy of work that may never have landed",
			short(t.Candidate), StepPatchProof)
	}
	if s != next {
		return fmt.Errorf("integration %s: step %q is out of order; the next step is %q. "+
			"The lifecycle is a sequence, not a set: running it out of order describes a candidate that was never put through the stages it claims",
			short(t.Candidate), s, next)
	}
	if strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("integration %s: step %q recorded no evidence: a step that cannot be checked later is a claim, not a receipt",
			short(t.Candidate), s)
	}
	t.Done = append(t.Done, Record{
		Step:       s,
		Candidate:  t.Candidate,
		Evidence:   strings.TrimSpace(evidence),
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// Blocked describes where a transaction stopped, for a lane to report verbatim.
func (t *Transaction) Blocked(reason string) string {
	next, ok := t.Next()
	if !ok {
		return fmt.Sprintf("integration %s: complete", short(t.Candidate))
	}
	return fmt.Sprintf("integration %s: stopped before %q after %d/%d steps: %s",
		short(t.Candidate), next, len(t.Done), len(Order), strings.TrimSpace(reason))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
