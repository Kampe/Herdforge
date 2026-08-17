package mergeadmit

import (
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-379: an exact reviewed candidate can land under a DIFFERENT merge SHA
// (rebase/cherry-pick rewrite). harvest-merge --verify-landed must still mint
// the sealed completion receipt approve/board-done consume. ModeMerge ancestry
// cannot prove that rewrite; ReconcileLanded must.
func TestReconcileLandedEquivalentPatchDifferentSHA(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")

	// Land an equivalent patch under a new object id, then advance main with
	// unrelated work so tip identity alone cannot stand in for the candidate.
	landedEquiv := rewriteOnto(t, dir, "landed", base, []string{candidate})
	if landedEquiv == candidate {
		t.Fatal("fixture did not rewrite the candidate sha; the test would prove nothing")
	}
	if err := runGit(dir, "merge-base", "--is-ancestor", candidate, landedEquiv); err == nil {
		t.Fatal("fixture candidate is still an ancestor of the rewrite; not the FAC-379 shape")
	}
	run(t, dir, "git", "checkout", "-q", "landed")
	advanced := commit(t, dir, "later.txt", "unrelated\n", "unrelated advance")
	if advanced == landedEquiv {
		t.Fatal("fixture failed to advance main past the equivalent patch")
	}

	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)

	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{
			OriginMain:    StaticProbe(advanced),
			CandidateHead: StaticProbe(candidate),
			Mergeable:     StaticProbe("CLEAN"),
			TaskRevision:  StaticProbe(testRevision),
			Checks:        func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
		},
	}
	req := okRequest(base, candidate)
	req.Mode = ModeRebase

	// Negative guard: ModeMerge Complete cannot close this rewrite. Observe the
	// refusal before trusting ReconcileLanded as the recovery path.
	dMerge := &Decision{
		Admitted: true, Ref: testRef, CandidateSHA: candidate, BaseSHA: base,
		Mode: ModeMerge, Tier: "R3", ReviewerFam: "openai",
		VerificationDigest: testVfy, PolicyRevision: preflight.PolicyRevision(g.Policy),
	}
	if _, err := g.Complete(dMerge, req); err == nil {
		t.Fatal("ModeMerge Complete admitted a rewritten equivalent patch; ReconcileLanded would be unnecessary")
	}

	receipt, err := g.ReconcileLanded(req)
	if err != nil {
		t.Fatalf("ReconcileLanded: %v", err)
	}
	if receipt.CandidateSHA != candidate {
		t.Fatalf("receipt candidate = %s, want reviewed %s", receipt.CandidateSHA, candidate)
	}
	if receipt.MergeSHA == candidate {
		t.Fatal("receipt merge sha equalled the reviewed candidate; expected the equivalent landed commit")
	}
	if receipt.MergeSHA != landedEquiv {
		t.Fatalf("receipt merge sha = %s, want equivalent landed %s (not the advanced tip %s)",
			short(receipt.MergeSHA), short(landedEquiv), short(advanced))
	}
	if receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest() {
		t.Fatal("receipt digest does not match its own contents")
	}
	if receipt.LeaseGeneration != req.LeaseGeneration {
		t.Fatalf("receipt lease generation = %d, want %d", receipt.LeaseGeneration, req.LeaseGeneration)
	}
	if receipt.VerificationDigest != testVfy {
		t.Fatalf("receipt verification digest = %q, want ledger %q", receipt.VerificationDigest, testVfy)
	}
	if receipt.AuthorFamily != "anthropic" || receipt.ReviewerFamily != "openai" {
		t.Fatalf("receipt families author=%s reviewer=%s", receipt.AuthorFamily, receipt.ReviewerFamily)
	}
	if receipt.RiskTier == "" {
		t.Fatal("receipt missing risk tier provenance")
	}

	landedPatch, err := hsync.PatchID(dir, receipt.MergeSHA)
	if err != nil {
		t.Fatalf("patch id of merge sha: %v", err)
	}
	if landedPatch != receipt.PatchID {
		t.Fatalf("receipt patch %s is not the patch on merge sha %s (%s)",
			short(receipt.PatchID), short(receipt.MergeSHA), short(landedPatch))
	}

	run(t, dir, "git", "update-ref", "refs/remotes/origin/main", advanced)
	st := &lifecycle.TaskState{
		TaskRef: testRef, State: lifecycle.StateIntegrated,
		LeaseGeneration: req.LeaseGeneration, CandidateSHA: candidate,
	}
	if err := receipt.Validate(dir, testRef, st); err != nil {
		t.Fatalf("BoardDone would REFUSE the reconciled receipt: %v", err)
	}
	if _, err := os.Stat(hsync.ReceiptPath(dir, testRef)); err != nil {
		t.Fatalf("receipt not on disk: %v", err)
	}
}

func TestReconcileLandedRefusesWhenEquivalentPatchMissing(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")
	run(t, dir, "git", "checkout", "-q", "-B", "main", base)
	unrelated := commit(t, dir, "z.txt", "nope\n", "unrelated")

	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)

	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{
			OriginMain: StaticProbe(unrelated), CandidateHead: StaticProbe(candidate),
			Mergeable: StaticProbe("CLEAN"), TaskRevision: StaticProbe(testRevision),
			Checks: func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
		},
	}
	req := okRequest(base, candidate)
	_, err := g.ReconcileLanded(req)
	if err == nil {
		t.Fatal("ReconcileLanded minted a receipt for a candidate that never landed")
	}
	if !strings.Contains(err.Error(), CodeProofFailed) {
		t.Fatalf("refusal did not carry proof-failure code: %v", err)
	}
	if _, err := os.Stat(hsync.ReceiptPath(dir, testRef)); err == nil {
		t.Fatal("failed reconcile still wrote a receipt")
	}
}

func TestReconcileLandedIsIdempotent(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")
	landedEquiv := rewriteOnto(t, dir, "landed", base, []string{candidate})

	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)

	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{
			OriginMain: StaticProbe(landedEquiv), CandidateHead: StaticProbe(candidate),
			Mergeable: StaticProbe("CLEAN"), TaskRevision: StaticProbe(testRevision),
			Checks: func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
		},
	}
	req := okRequest(base, candidate)
	first, err := g.ReconcileLanded(req)
	if err != nil {
		t.Fatalf("first ReconcileLanded: %v", err)
	}
	second, err := g.ReconcileLanded(req)
	if err != nil {
		t.Fatalf("replayed ReconcileLanded: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("replay minted a different receipt: %s vs %s", short(first.Digest), short(second.Digest))
	}
}
