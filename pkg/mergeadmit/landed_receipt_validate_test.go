package mergeadmit

import (
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-831, producer half. pkg/sync proves the shipped CONSUMER accepts a sealed
// carrier; it cannot prove any producer puts one there. The review that found
// the split Proof.MergeSHA/ContentSHA defect found it as a FIELD COPY that was
// missing, and a test which hands the consumer a hand-built receipt passes with
// every producer copy deleted.
//
// These two drive the real producer end to end: a git repository shaped like a
// pull-request landing, a real review ledger, a real gate, the real
// ReconcileLanded, the receipt it actually persisted read back from disk, and
// then the PUBLIC CompletionReceipt.Validate -- the same gate `herd approve`
// calls. Nothing is stubbed and no validation is bypassed.
//
// Gate.Complete is deliberately NOT covered here. Its producer is Prove, which
// binds PatchID to MergeSHA and never sets ContentSHA on any of its three
// modes, so its copy of the field cannot be observed to differ from omitting
// it. A control there would report coverage that does not exist.

// prLandingRepo builds the landing shape and returns the repository plus the
// four identities a receipt has to bind:
//
//	c0 ─────────────── base ──────────────── merge     (main, and origin/main)
//	 └── carrier ──── candidate ───────────────┘       (the pull request's line)
//
// Every property that makes this the shape under test is asserted, so a
// producer test cannot pass against a topology that could not exhibit the bug:
//
//   - the reviewed base is NOT an ancestor of the carrier: the commit that
//     holds the reviewed patch sits off the integrated line, which is what
//     forces selection to promote to the merge and splits ContentSHA from
//     MergeSHA in the first place;
//   - the reviewed tip DOES contain the base, because the pull request took
//     main in before it landed;
//   - neither the reviewed tip nor the landing merge has a patch of its own,
//     so a receipt binding content to MergeSHA alone is exactly the receipt
//     the consumer refuses;
//   - the reviewed bytes are genuinely in the merge's tree.
func prLandingRepo(t *testing.T) (dir, base, carrier, candidate, mergeCommit string) {
	t.Helper()
	dir = gitRepo(t)
	c0 := commit(t, dir, "a.txt", "root\n", "c0")

	// The pull request's own line starts before the reviewed base exists.
	run(t, dir, "git", "checkout", "-q", "-b", "pr", c0)
	carrier = commit(t, dir, "reviewed.txt", "reviewed content\n", "reviewed candidate work")

	// Main advances to the reviewed base.
	run(t, dir, "git", "checkout", "-q", "main")
	base = commit(t, dir, "base.txt", "base advanced\n", "unrelated main work (reviewed base)")

	// The pull request takes the base in. Its tip is what was reviewed.
	run(t, dir, "git", "checkout", "-q", "pr")
	run(t, dir, "git", "merge", "--no-ff", "--no-edit", "-m", "bring the reviewed base into the pull request", base)
	candidate = revParse(t, dir, "HEAD")

	// The landing: a merge commit carrying no patch of its own.
	mergeCommit = githubEmptyMerge(t, dir, base, candidate, "Merge pull request #843")
	// The consumer requires MergeSHA to be an ancestor of origin/main.
	run(t, dir, "git", "update-ref", "refs/remotes/origin/main", mergeCommit)

	if ancestorOK(t, dir, base, carrier) {
		t.Fatal("fixture invalid: the reviewed base is an ancestor of the carrier, so no carrier would ever be sealed")
	}
	if !ancestorOK(t, dir, base, candidate) {
		t.Fatal("fixture invalid: the reviewed tip does not contain the reviewed base")
	}
	if !ancestorOK(t, dir, carrier, mergeCommit) || !ancestorOK(t, dir, base, mergeCommit) {
		t.Fatal("fixture invalid: the merge does not contain both the reviewed base and the carrier")
	}
	if _, err := hsync.PatchID(dir, candidate); err == nil {
		t.Fatal("fixture invalid: the reviewed tip carries a patch of its own, so the content would not come from the carrier")
	}
	if _, err := hsync.PatchID(dir, mergeCommit); err == nil {
		t.Fatal("fixture invalid: the merge carries a patch of its own, so a merge-bound receipt would already validate")
	}
	return dir, base, carrier, candidate, mergeCommit
}

// assertSealedCarrierReceipt is the contract both producer paths owe. label
// names the path, so each control anchors on its own assertion text.
//
// st is the durable lifecycle state Validate requires of a full-provenance
// receipt, and nil for a reduced one -- reduced receipts omit the
// dispatch-derived fields on purpose and Validate returns before that check.
func assertSealedCarrierReceipt(t *testing.T, label, dir string, receipt *hsync.CompletionReceipt,
	base, carrier, candidate, mergeCommit string, st *lifecycle.TaskState) {
	t.Helper()
	if receipt == nil {
		t.Fatalf("%s producer returned no receipt", label)
	}
	// The field copy itself. This is the assertion the drop-the-copy control
	// anchors on: with `ContentSHA: proof.ContentSHA` removed from the
	// producer, the receipt binds content to the merge and dies right here.
	if receipt.ContentSHA != carrier {
		t.Fatalf("%s producer did not seal the content carrier: receipt content_sha is %q, want the carrier %s",
			label, receipt.ContentSHA, carrier)
	}
	if receipt.MergeSHA != mergeCommit {
		t.Fatalf("%s producer sealed integration sha %q, want the merge commit %s",
			label, receipt.MergeSHA, mergeCommit)
	}
	if receipt.MergeSHA == receipt.ContentSHA {
		t.Fatalf("%s receipt did not separate content from integration, so this fixture proves nothing", label)
	}
	if receipt.CandidateSHA != candidate {
		t.Fatalf("%s receipt candidate %q is not the reviewed tip %s", label, receipt.CandidateSHA, candidate)
	}
	if receipt.BaseSHA != base {
		t.Fatalf("%s receipt base %q is not the reviewed base %s", label, receipt.BaseSHA, base)
	}
	wantPatch, err := hsync.PatchID(dir, carrier)
	if err != nil {
		t.Fatalf("%s: patch id of the carrier: %v", label, err)
	}
	if receipt.PatchID != wantPatch {
		t.Fatalf("%s receipt patch id %q is not the carrier's patch %s", label, receipt.PatchID, wantPatch)
	}

	// The digest is recomputed here rather than trusted, and shown to COVER the
	// carrier: strip the field and the digest must move, or the carrier could
	// be removed from a sealed receipt without detection.
	if receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest() {
		t.Fatalf("%s receipt digest %q is not the digest of its own contents", label, receipt.Digest)
	}
	stripped := *receipt
	stripped.ContentSHA = ""
	if stripped.ComputeDigest() == receipt.Digest {
		t.Fatalf("%s receipt digest does not cover content_sha", label)
	}

	// ReconcileLanded returns what it read back from disk, so this proves the
	// field survived serialization and is not merely an in-memory struct field.
	raw, err := os.ReadFile(hsync.ReceiptPath(dir, receipt.TaskRef))
	if err != nil {
		t.Fatalf("%s: read the persisted receipt: %v", label, err)
	}
	if !strings.Contains(string(raw), `"content_sha"`) || !strings.Contains(string(raw), carrier) {
		t.Fatalf("%s persisted receipt does not carry content_sha for %s:\n%s", label, short(carrier), raw)
	}

	// The whole point: the SHIPPED consumer gate accepts what the producer
	// minted. Not a helper, not a proof object -- the exported Validate.
	if err := receipt.Validate(dir, testRef, st); err != nil {
		t.Fatalf("%s public Validate refused the sealed receipt: %v", label, err)
	}

	// And it accepts it BECAUSE of the carrier. Drop the field, re-seal so the
	// digest is honest about the contents, and the same gate must refuse: that
	// is what makes the acceptance above evidence rather than a coincidence.
	without := *receipt
	without.ContentSHA = ""
	without.Seal()
	if err := without.Validate(dir, testRef, st); err == nil {
		t.Fatalf("%s public Validate accepted a receipt with no content carrier, so a missing producer copy would be invisible", label)
	}
}

// The full-provenance producer path: ReconcileLanded's complete receipt, with
// the review ledger's own admission evidence and the durable lifecycle state
// Validate demands of a non-reduced receipt.
func TestReconcileLandedSealsThePullRequestCarrierAndPublicValidateAcceptsIt(t *testing.T) {
	dir, base, carrier, candidate, mergeCommit := prLandingRepo(t)
	ledger := newLedger(t, dir)
	launch(t, ledger, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, ledger, candidate, "reviewer-a", reviewledger.VerdictPASS)

	gate := &Gate{
		RepoDir: dir, Ledger: ledger, Policy: testPolicy(),
		Live: LiveState{OriginMain: StaticProbe(mergeCommit)},
	}
	receipt, err := gate.ReconcileLanded(okRequest(base, candidate))
	if err != nil {
		t.Fatalf("ReconcileLanded on a pull-request landing: %v", err)
	}

	state := &lifecycle.TaskState{
		TaskRef:         hsync.NormalizeRef(testRef),
		State:           lifecycle.StateIntegrated,
		LeaseGeneration: 3,
		CandidateSHA:    candidate,
	}
	assertSealedCarrierReceipt(t, "full-provenance", dir, receipt, base, carrier, candidate, mergeCommit, state)
}

// The reduced-provenance producer path: a SEPARATE receipt literal in
// reconcileLandedReduced, which is the path an actual `--verify-landed` pull
// request reconciliation takes. Its copy of the carrier is its own, and the
// full-provenance control cannot speak for it.
func TestReconcileLandedReducedSealsThePullRequestCarrierAndPublicValidateAcceptsIt(t *testing.T) {
	dir, base, carrier, candidate, mergeCommit := prLandingRepo(t)
	ledger := newLedger(t, dir)
	launch(t, ledger, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, ledger, candidate, "reviewer-a", reviewledger.VerdictPASS)

	gate := &Gate{
		RepoDir: dir, Ledger: ledger, Policy: testPolicy(),
		Live: LiveState{OriginMain: StaticProbe(mergeCommit)},
	}
	request := okRequest(base, candidate)
	request.ReducedProvenance = &ReducedProvenance{PullRequest: 843, VerifyLanded: true}
	receipt, err := gate.ReconcileLanded(request)
	if err != nil {
		t.Fatalf("reduced ReconcileLanded on a pull-request landing: %v", err)
	}
	if receipt.ProvenanceMode != hsync.ProvenanceReduced {
		t.Fatalf("reduced-provenance producer minted a %q receipt", receipt.ProvenanceMode)
	}
	if receipt.PullRequest != 843 {
		t.Fatalf("reduced-provenance receipt names pull request %d, not 843", receipt.PullRequest)
	}
	// A reduced receipt is validated with NO lifecycle state on purpose.
	assertSealedCarrierReceipt(t, "reduced-provenance", dir, receipt, base, carrier, candidate, mergeCommit, nil)
}
