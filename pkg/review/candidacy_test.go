package review

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// The exact 64-branch shape reported live, trimmed to one of each class.
func liveBranches() []harvest.UnmergedWork {
	return []harvest.UnmergedWork{
		{Branch: "audit/cha-2170-reachability"},
		{Branch: "archive/perf-post-boundary-6dace"},
		{Branch: "worktree-agent-a220b4ebf0cd79450"},
		{Branch: "standing/nft-data-engineer", Unmerged: []string{"8ef12f0dbd6b172745c3b9abde4fc294fe7b1d2f"}},
		{Branch: "standing/platform-ops", Unmerged: []string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}},
		{Branch: "herd/cha-1804", Unmerged: []string{"cafebabecafebabecafebabecafebabecafebabe"}},
		{Branch: "recon/CHA-2153"},
		{Branch: "reconstruct/cha-2150"},
		{Branch: "review/cha-2138-reconstruction"},
		{Branch: "fix/cha-2173-morpho-usr"},
	}
}

// TestScopeExcludesOnlyDeclaredScratch pins the consumer's declared semantics.
// Excluding a class wrongly hides a real candidate, which is worse than a slow
// scan, so the exclusions must stay exactly this narrow.
func TestScopeExcludesOnlyDeclaredScratch(t *testing.T) {
	// Receipt names the CHA-2199 candidate on standing/nft-data-engineer.
	receipted := map[string]bool{"8ef12f0dbd6b172745c3b9abde4fc294fe7b1d2f": true}
	oracle := func(w harvest.UnmergedWork) bool {
		for _, sha := range w.Unmerged {
			if receipted[sha] {
				return true
			}
		}
		return false
	}

	kept, skipped := ScopeDrainCandidates(liveBranches(), oracle)

	keptSet := map[string]bool{}
	for _, k := range kept {
		keptSet[k.Branch] = true
	}
	// Product candidate classes must all survive.
	for _, want := range []string{
		"recon/CHA-2153", "reconstruct/cha-2150",
		"review/cha-2138-reconstruction", "fix/cha-2173-morpho-usr",
	} {
		if !keptSet[want] {
			t.Fatalf("%s is a real candidate class and must be kept", want)
		}
	}
	// A receipted standing branch IS a candidate.
	if !keptSet["standing/nft-data-engineer"] {
		t.Fatal("a receipted standing branch must be kept (CHA-2199 case)")
	}
	// An unreceipted standing/herd branch is control-lane state.
	if keptSet["standing/platform-ops"] || keptSet["herd/cha-1804"] {
		t.Fatal("unreceipted control-lane branches must be scoped out")
	}

	reasons := SummarizeSkips(skipped)
	if reasons[SkipReachabilityScratch] != 1 {
		t.Fatalf("audit reachability scratch must be excluded, got %v", reasons)
	}
	if reasons[SkipArchived] != 1 {
		t.Fatalf("archive/* must be excluded, got %v", reasons)
	}
	if reasons[SkipAgentScratch] != 1 {
		t.Fatalf("unreceipted worktree-agent-* must be excluded, got %v", reasons)
	}
	if reasons[SkipUnreceiptedControl] != 2 {
		t.Fatalf("both unreceipted control lanes must be excluded, got %v", reasons)
	}
}

// A bare audit/* branch could carry product work; only reachability proofs are
// scratch. This guards against widening the exclusion.
func TestScopeKeepsNonReachabilityAuditBranch(t *testing.T) {
	kept, skipped := ScopeDrainCandidates(
		[]harvest.UnmergedWork{{Branch: "audit/cha-2200-real-work"}}, nil)
	if len(kept) != 1 || len(skipped) != 0 {
		t.Fatalf("a non-reachability audit branch must be kept: kept=%d skipped=%v", len(kept), skipped)
	}
}

// Without a receipt oracle, CONTROL LANES cannot be proven non-candidates and
// stay in scope: hiding a real candidate is worse than a slow scan. Agent
// scratch is the declared exception -- excluded by default unless a receipt
// names it. This test originally asserted the opposite for agent scratch, which
// is how a scratch branch stayed in scope on a board with no receipts.
func TestScopeFailsOpenForControlLanesOnly(t *testing.T) {
	kept, skipped := ScopeDrainCandidates([]harvest.UnmergedWork{
		{Branch: "standing/nft-data-engineer"},
		{Branch: "herd/cha-1804"},
		{Branch: "worktree-agent-abc"},
	}, nil)
	keptSet := map[string]bool{}
	for _, k := range kept {
		keptSet[k.Branch] = true
	}
	if !keptSet["standing/nft-data-engineer"] || !keptSet["herd/cha-1804"] {
		t.Fatalf("control lanes must stay in scope without an oracle, kept=%v", keptSet)
	}
	if keptSet["worktree-agent-abc"] {
		t.Fatalf("agent scratch is excluded by default, kept=%v skipped=%v", keptSet, skipped)
	}
}

// A worktree with no branch identity cannot be judged and must be kept.
func TestScopeKeepsUnnamedWorktree(t *testing.T) {
	kept, _ := ScopeDrainCandidates([]harvest.UnmergedWork{{WorktreePath: "/tmp/x"}}, nil)
	if len(kept) != 1 {
		t.Fatal("an unnamed worktree must be kept for review-scan to judge")
	}
}
