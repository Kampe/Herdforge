package mergeadmit

import (
	"errors"
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// PUBLIC-ENTRY budget tests.
//
// Review 212: each public entry installed its own allowance and several stages
// ran outside any, so a bounded proof was followed by unbounded git before the
// receipt was sealed. These drive the REAL public methods on the REAL hermetic
// fixtures those paths already use, inject a finite allowance through the actual
// Gate, and assert three things every time: the refusal is recognisable through
// errors.Is, NO receipt is written, and the same fixture on the default
// allowance still succeeds.
//
// A positive MaxCommands is mandatory. Zero means DEFAULT, not exhausted.

// receiptAbsent fails if any receipt reached disk for this ref.
func receiptAbsent(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(hsync.ReceiptPath(dir, testRef)); err == nil {
		t.Fatal("a receipt was sealed despite an exhausted allowance")
	} else if !os.IsNotExist(err) {
		t.Fatalf("receipt path unreadable: %v", err)
	}
}

// Complete: one allowance across proof AND identity, stopping before the seal.
func TestCompletePublicEntryStopsOnTheSharedAllowanceBeforeSealing(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	// Admission FIRST, then the merge. Admit re-reads origin/main and refuses a
	// base that advanced since the candidate was admitted; reporting the merge
	// before admitting trips that guard, which is production behaving correctly.
	d := mustAdmit(t, f.gate, f.req)
	f.merged()
	f.gate.ProofBudget = ProofBudget{MaxCommands: 1}

	receipt, err := f.gate.Complete(d, f.req)
	if err == nil {
		t.Fatal("Complete sealed a receipt on an exhausted allowance")
	}
	if !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands", err)
	}
	if receipt != nil {
		t.Fatalf("a refused Complete returned a receipt: %+v", receipt)
	}
	receiptAbsent(t, f.dir)
}

// The SAME fixture and the SAME public path on the default allowance still
// completes, so the refusal above is not a gate that refuses everything.
func TestCompletePublicEntrySucceedsOnTheDefaultAllowance(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	d := mustAdmit(t, f.gate, f.req)
	f.merged()

	receipt, err := f.gate.Complete(d, f.req)
	if err != nil {
		t.Fatalf("the default allowance refused an ordinary completion: %v", err)
	}
	if receipt == nil || receipt.Digest == "" {
		t.Fatalf("default-allowance receipt is empty: %+v", receipt)
	}
	if _, err := os.Stat(hsync.ReceiptPath(f.dir, testRef)); err != nil {
		t.Fatalf("the default allowance did not seal a receipt: %v", err)
	}
}

// reconcileFixture clones the topology the normal reconcile path already uses:
// an equivalent patch landed under a new object id, main advanced past it.
func reconcileFixture(t *testing.T) (*Gate, Request, string) {
	t.Helper()
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")
	landedEquiv := rewriteOnto(t, dir, "landed", base, []string{candidate})
	if landedEquiv == candidate {
		t.Fatal("fixture did not rewrite the candidate sha; the test would prove nothing")
	}
	run(t, dir, "git", "checkout", "-q", "landed")
	advanced := commit(t, dir, "later.txt", "unrelated\n", "unrelated advance")

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
	return g, req, dir
}

// ReconcileLanded, NORMAL path: proof and identity share one allowance, and an
// exhausted one stops before the seal.
func TestReconcileLandedPublicEntryStopsOnTheSharedAllowance(t *testing.T) {
	g, req, dir := reconcileFixture(t)
	g.ProofBudget = ProofBudget{MaxCommands: 1}

	receipt, err := g.ReconcileLanded(req)
	if err == nil {
		t.Fatal("ReconcileLanded sealed a receipt on an exhausted allowance")
	}
	if !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands", err)
	}
	if receipt != nil {
		t.Fatalf("a refused reconcile returned a receipt: %+v", receipt)
	}
	receiptAbsent(t, dir)
}

// Same fixture, same public path, default allowance: still reconciles.
func TestReconcileLandedPublicEntrySucceedsOnTheDefaultAllowance(t *testing.T) {
	g, req, dir := reconcileFixture(t)
	receipt, err := g.ReconcileLanded(req)
	if err != nil {
		t.Fatalf("the default allowance refused an ordinary reconcile: %v", err)
	}
	if receipt == nil || receipt.Digest == "" {
		t.Fatalf("default-allowance receipt is empty: %+v", receipt)
	}
	if _, err := os.Stat(hsync.ReceiptPath(dir, testRef)); err != nil {
		t.Fatalf("the default allowance did not seal a receipt: %v", err)
	}
}

// ReconcileLanded, REDUCED path: it has its own proof and identity stages, and
// they must spend the same one allowance.
// reducedFixture is the reduced-provenance topology, built fresh per test.
// Reusing one Gate for the baseline AND the refusal would let the idempotent
// replay path answer the second call from the receipt on disk, without ever
// reaching the git the allowance is supposed to bound.
func reducedFixture(t *testing.T) (*Gate, Request, string) {
	t.Helper()
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate")
	landed := rewriteOnto(t, dir, "landed", base, []string{candidate})
	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)
	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{OriginMain: StaticProbe(landed)},
	}
	req := Request{Ref: testRef, CandidateSHA: candidate, BaseSHA: base,
		ReducedProvenance: &ReducedProvenance{PullRequest: 2864, VerifyLanded: true}}
	return g, req, dir
}

// ReconcileLanded, REDUCED path: its own proof and identity stages must spend
// the same one allowance, and stop before the seal.
func TestReconcileLandedReducedPublicEntryStopsOnTheSharedAllowance(t *testing.T) {
	g, req, dir := reducedFixture(t)
	g.ProofBudget = ProofBudget{MaxCommands: 1}

	receipt, err := g.ReconcileLanded(req)
	if err == nil {
		t.Fatal("the reduced path sealed a receipt on an exhausted allowance")
	}
	if !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands from the reduced path", err)
	}
	if receipt != nil {
		t.Fatalf("a refused reduced reconcile returned a receipt: %+v", receipt)
	}
	receiptAbsent(t, dir)
}

// A FRESH reduced fixture on the default allowance still reconciles.
func TestReconcileLandedReducedPublicEntrySucceedsOnTheDefaultAllowance(t *testing.T) {
	g, req, dir := reducedFixture(t)
	if _, err := g.ReconcileLanded(req); err != nil {
		t.Fatalf("the default allowance refused the reduced path: %v", err)
	}
	if _, err := os.Stat(hsync.ReceiptPath(dir, testRef)); err != nil {
		t.Fatalf("the default allowance did not seal a reduced receipt: %v", err)
	}
}

// followUpFixture builds TWO deliveries on one task, which is what a follow-up
// actually is. The first lands and is sealed; the second is reviewed on a base
// that CONTAINS the first delivery, and names a DIFFERENT candidate.
//
// Both are production requirements, not fixture taste: validatePriorReceipt
// refuses a follow-up that reuses the prior candidate, and refuses one whose
// reviewed base does not contain the prior merge.
func followUpFixture(t *testing.T) (*Gate, Request, Request, string) {
	t.Helper()
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")
	landedEquiv := rewriteOnto(t, dir, "landed", base, []string{candidate})
	if landedEquiv == candidate {
		t.Fatal("fixture did not rewrite the candidate sha; the test would prove nothing")
	}
	run(t, dir, "git", "checkout", "-q", "landed")
	advanced := commit(t, dir, "later.txt", "unrelated\n", "unrelated advance")

	// The follow-up is reviewed on the advanced tip, so the first delivery is
	// contained in its base however the proof named the prior merge.
	run(t, dir, "git", "checkout", "-q", "-b", "followup", advanced)
	second := commit(t, dir, "c.txt", "three\n", "follow-up work")
	if landed := rewriteOnto(t, dir, "landed-followup", advanced, []string{second}); landed == second {
		t.Fatal("fixture did not rewrite the follow-up candidate sha")
	}

	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)
	launch(t, l, second, "reviewer-b", "anthropic", "builder-session-1")
	verdict(t, l, second, "reviewer-b", reviewledger.VerdictPASS)

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
	first := okRequest(base, candidate)
	first.Mode = ModeRebase
	follow := okRequest(advanced, second)
	follow.Mode = ModeRebase

	return g, first, follow, dir
}

// landFollowUp reports the second delivery as landed. Kept separate from the
// fixture so the prior receipt is minted against the state that existed when
// the FIRST delivery landed, exactly as a real follow-up runs.
func landFollowUp(t *testing.T, g *Gate, dir, secondCandidate string) {
	t.Helper()
	g.Live.OriginMain = StaticProbe(revParse(t, dir, "landed-followup"))
	g.Live.CandidateHead = StaticProbe(secondCandidate)
}

// FOLLOW-UP path: validatePriorReceipt reads the repository identity, resolves
// two commits and checks ancestry. All of that ran outside the entry's
// allowance before review 212; it must now spend the entry's own.
func TestFollowUpPublicEntryStopsOnTheSharedAllowance(t *testing.T) {
	g, first, follow, dir := followUpFixture(t)

	// A prior receipt is required to reach the follow-up path at all. Minting it
	// through the public entry on the default allowance is also this test's
	// success witness: the fixture reconciles before anything is constrained.
	prior, err := g.ReconcileLanded(first)
	if err != nil {
		t.Fatalf("minting the prior receipt: %v", err)
	}
	if prior == nil || prior.Digest == "" {
		t.Fatal("no prior receipt to follow up from")
	}
	landFollowUp(t, g, dir, follow.CandidateSHA)

	follow.PriorReceiptDigest = prior.Digest
	g.ProofBudget = ProofBudget{MaxCommands: 1}

	receipt, err := g.ReconcileLanded(follow)
	if !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands from the follow-up path", err)
	}
	if receipt != nil {
		t.Fatalf("a refused follow-up returned a receipt: %+v", receipt)
	}
	// The prior receipt must still be exactly the one that was sealed: a
	// refused follow-up may neither supersede it nor write a new one.
	still, err := hsync.LoadReceipt(hsync.ReceiptPath(dir, testRef))
	if err != nil {
		t.Fatalf("prior receipt unreadable after a refused follow-up: %v", err)
	}
	if still.Digest != prior.Digest {
		t.Fatalf("a refused follow-up rewrote the sealed receipt: %s -> %s", prior.Digest, still.Digest)
	}
}

// The SAME follow-up request on the default allowance is NOT stopped by the
// command budget. Without this the refusal above could be caused by a
// malformed request rather than by the allowance, and would prove nothing.
func TestFollowUpPublicEntryIsNotBudgetStoppedOnTheDefaultAllowance(t *testing.T) {
	g, first, follow, dir := followUpFixture(t)

	prior, err := g.ReconcileLanded(first)
	if err != nil {
		t.Fatalf("minting the prior receipt: %v", err)
	}
	landFollowUp(t, g, dir, follow.CandidateSHA)
	follow.PriorReceiptDigest = prior.Digest

	if _, err := g.ReconcileLanded(follow); errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("the default allowance ran out of git commands on the follow-up path: %v", err)
	}
}
