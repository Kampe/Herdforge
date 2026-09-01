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

func TestReconcileLandedReducedProvenanceUsesExactVerdictAndProof(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate")
	landed := rewriteOnto(t, dir, "landed", base, []string{candidate})
	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)
	g := &Gate{RepoDir: dir, Ledger: l, Policy: testPolicy(), Live: LiveState{OriginMain: StaticProbe(landed)}}
	req := Request{Ref: testRef, CandidateSHA: candidate, BaseSHA: base, ReducedProvenance: &ReducedProvenance{PullRequest: 2864, VerifyLanded: true}}
	receipt, err := g.ReconcileLanded(req)
	if err != nil {
		t.Fatalf("reduced reconcile: %v", err)
	}
	if receipt.ProvenanceMode != hsync.ProvenanceReduced || receipt.PullRequest != 2864 {
		t.Fatalf("reduced receipt provenance = %q/%d", receipt.ProvenanceMode, receipt.PullRequest)
	}
	if receipt.TaskID != "" || receipt.ProviderRevision != "" || receipt.AcceptanceDigest != "" {
		t.Fatal("reduced receipt fabricated omitted provenance")
	}
	if receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest() {
		t.Fatal("reduced receipt is not sealed")
	}
}

func TestReconcileLandedReducedProvenanceRefusesMissingMinimum(t *testing.T) {
	g := &Gate{Ledger: &reviewledger.Ledger{}, Policy: testPolicy()}
	for name, rp := range map[string]*ReducedProvenance{
		"missing pr":    {VerifyLanded: true},
		"missing proof": {PullRequest: 2864},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := g.ReconcileLanded(Request{Ref: testRef, CandidateSHA: shaCurrent, BaseSHA: shaBase, ReducedProvenance: rp})
			if err == nil || !strings.Contains(err.Error(), "reduced provenance requires") {
				t.Fatalf("expected hard refusal, got %v", err)
			}
		})
	}
}

// FAC-681: GitHub rebases a reviewed stack onto the current main. A main-side
// context change can alter an intermediate stable patch ID even though the
// complete reviewed change is preserved. The generated empty worktree anchor
// is administrative history, not one of the two reviewed content commits.
func TestProveEquivalentLandedContextChangedStack(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "shared.txt", "alpha\ncontext one\ncontext two\nold context\ncontext four\nomega\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	run(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "worktree anchor")
	first := commit(t, dir, "shared.txt", "alpha\nreviewed one\ncontext one\ncontext two\nold context\ncontext four\nomega\n", "reviewed one")
	candidate := commit(t, dir, "second.txt", "reviewed two\n", "reviewed two")

	run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
	commit(t, dir, "shared.txt", "alpha\ncontext one\ncontext two\nnew main context\ncontext four\nomega\n", "main advances")
	landedFirst := commit(t, dir, "shared.txt", "alpha\nreviewed one\ncontext one\ncontext two\nnew main context\ncontext four\nomega\n", "reviewed one")
	landed := commit(t, dir, "second.txt", "reviewed two\n", "reviewed two")

	wantFirst, err := commitPatchID(dir, first)
	if err != nil {
		t.Fatalf("candidate first patch id: %v", err)
	}
	gotFirst, err := commitPatchID(dir, landedFirst)
	if err != nil {
		t.Fatalf("landed first patch id: %v", err)
	}
	if wantFirst == gotFirst {
		t.Fatal("fixture first patch IDs match; this does not reproduce FAC-681")
	}

	proof, err := ProveEquivalentLanded(dir, ProofRequest{
		BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed,
	})
	if err != nil {
		t.Fatalf("context-changed stacked proof: %v", err)
	}
	if proof.MergeSHA != landed {
		t.Fatalf("merge sha = %s, want landed stack tip %s", proof.MergeSHA, landed)
	}
	if proof.Method != "combined-range-replay-on-landed" {
		t.Fatalf("method = %q", proof.Method)
	}

	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)
	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{OriginMain: StaticProbe(landed)},
	}
	receipt, err := g.ReconcileLanded(okRequest(base, candidate))
	if err != nil {
		t.Fatalf("reconcile context-changed stack: %v", err)
	}
	if receipt.BaseSHA != base || receipt.CandidateSHA != candidate || receipt.MergeSHA != landed {
		t.Fatalf("receipt identity base=%s candidate=%s merge=%s", short(receipt.BaseSHA), short(receipt.CandidateSHA), short(receipt.MergeSHA))
	}
	readBack, err := hsync.LoadReceipt(hsync.ReceiptPath(dir, testRef))
	if err != nil {
		t.Fatalf("read back reconciled receipt: %v", err)
	}
	if readBack.Digest != receipt.Digest || readBack.Digest != readBack.ComputeDigest() {
		t.Fatal("reconciled receipt did not read back with its exact sealed identity")
	}
}

func TestProveEquivalentLandedContextChangedStackMutationControls(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutation string
	}{
		{name: "omitted reviewed commit", mutation: "omit"},
		{name: "changed reviewed commit", mutation: "change"},
		{name: "reordered reviewed commits", mutation: "reorder"},
		{name: "substituted unreviewed commit", mutation: "substitute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitRepo(t)
			base := commit(t, dir, "shared.txt", "alpha\ncontext one\ncontext two\nold context\ncontext four\nomega\n", "base")
			run(t, dir, "git", "checkout", "-q", "-b", "work")
			first := commit(t, dir, "shared.txt", "alpha\nreviewed one\ncontext one\ncontext two\nold context\ncontext four\nomega\n", "reviewed one")
			candidate := commit(t, dir, "second.txt", "reviewed two\n", "reviewed two")

			run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
			commit(t, dir, "shared.txt", "alpha\ncontext one\ncontext two\nnew main context\ncontext four\nomega\n", "main advances")
			switch tc.mutation {
			case "omit":
				commit(t, dir, "second.txt", "reviewed two\n", "reviewed two")
			case "change":
				commit(t, dir, "shared.txt", "alpha\naltered one\ncontext one\ncontext two\nnew main context\ncontext four\nomega\n", "altered one")
				commit(t, dir, "second.txt", "reviewed two\n", "reviewed two")
			case "reorder":
				commit(t, dir, "second.txt", "reviewed two\n", "reviewed two")
				commit(t, dir, "shared.txt", "alpha\nreviewed one\ncontext one\ncontext two\nnew main context\ncontext four\nomega\n", "reviewed one")
			case "substitute":
				commit(t, dir, "unreviewed.txt", "not reviewed\n", "unreviewed substitute")
				commit(t, dir, "second.txt", "reviewed two\n", "reviewed two")
			}
			landed := revParse(t, dir, "HEAD")

			// The reordered control must truly end in the other reviewed patch;
			// otherwise it would not isolate the ordered endpoint binding.
			if tc.mutation == "reorder" {
				candidateTip, err := commitPatchID(dir, candidate)
				if err != nil {
					t.Fatalf("candidate tip patch: %v", err)
				}
				landedTip, err := commitPatchID(dir, landed)
				if err != nil {
					t.Fatalf("landed tip patch: %v", err)
				}
				if candidateTip == landedTip {
					t.Fatal("reordered fixture retained the reviewed tip patch")
				}
			}

			if _, err := ProveEquivalentLanded(dir, ProofRequest{
				BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed,
			}); err == nil {
				t.Fatalf("proof admitted %s (candidate first commit %s)", tc.name, short(first))
			}
		})
	}
}
