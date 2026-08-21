package review

import (
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// FAC-559: review-scan visited every unmerged worktree, and on a real board 64
// of them included classes that are never product review candidates. Scoping
// the set cuts the work instead of paying for it with a bigger budget.
//
// The rules below are the CONSUMER's declared semantics, not inferred ones.
// Guessing here is dangerous in one specific direction: excluding a branch
// class wrongly hides a real candidate, which is far worse than a slow scan.
// So exclusions are narrow and explicit, and everything unrecognized is kept.

// Skip reasons, reported so nothing is dropped silently. A scan that quietly
// shrinks its own input reads as "nothing to drain".
const (
	SkipReachabilityScratch = "audit-reachability-scratch"
	SkipArchived            = "archived-branch"
	SkipAgentScratch        = "agent-scratch-identity"
	SkipUnreceiptedControl  = "control-lane-without-candidate-receipt"
)

// SkippedCandidate is one excluded worktree and why.
type SkippedCandidate struct {
	Branch string `json:"branch"`
	Reason string `json:"reason"`
}

// HasCandidateReceipt reports whether a worktree carries a task-bound candidate
// receipt naming one of its exact commits. It takes the whole worktree, not the
// branch name, because the consumer's rule is explicitly about an exact SHA: a
// moving standing branch tip must never be treated wholesale as one harvestable
// candidate.
type HasCandidateReceipt func(w harvest.UnmergedWork) bool

// ScopeDrainCandidates splits unmerged worktrees into those worth review-scanning
// and those excluded, with a reason for each exclusion.
//
// Declared semantics:
//   - audit/*-reachability, archive/*: read-only proofs and history, never
//     product review candidates.
//   - worktree-agent-*: scratch identity, excluded unless a receipt names the
//     exact branch.
//   - standing/* and herd/*: these DO carry real candidate commits, so they are
//     never prefix-excluded. But a moving standing branch tip must not be
//     treated wholesale as one harvestable candidate, so they are receipt-gated:
//     included only when a task-bound candidate receipt names them, otherwise
//     they are control-lane state.
//   - recon/*, reconstruct/*, review/*, fix/*, and anything unrecognized: kept.
func ScopeDrainCandidates(unmerged []harvest.UnmergedWork, hasReceipt HasCandidateReceipt) (kept []harvest.UnmergedWork, skipped []SkippedCandidate) {
	oracleAvailable := hasReceipt != nil
	if hasReceipt == nil {
		// Without a receipt oracle, control-lane classes cannot be proven
		// non-candidates. Keep them: a slow scan beats a hidden candidate.
		// Agent scratch is the documented exception -- see scopeReason.
		hasReceipt = func(harvest.UnmergedWork) bool { return true }
	}
	for _, w := range unmerged {
		branch := strings.TrimSpace(w.Branch)
		if branch == "" {
			// No branch identity to judge. Keep it and let review-scan decide.
			kept = append(kept, w)
			continue
		}
		if reason, skip := scopeReason(branch, w, hasReceipt, oracleAvailable); skip {
			skipped = append(skipped, SkippedCandidate{Branch: branch, Reason: reason})
			continue
		}
		kept = append(kept, w)
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Branch < skipped[j].Branch })
	return kept, skipped
}

func scopeReason(branch string, w harvest.UnmergedWork, hasReceipt HasCandidateReceipt, receiptOracleAvailable bool) (string, bool) {
	lower := strings.ToLower(branch)

	// Narrow on purpose: only audit branches that are reachability proofs. A
	// bare audit/* could legitimately carry product work.
	if strings.HasPrefix(lower, "audit/") && strings.HasSuffix(lower, "-reachability") {
		return SkipReachabilityScratch, true
	}
	if strings.HasPrefix(lower, "archive/") {
		return SkipArchived, true
	}
	if strings.HasPrefix(lower, "worktree-agent-") {
		// Declared rule: exclude BY DEFAULT unless an explicit receipt names it.
		// The generic fail-open below is wrong for this class -- it kept an
		// agent scratch branch in scope on a board with no readable receipts,
		// which is exactly what the consumer reported. Default is exclude.
		if receiptOracleAvailable && hasReceipt(w) {
			return "", false
		}
		return SkipAgentScratch, true
	}
	// Control-lane branches: real candidates live here, so gate on a receipt
	// rather than excluding the prefix.
	if strings.HasPrefix(lower, "standing/") || strings.HasPrefix(lower, "herd/") {
		if hasReceipt(w) {
			return "", false
		}
		return SkipUnreceiptedControl, true
	}
	return "", false
}

// SummarizeSkips counts exclusions by reason for operator output.
func SummarizeSkips(skipped []SkippedCandidate) map[string]int {
	out := map[string]int{}
	for _, s := range skipped {
		out[s.Reason]++
	}
	return out
}

// CountTips returns how many TIPS a worktree set expands to.
//
// FAC-561: review-scan iterates one tip per unmerged SHA, not one per worktree.
// A budget computed from the worktree count was therefore off by the average
// commits-per-worktree: 54 in-scope worktrees expanded to 400 tips, so a
// 54-item budget (2m42s) covered roughly an eighth of the actual work and the
// scan could never finish. Callers must budget on this number.
//
// It is deliberately pure and I/O-free so it can be called before budgeting.
func CountTips(unmerged []harvest.UnmergedWork) int {
	seen := map[string]bool{}
	n := 0
	for _, u := range unmerged {
		for _, sha := range u.Unmerged {
			if sha == "" || seen[sha] {
				continue
			}
			seen[sha] = true
			n++
		}
	}
	return n
}
