package mergeadmit

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// pathSetReadArgs is the argument text of the reconstruction path-set read.
// That flag combination occurs at exactly ONE call site in this package
// (reconstruction.go), so a budget refusal naming it pins which stage was
// stopped instead of merely proving that something ran out of allowance.
const pathSetReadArgs = "--name-only -z"

// ancestryProbeArgs is what ancestorProven charges. The reconstruction
// ancestry loop is the first git any reconciled reconstruction spends: every
// check between the public entry and it is ledger or receipt-file work.
const ancestryProbeArgs = "merge-base --is-ancestor"

// reconstructionFixture builds the reanchored-documentation topology that
// TestReconcileReconstructedConsent already proves is admissible, and returns
// a gate whose ReconcileLanded seals on the default allowance.
//
// It is rebuilt for each allowance deliberately. A sealed receipt makes the
// next call idempotent, and that replay answers from disk without reaching
// git, so a gate reused across budgets would measure nothing.
func reconstructionFixture(t *testing.T) (*Gate, Request, string) {
	t.Helper()
	dir := gitRepo(t)
	base := commit(t, dir, "docs.md", "heading\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "reviewed")
	candidate := commit(t, dir, "docs.md", "heading\nreviewed row\n", "reviewed")
	run(t, dir, "git", "checkout", "-q", "-b", "reconstructed", base)
	newBase := commit(t, dir, "docs.md", "heading\nnew context\n", "context")
	rebuilt := commit(t, dir, "docs.md", "heading\nnew context\nreviewed row\n", "reanchor")
	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)
	if err := l.Reconstruction(reviewledger.ReconstructionOpts{SHA: rebuilt, CandidateSHA: candidate, ContentProof: "same row reanchored"}); err != nil {
		t.Fatal(err)
	}
	g := &Gate{RepoDir: dir, Ledger: l, Policy: testPolicy(), Live: LiveState{OriginMain: StaticProbe(rebuilt), OriginMainAt: StaticOriginProbe(rebuilt)}}
	req := Request{
		Ref:               testRef,
		CandidateSHA:      candidate,
		BaseSHA:           base,
		ReducedProvenance: &ReducedProvenance{PullRequest: 1, VerifyLanded: true},
		Reconstruction:    &ReconstructionBinding{SHA: rebuilt, BaseSHA: newBase},
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Event == string(reviewledger.EventReconstruction) {
			req.Reconstruction.AttestationDigest = ReconstructionDigest(row)
		}
	}
	if req.Reconstruction.AttestationDigest == "" {
		t.Fatal("fixture retained no reconstruction attestation to bind")
	}
	return g, req, dir
}

// The default allowance must still seal. Without this the refusal test below
// could pass against a fixture that never worked at all.
func TestReconcileLandedReconstructionSucceedsOnTheDefaultAllowance(t *testing.T) {
	g, req, dir := reconstructionFixture(t)
	receipt, err := g.ReconcileLanded(req)
	if err != nil {
		t.Fatalf("the reconstruction path did not seal on the default allowance: %v", err)
	}
	if receipt == nil || receipt.ReconstructedSHA != req.Reconstruction.SHA ||
		receipt.ReconstructionDigest != req.Reconstruction.AttestationDigest {
		t.Fatalf("reconstruction identities lost: %+v", receipt)
	}
	if _, err := os.Stat(hsync.ReceiptPath(dir, testRef)); err != nil {
		t.Fatalf("default allowance sealed no receipt on disk: %v", err)
	}
}

// A finite allowance must stop the public reconstruction route inside its own
// git stage, before any receipt is written, and say why.
//
// The stage boundary is DERIVED, not asserted as a count: the allowance is
// swept upward until the refusal names the path-set read that only
// reconstruction performs. Whatever ran before it is then observed. An
// unrelated earlier check cannot stand in for the reconstruction stage,
// because such a check would either not be charged at all or would be named
// by the refusal that the loop below inspects.
func TestReconcileLandedReconstructionAncestrySpendsTheSharedAllowance(t *testing.T) {
	const sweepLimit = 24
	refusals := make(map[int]string, sweepLimit)
	boundary := 0
	for n := 1; n <= sweepLimit; n++ {
		g, req, dir := reconstructionFixture(t)
		g.ProofBudget = ProofBudget{MaxCommands: n}
		receipt, err := g.ReconcileLanded(req)
		if err == nil {
			t.Fatalf("an allowance of %d git commands sealed a reconstruction receipt before the reconstruction path-set read was ever refused", n)
		}
		if !errors.Is(err, ErrProofBudgetCommands) {
			t.Fatalf("an allowance of %d stopped for something other than the command budget: %v", n, err)
		}
		if receipt != nil {
			t.Fatalf("an allowance of %d returned a receipt alongside its refusal", n)
		}
		receiptAbsent(t, dir)
		refusals[n] = err.Error()
		if strings.Contains(refusals[n], pathSetReadArgs) {
			boundary = n
			break
		}
	}
	if boundary == 0 {
		t.Fatalf("no allowance up to %d was stopped at the reconstruction path-set read; refusals so far: %v", sweepLimit, refusals)
	}
	// boundary commands ran before the first path-set read. The reconstruction
	// stage charges two ancestry probes before it, so anything less means they
	// reached git outside the allowance this invocation installed.
	if boundary < 2 {
		t.Fatalf("the reconstruction ancestry probes did not spend the shared allowance: at most %d git command(s) ran before the reconstruction path-set read, so its two bounded ancestry probes were not charged", boundary)
	}
	for n := 1; n < boundary; n++ {
		if !strings.Contains(refusals[n], ancestryProbeArgs) {
			t.Fatalf("git command %d before the reconstruction path-set read was not a bounded ancestry probe: %s", n+1, refusals[n])
		}
	}
}
