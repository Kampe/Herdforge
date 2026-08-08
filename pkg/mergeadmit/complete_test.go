package mergeadmit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// completeFixture builds a REAL git repository with a real merge in it, plus a
// ledger holding a valid independent PASS for the candidate. Everything is
// hermetic: no network, no origin remote, no ambient git config.
type completeFixture struct {
	dir       string
	gate      *Gate
	req       Request
	base      string
	candidate string
	landed    string
}

func newCompleteFixture(t *testing.T, mode Mode) *completeFixture {
	t.Helper()
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")

	var landed string
	switch mode {
	case ModeMerge:
		run(t, dir, "git", "checkout", "-q", "main")
		run(t, dir, "git", "merge", "-q", "--no-ff", "-m", "merge work", "work")
		landed = revParse(t, dir, "HEAD")
	case ModeRebase:
		landed = rewriteOnto(t, dir, "landed", base, []string{candidate})
	case ModeSquash:
		run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
		run(t, dir, "git", "merge", "-q", "--squash", "work")
		run(t, dir, "git", "commit", "-q", "-m", "squashed")
		landed = revParse(t, dir, "HEAD")
	}

	l := newLedger(t, dir)
	launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)

	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{
			OriginMain:    StaticProbe(base), // pre-merge; flipped to landed below
			CandidateHead: StaticProbe(candidate),
			Mergeable:     StaticProbe("CLEAN"),
			TaskRevision:  StaticProbe(testRevision),
			Checks:        func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
		},
	}
	req := okRequest(base, candidate)
	req.Mode = mode
	return &completeFixture{dir: dir, gate: g, req: req, base: base, candidate: candidate, landed: landed}
}

// merged flips the live integration tip to post-merge state, which is what the
// probe would report once the merge has actually happened.
func (f *completeFixture) merged() { f.gate.Live.OriginMain = StaticProbe(f.landed) }

// A completed merge must leave behind exactly the receipt pkg/sync.BoardDone
// demands. Before FAC-156 nothing in production called WriteReceipt at all, so
// `herd approve` could only ever close a card by manual override.
func TestCompleteMintsAReceiptBoardDoneAccepts(t *testing.T) {
	for _, mode := range []Mode{ModeMerge, ModeRebase, ModeSquash} {
		t.Run(string(mode), func(t *testing.T) {
			f := newCompleteFixture(t, mode)
			d := mustAdmit(t, f.gate, f.req)
			f.merged()

			receipt, err := f.gate.Complete(d, f.req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}

			// The digest must be self-consistent, or BoardDone refuses it as
			// tampered.
			if receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest() {
				t.Fatal("receipt digest does not match its own contents")
			}
			// The content binding BoardDone recomputes: MergeSHA must carry
			// exactly the patch the receipt names.
			landedPatch, err := hsync.PatchID(f.dir, receipt.MergeSHA)
			if err != nil {
				t.Fatalf("patch id of receipt merge sha: %v", err)
			}
			if landedPatch != receipt.PatchID {
				t.Fatalf("receipt patch %s is not the patch on merge sha %s (%s)",
					short(receipt.PatchID), short(receipt.MergeSHA), short(landedPatch))
			}
			if receipt.Verdict != "PASS" || receipt.IntegrationResult != hsync.IntegrationMerged {
				t.Fatalf("receipt verdict=%q integration=%q", receipt.Verdict, receipt.IntegrationResult)
			}
			if receipt.AuthorFamily == receipt.ReviewerFamily {
				t.Fatal("receipt records a self-verdict")
			}
			if receipt.VerificationDigest != testVfy {
				t.Fatalf("receipt verification digest = %q, want the ADMITTED verdict's %q",
					receipt.VerificationDigest, testVfy)
			}
			// It must be on disk where BoardDone looks for it.
			if _, err := os.Stat(hsync.ReceiptPath(f.dir, testRef)); err != nil {
				t.Fatalf("receipt not at the path BoardDone reads: %v", err)
			}
		})
	}
}

// THE LOOP-CLOSING TEST. Everything else in this file checks fields; this one
// hands the minted receipt to the ACTUAL validator BoardDone gates on and
// requires it to pass. If Complete ever mints something BoardDone would
// refuse, the two halves of the pipeline have drifted apart and `herd approve`
// silently falls back to manual override again — which is the state FAC-156
// found the fleet in.
func TestCompleteReceiptSatisfiesBoardDoneValidator(t *testing.T) {
	for _, mode := range []Mode{ModeMerge, ModeRebase, ModeSquash} {
		t.Run(string(mode), func(t *testing.T) {
			f := newCompleteFixture(t, mode)
			d := mustAdmit(t, f.gate, f.req)
			f.merged()

			// Validate reads origin/main. Point the remote-tracking ref at the
			// landed tip locally — this is a ref write, not a fetch.
			run(t, f.dir, "git", "update-ref", "refs/remotes/origin/main", f.landed)

			receipt, err := f.gate.Complete(d, f.req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}

			st := &lifecycle.TaskState{
				TaskRef:         testRef,
				State:           lifecycle.StateIntegrated,
				LeaseGeneration: f.req.LeaseGeneration,
				CandidateSHA:    f.candidate,
			}
			if err := receipt.Validate(f.dir, testRef, st); err != nil {
				t.Fatalf("BoardDone would REFUSE the receipt this merge minted: %v", err)
			}

			// And the validator must still be capable of refusing: a receipt
			// whose lease generation is stale is not closable.
			stale := *st
			stale.LeaseGeneration = f.req.LeaseGeneration + 1
			if err := receipt.Validate(f.dir, testRef, &stale); err == nil {
				t.Fatal("validator accepted a stale lease generation; it is not actually gating")
			}
		})
	}
}

// Delivery is idempotent: a replay after a crash finishes the job rather than
// minting a second receipt or double-spending the admission.
func TestCompleteIsIdempotent(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	d := mustAdmit(t, f.gate, f.req)
	f.merged()

	first, err := f.gate.Complete(d, f.req)
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	second, err := f.gate.Complete(d, f.req)
	if err != nil {
		t.Fatalf("replayed Complete: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("replay minted a different receipt: %s vs %s", short(first.Digest), short(second.Digest))
	}
}

// A receipt may only be minted from an admitted decision. This is what stops a
// caller merging by hand and asking for the paperwork afterwards.
func TestCompleteRefusesWithoutAdmittedDecision(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	f.merged()

	// The refused decision is otherwise COMPLETE — right candidate, right
	// base, a verification digest present — so the only thing that can stop it
	// is the Admitted flag itself. An under-populated decision would be caught
	// by a later check and the test would pass for the wrong reason.
	for name, d := range map[string]*Decision{
		"nil decision": nil,
		"refused decision": {
			Admitted: false, Code: CodeLedgerRefused, CandidateSHA: f.candidate, BaseSHA: f.base,
			Mode: ModeMerge, Tier: "R3", ReviewerFam: "openai", VerificationDigest: testVfy,
		},
		"admitted but carries no verification digest": {
			Admitted: true, CandidateSHA: f.candidate, BaseSHA: f.base, Mode: ModeMerge,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.gate.Complete(d, f.req); err == nil {
				t.Fatal("Complete minted a receipt it must refuse")
			}
			if _, err := os.Stat(hsync.ReceiptPath(f.dir, testRef)); err == nil {
				t.Fatal("a refused completion still wrote a receipt to disk")
			}
		})
	}
}

// A decision for a DIFFERENT candidate is not authority for this one.
//
// The second candidate here is a REAL commit that genuinely landed, so its
// integration proof would succeed on its own. That is the point: without the
// binding check an admitted decision becomes a bearer token, and candidate B
// would be completed using candidate A's reviewer, risk tier, and
// verification digest. A fake sha would have been caught by Prove instead and
// the test would prove nothing.
func TestCompleteRefusesDecisionForAnotherCandidate(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")

	run(t, dir, "git", "checkout", "-q", "-b", "work-a")
	candidateA := commit(t, dir, "a-work.txt", "alpha\n", "candidate A")
	run(t, dir, "git", "checkout", "-q", "-b", "work-b", base)
	candidateB := commit(t, dir, "b-work.txt", "bravo\n", "candidate B")

	// BOTH land on main, so both have a valid ancestry proof.
	run(t, dir, "git", "checkout", "-q", "main")
	run(t, dir, "git", "merge", "-q", "--no-ff", "-m", "merge A", "work-a")
	run(t, dir, "git", "merge", "-q", "--no-ff", "-m", "merge B", "work-b")
	landed := revParse(t, dir, "HEAD")

	l := newLedger(t, dir)
	launch(t, l, candidateA, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, candidateA, "reviewer-a", reviewledger.VerdictPASS)

	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{
			OriginMain: StaticProbe(base), CandidateHead: StaticProbe(candidateA),
			Mergeable: StaticProbe("CLEAN"), TaskRevision: StaticProbe(testRevision),
			Checks: func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
		},
	}
	reqA := okRequest(base, candidateA)
	d := mustAdmit(t, g, reqA)
	g.Live.OriginMain = StaticProbe(landed)

	// Confirm the fixture: candidate B's own proof is sound, so only the
	// decision binding can refuse this.
	if _, err := Prove(dir, ProofRequest{Mode: ModeMerge, BaseSHA: base, CandidateSHA: candidateB, LandedSHA: landed}); err != nil {
		t.Fatalf("fixture: candidate B should prove cleanly, got %v", err)
	}

	reqB := okRequest(base, candidateB)
	if _, err := g.Complete(d, reqB); err == nil {
		t.Fatal("an admitted decision for candidate A completed candidate B: the decision is a bearer token")
	}
}

// ISOLATES THE READ-BACK. A write that reports success but persists something
// else is exactly what BoardDone's provider read-back exists for, and the same
// discipline has to hold for our own filesystem. The seam simulates that
// write; without it this check could never fail and would not be tested.
func TestCompleteRefusesWhenTheReceiptDoesNotReadBack(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	d := mustAdmit(t, f.gate, f.req)
	f.merged()

	original := writeReceipt
	t.Cleanup(func() { writeReceipt = original })
	writeReceipt = func(repoDir string, r *hsync.CompletionReceipt) error {
		// Report success, persist a receipt for a different lease generation.
		tampered := *r
		tampered.LeaseGeneration = r.LeaseGeneration + 41
		return original(repoDir, &tampered)
	}

	_, err := f.gate.Complete(d, f.req)
	if err == nil {
		t.Fatal("Complete accepted a receipt that did not read back as what it sealed")
	}
	if !strings.Contains(err.Error(), CodeReceiptReadback) {
		t.Fatalf("refusal did not carry the structured read-back code: %v", err)
	}
}

// If the merge did not actually land the reviewed content, no receipt exists —
// the proof failure is terminal, not a warning.
func TestCompleteRefusesWhenTheProofFails(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	d := mustAdmit(t, f.gate, f.req)
	// The integration tip advanced to something that never carried the
	// candidate at all.
	run(t, f.dir, "git", "checkout", "-q", "-B", "elsewhere", f.base)
	unrelated := commit(t, f.dir, "z.txt", "unrelated\n", "someone else")
	f.gate.Live.OriginMain = StaticProbe(unrelated)

	_, err := f.gate.Complete(d, f.req)
	if err == nil {
		t.Fatal("Complete minted a receipt for a merge that never landed the candidate")
	}
	if !strings.Contains(err.Error(), CodeProofFailed) {
		t.Fatalf("refusal did not carry the structured proof-failure code: %v", err)
	}
	if _, statErr := os.Stat(hsync.ReceiptPath(f.dir, testRef)); statErr == nil {
		t.Fatal("a failed proof still wrote a receipt")
	}
}

// A post-merge probe failure must not be papered over: with no authoritative
// read of where the merge landed, there is nothing to prove against.
func TestCompleteRefusesWhenPostMergeProbeFails(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	d := mustAdmit(t, f.gate, f.req)
	f.gate.Live.OriginMain = StaticProbe("")

	if _, err := f.gate.Complete(d, f.req); err == nil {
		t.Fatal("Complete proceeded on an empty post-merge read")
	}
}

// Two different receipts for one card means something is wrong upstream.
// Overwriting hides it.
func TestCompleteRefusesToOverwriteADifferentReceipt(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	d := mustAdmit(t, f.gate, f.req)
	f.merged()

	path := hsync.ReceiptPath(f.dir, testRef)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	foreign := &hsync.CompletionReceipt{
		RepoID: "somewhere-else", TaskRef: testRef, TaskID: testTaskID,
		ProviderRevision: "rev-1", LeaseGeneration: 1,
		BaseSHA: shaBase, CandidateSHA: shaOld, MergeSHA: shaOld,
		PatchID: "p", AcceptanceDigest: "a", VerificationDigest: "v",
		RiskTier: "R1", AuthorFamily: "anthropic", ReviewerFamily: "openai",
		Verdict: "PASS", IntegrationResult: hsync.IntegrationMerged,
	}
	if err := hsync.WriteReceipt(f.dir, foreign); err != nil {
		t.Fatalf("seed foreign receipt: %v", err)
	}

	if _, err := f.gate.Complete(d, f.req); err == nil {
		t.Fatal("Complete silently overwrote a different receipt for the same card")
	}
	back, err := hsync.LoadReceipt(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if back.Digest != foreign.Digest {
		t.Fatal("the pre-existing receipt was overwritten anyway")
	}
}

// The admission is spent exactly once, and only AFTER the receipt is durable.
func TestCompleteSpendsTheAdmissionExactlyOnce(t *testing.T) {
	f := newCompleteFixture(t, ModeMerge)
	d := mustAdmit(t, f.gate, f.req)
	f.merged()

	if _, err := f.gate.Complete(d, f.req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Re-admitting the same candidate must now refuse: the admission is spent.
	f.gate.Live.OriginMain = StaticProbe(f.base)
	mustRefuse(t, f.gate, f.req, CodeLedgerRefused)
}

// An empty candidate produces no receipt. This is the shape that reopened
// FAC-156: PR #151 merged a branch holding only its anchor commit, and every
// downstream check passed because there was nothing there to be wrong.
func TestCompleteRefusesEmptyCandidateEndToEnd(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	// The "candidate" is the base itself: zero additions, zero files.
	l := newLedger(t, dir)
	launch(t, l, base, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, base, "reviewer-a", reviewledger.VerdictPASS)

	g := &Gate{
		RepoDir: dir, Ledger: l, Policy: testPolicy(),
		Live: LiveState{
			OriginMain: StaticProbe(base), CandidateHead: StaticProbe(base),
			Mergeable: StaticProbe("CLEAN"), TaskRevision: StaticProbe(testRevision),
			Checks: func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
		},
	}
	req := okRequest(base, base)
	d := mustAdmit(t, g, req) // the ledger has a valid PASS; admission is not where this dies

	if _, err := g.Complete(d, req); err == nil {
		t.Fatal("an empty candidate produced a completion receipt")
	}
	if _, err := os.Stat(hsync.ReceiptPath(dir, testRef)); err == nil {
		t.Fatal("an empty candidate wrote a receipt to disk")
	}
}
