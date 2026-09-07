package mergeadmit

import (
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"strings"
	"testing"
)

func TestEquivalentLandedSquashStack(t *testing.T) {
	for _, tc := range []struct {
		name, body           string
		extra, advance, want bool
	}{
		{name: "overlapping edits", body: "final\n", want: true},
		{name: "later unrelated main", body: "final\n", advance: true, want: true},
		{name: "missing second edit", body: "intermediate\n"},
		{name: "altered result", body: "altered\n"},
		{name: "whitespace alteration", body: "final \n"},
		{name: "unreviewed extra", body: "final\n", extra: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitRepo(t)
			base := commit(t, dir, "a.txt", "base\n", "base")
			run(t, dir, "git", "checkout", "-q", "-b", "work")
			commit(t, dir, "a.txt", "intermediate\n", "first reviewed edit")
			candidate := commit(t, dir, "a.txt", "final\n", "second reviewed edit")
			run(t, dir, "git", "checkout", "-q", "main")
			// Unrelated work preceding the squash must not be mistaken for reviewed scope.
			commit(t, dir, "before.txt", "unrelated\n", "main advance")
			if tc.extra {
				commit(t, dir, "extra.txt", "not reviewed\n", "extra before squash")
			}
			merge := commit(t, dir, "a.txt", tc.body, "squashed range")
			if tc.extra {
				// Put an additional, unreviewed change in the squash itself.
				run(t, dir, "git", "reset", "--soft", "HEAD~2")
				run(t, dir, "git", "commit", "-q", "-m", "squash plus extra")
				merge = revParse(t, dir, "HEAD")
			}
			landed := merge
			if tc.advance {
				landed = commit(t, dir, "after.txt", "later\n", "later main")
			}
			proof, err := ProveEquivalentLanded(dir, ProofRequest{BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
			if !tc.want {
				if err == nil {
					t.Fatal("admitted altered squash")
				}
				return
			}
			if err != nil {
				t.Fatalf("exact squash refused: %v", err)
			}
			if proof.MergeSHA != merge || proof.CandidateSHA != candidate || proof.BaseSHA != base {
				t.Fatalf("wrong immutable proof: %+v", proof)
			}
			if proof.Mode != ModeSquash {
				t.Fatalf("proof did not identify squash: %+v", proof)
			}
		})
	}
}

func TestSquashLandingRefusesAmbiguousEndpoints(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "base\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	commit(t, dir, "a.txt", "intermediate\n", "first")
	candidate := commit(t, dir, "a.txt", "final\n", "second")
	run(t, dir, "git", "checkout", "-q", "main")
	commit(t, dir, "a.txt", "final\n", "first squash")
	commit(t, dir, "a.txt", "base\n", "revert squash")
	landed := commit(t, dir, "a.txt", "final\n", "second squash")
	_, err := ProveEquivalentLanded(dir, ProofRequest{BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err == nil || !strings.Contains(err.Error(), "ambiguous squash") {
		t.Fatalf("ambiguous endpoints admitted or wrong refusal: %v", err)
	}
}

func TestReconcileSquashStackRequiresExactPASS(t *testing.T) {
	for _, pass := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing PASS", true: "exact PASS"}[pass], func(t *testing.T) {
			dir := gitRepo(t)
			base := commit(t, dir, "a.txt", "base\n", "base")
			run(t, dir, "git", "checkout", "-q", "-b", "work")
			commit(t, dir, "a.txt", "intermediate\n", "first")
			candidate := commit(t, dir, "a.txt", "final\n", "second")
			run(t, dir, "git", "checkout", "-q", "main")
			commit(t, dir, "unrelated.txt", "main\n", "main advance")
			landed := commit(t, dir, "a.txt", "final\n", "squash")
			l := newLedger(t, dir)
			launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
			if pass {
				verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)
			}
			g := &Gate{RepoDir: dir, Ledger: l, Policy: testPolicy(), Live: LiveState{OriginMain: StaticProbe(landed)}}
			receipt, err := g.ReconcileLanded(Request{Ref: testRef, CandidateSHA: candidate, BaseSHA: base, ReducedProvenance: &ReducedProvenance{PullRequest: 758, VerifyLanded: true}})
			if !pass {
				if err == nil {
					t.Fatal("minted receipt without exact PASS")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest() {
				t.Fatal("unsealed receipt")
			}
		})
	}
}
