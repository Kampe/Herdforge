package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-831 review 6c93cc2b, blocker 2, through the PUBLIC ENTRY.
//
// runHarvestVerifyLanded is what ships: it resolves the surface, resolves the
// candidate, resolves the request, BUILDS THE GATE, and only then proves, seals
// and records. A fixture that calls the composition helper directly skips the
// constructor under test, so these drive the real entry and let the real
// buildMergeGate supply the allowance.
//
// The retired-carrier route is used deliberately: no worktree stands on the
// reviewed branch, the candidate is pinned explicitly, and the invoking checkout
// stands in -- which is the exact shape this repair was written for.

// publicEntryFixture prepares a repository where runHarvestVerifyLanded can run
// end to end: a real origin/clone with a squash landing, a reduced-provenance
// binding, and a ledger carrying an independent PASS for the candidate.
func publicEntryFixture(t *testing.T) (repo, candidate string, binding verifyLandedBinding) {
	t.Helper()
	repo, base, candidate, _ := landedPinFixture(t)
	t.Chdir(repo)

	// A missing config file yields the default protected policy, which is what
	// production uses when a repository has not overridden it.
	t.Setenv("HERD_CONFIG_PATH", filepath.Join(repo, "absent-herd.yaml"))
	ledgerPath := filepath.Join(repo, ".herd", "review-ledger.jsonl")
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}

	ledger, err := reviewledger.NewReviewLedger(repo, ledgerPath)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if err := ledger.Record(reviewledger.RecordOpts{
		SHA: candidate, Reviewer: "reviewer-a", BuilderFamily: "anthropic", BuilderIdentity: "builder-1",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R3", Task: pinProofRef, Lease: "lease-1",
	}); err != nil {
		t.Fatalf("record launch: %v", err)
	}
	if _, err := ledger.Verdict(reviewledger.VerdictOpts{
		SHA: candidate, Reviewer: "reviewer-a", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", Task: pinProofRef, Lease: "lease-1",
		PatchURL: "patch-1", VfyDigest: "vfy-1", Artifact: "verdict.md", CandidateSHA: candidate,
	}); err != nil {
		t.Fatalf("write verdict: %v", err)
	}

	// No admission record is written. resolveVerifyLandedRequest returns reduced
	// provenance directly from the binding when --pr is present, which is the
	// shape a post-merge verify-landed reconciliation actually carries, and it
	// keeps the fixture from depending on a second durable store whose absence
	// reads as "no merge-admission ... and --task-id is missing" (CI 34749406649).
	return repo, candidate, verifyLandedBinding{Ref: pinProofRef, Candidate: candidate, BaseSHA: base, PullRequest: 1}
}

func publicEntryArtifacts(t *testing.T, repo string) (receipt, disposition bool) {
	t.Helper()
	if _, err := os.Stat(hsync.ReceiptPath(repo, pinProofRef)); err == nil {
		receipt = true
	}
	if d, err := hsync.ReadLandedDisposition(repo, pinProofRef); err == nil && d != nil {
		disposition = true
	}
	return receipt, disposition
}

// POSITIVE, default allowance, through the public entry. Without this the
// negative below could pass on an unrelated prerequisite failure -- a missing
// ledger row or an unreadable admission would produce the same "no artifacts".
func TestRunHarvestVerifyLandedSealsAndRecordsOnTheDefaultAllowance(t *testing.T) {
	repo, _, binding := publicEntryFixture(t)
	cliProofBudget = mergeadmit.ProofBudget{}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	if err := runHarvestVerifyLanded("work", binding); err != nil {
		t.Fatalf("the public entry could not complete a landing it should seal: %v", err)
	}
	assertReceiptAndDispositionAgree(t, repo)
}

// EXISTENCE IS NOT AGREEMENT. The receipt is the authority and the disposition
// is derived from it, so every identity they share must be equal -- a route that
// sealed one integration and advertised another would satisfy an existence
// check and still be wrong (review b70054a6).
func assertReceiptAndDispositionAgree(t *testing.T, repo string) {
	t.Helper()
	raw, err := os.ReadFile(hsync.ReceiptPath(repo, pinProofRef))
	if err != nil {
		t.Fatalf("a completed invocation sealed no receipt: %v", err)
	}
	var receipt hsync.CompletionReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("receipt unreadable: %v", err)
	}
	d, err := hsync.ReadLandedDisposition(repo, pinProofRef)
	if err != nil || d == nil {
		t.Fatalf("a completed invocation recorded no disposition: %v", err)
	}
	if d.CandidateSHA != receipt.CandidateSHA {
		t.Fatalf("disposition candidate %s does not bind the sealed receipt's %s", d.CandidateSHA, receipt.CandidateSHA)
	}
	if d.MergeSHA != receipt.MergeSHA {
		t.Fatalf("disposition integration %s does not bind the sealed receipt's %s", d.MergeSHA, receipt.MergeSHA)
	}
	if !strings.EqualFold(hsync.NormalizeRef(d.Ref), hsync.NormalizeRef(receipt.TaskRef)) {
		t.Fatalf("disposition ref %s does not bind the sealed receipt's %s", d.Ref, receipt.TaskRef)
	}
}

// OMITTED --candidate, through the public entry. This is the route that used to
// resolve identity with unbounded git before any allowance existed: with no
// explicit candidate the resolver falls to the ref's admitted PASS, and only
// then to HEAD. The explicit-candidate positive above stays, so a failure here
// is attributable to this path rather than to the fixture.
func TestRunHarvestVerifyLandedResolvesAnOmittedCandidateAndSeals(t *testing.T) {
	repo, _, binding := publicEntryFixture(t)
	binding.Candidate = ""
	cliProofBudget = mergeadmit.ProofBudget{}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	if err := runHarvestVerifyLanded("work", binding); err != nil {
		t.Fatalf("the public entry could not resolve an omitted candidate and seal: %v", err)
	}
	assertReceiptAndDispositionAgree(t, repo)
}

// The contract the repair exists for, at the public entry: an allowance that
// covers OBSERVATION and nothing more. Neither artifact may exist.
//
// The threshold is derived from an INDEPENDENT observation-only measurement, not
// from the whole composition. Measuring the composition and subtracting one is
// not an oracle: the very mutant this is meant to catch -- a seal that mints its
// own budget instead of spending this invocation's -- REDUCES what the whole
// composition costs, so the measured minimum collapses to the observation cost,
// "one less" then fails during observation with no artifacts, and the mutant
// survives a green test. A threshold that moves with the mutation cannot certify
// that the seal shared the original allowance.
//
// So the allowance is exactly what observation alone needs, and the test proves
// both halves explicitly: that observation COMPLETES on it, and that the
// invocation as a whole does NOT.
func TestRunHarvestVerifyLandedRecordsNothingWhenSealingExhausts(t *testing.T) {
	observation := observationOnlyCost(t)

	repo, _, binding := publicEntryFixture(t)
	// Half one, stated rather than assumed: on this allowance every stage of
	// this very fixture up to and including observation completes. Any failure
	// below is therefore after observation, which is what makes it a statement
	// about the seal.
	if err := observationCompletesOn(t, binding, observation); err != nil {
		t.Fatalf("fixture invalid: observation does not complete on %d commands: %v", observation, err)
	}

	cliProofBudget = mergeadmit.ProofBudget{MaxCommands: observation}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	err := runHarvestVerifyLanded("work", binding)
	if err == nil {
		t.Fatal("the seal completed on an allowance that only covers observation; it did not spend this invocation's budget")
	}
	receipt, disposition := publicEntryArtifacts(t, repo)
	if receipt {
		t.Fatal("a receipt was sealed by an invocation that exhausted its allowance")
	}
	if disposition {
		t.Fatal("a landed disposition was recorded for a receipt that was never minted")
	}
}

// observationCompletesOn runs the entry's PREFIX -- every stage the real
// invocation spends before it seals -- on the given allowance, sharing one
// context exactly as runHarvestVerifyLanded does.
//
// It has to be the whole prefix, not the observation alone: surface selection
// and candidate resolution now spend the same ledger, so an allowance measured
// from observation by itself would be short by what they cost and the
// invocation would stop BEFORE the seal for a reason that says nothing about
// the seal. The stages below are the entry's, in the entry's order.
func observationCompletesOn(t *testing.T, binding verifyLandedBinding, allowance int) error {
	t.Helper()
	prev := cliProofBudget
	cliProofBudget = mergeadmit.ProofBudget{MaxCommands: allowance}
	defer func() { cliProofBudget = prev }()

	gate, err := buildMergeGate(binding.Ref, binding.TaskID, 0)
	if err != nil {
		t.Fatalf("build gate: %v", err)
	}
	ctx, cancel := gate.ProofContext()
	defer cancel()

	surface, err := resolveVerifyLandedSurface(ctx, "work", binding, worktreeForBranch, invokingRepoRoot)
	if err != nil {
		return err
	}
	candidate, err := resolveVerifyLandedCandidate(ctx, surface.Dir, "work", binding)
	if err != nil {
		return err
	}
	req, err := resolveVerifyLandedRequest(binding, candidate)
	if err != nil {
		t.Fatalf("resolve request: %v", err)
	}
	_, err = observeVerifyLanded(ctx, surface.Dir, gate, req)
	return err
}

// observationOnlyCost is the smallest allowance on which everything UP TO AND
// INCLUDING observation completes, measured through the gate the CLI builds so
// the count matches what the real invocation spends before it seals.
//
// It is independent of the seal by construction: no control that this number
// arbitrates mutates a prefix stage, so it does not move when the seal does.
// That independence is the whole point -- measuring the whole composition and
// subtracting one would let a seal that mints its own budget lower the very
// threshold meant to catch it.
func observationOnlyCost(t *testing.T) int {
	t.Helper()
	for n := 1; n < 256; n++ {
		_, _, binding := publicEntryFixture(t)
		if observationCompletesOn(t, binding, n) == nil {
			return n
		}
	}
	t.Fatal("observation never completed within a sane allowance")
	return 0
}

// LIVE CARRIER, OMITTED --candidate. This is the resolution path review b70054a6
// found running OUTSIDE any allowance: with no explicit object the resolver
// reads the carrier, and before the repair it did so with bare exec.Command,
// before the budget the route later enforced even existed.
//
// A real linked worktree stands on the reviewed branch, so resolveVerifyLandedSurface
// takes the LIVE branch rather than the retired-carrier fallback -- the same
// lookup production uses, not an injected stub.
//
// unpinnedRef names a task the ledger holds no admitted PASS for, which is what
// drives resolution past the ledger and onto the carrier HEAD itself.
const unpinnedRef = "FAC-831-UNPINNED"

func liveCarrierFixture(t *testing.T, branch, startPoint string) (repo, carrier, candidate string, binding verifyLandedBinding) {
	t.Helper()
	repo, candidate, binding = publicEntryFixture(t)

	args := []string{"-C", repo, "worktree", "add", "-q", filepath.Join(filepath.Dir(repo), "carrier-"+branch)}
	if startPoint == "" {
		args = append(args, branch)
	} else {
		args = append(args, "-b", branch, startPoint)
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("add live carrier: %v\n%s", err, out)
	}
	// The carrier directory is whatever PRODUCTION's own lookup resolves, so the
	// fixture cannot pass by naming a path the shipped resolver would not find.
	carrier, err := worktreeForBranch(context.Background(), branch)
	if err != nil {
		t.Fatalf("fixture invalid: the carrier lookup could not run: %v", err)
	}
	if carrier == "" {
		t.Fatalf("fixture invalid: no live carrier resolves for branch %q", branch)
	}
	return repo, carrier, candidate, binding
}

// POSITIVE. A live carrier and an omitted --candidate still seal and record, and
// the two artifacts agree. Without this the refusals below could all pass
// against a route that refuses every live-carrier invocation.
func TestRunHarvestVerifyLandedSealsFromALiveCarrierWithAnOmittedCandidate(t *testing.T) {
	repo, _, _, binding := liveCarrierFixture(t, "work", "")
	binding.Candidate = ""
	cliProofBudget = mergeadmit.ProofBudget{}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	if err := runHarvestVerifyLanded("work", binding); err != nil {
		t.Fatalf("the public entry could not resolve from a live carrier and seal: %v", err)
	}
	assertReceiptAndDispositionAgree(t, repo)
}

// BOUNDED. The resolution itself is charged to THIS invocation's allowance.
//
// One command is enough for the carrier HEAD read and no more, so the route must
// stop inside resolution with the budget sentinel intact. This is the assertion
// the repair exists for: while resolution ran on bare exec.Command it could not
// stop here at all, and an independent per-helper allowance would not either --
// only spending the route's own ledger produces this refusal.
func TestRunHarvestVerifyLandedChargesCandidateResolutionToTheSharedAllowance(t *testing.T) {
	repo, _, _, binding := liveCarrierFixture(t, "work", "")
	binding.Candidate = ""
	binding.Ref = unpinnedRef
	cliProofBudget = mergeadmit.ProofBudget{MaxCommands: 1}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	err := runHarvestVerifyLanded("work", binding)
	if err == nil {
		t.Fatal("candidate resolution completed on an allowance of one command; it did not spend this invocation's budget")
	}
	if !errors.Is(err, mergeadmit.ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands through errors.Is", err)
	}
	if receipt, disposition := publicEntryArtifacts(t, repo); receipt || disposition {
		t.Fatalf("an invocation that stopped during resolution left artifacts (receipt=%v disposition=%v)", receipt, disposition)
	}
}

// ERROR. A failed origin fetch is a REFUSAL, not an answer. The old guard
// discarded the fetch status and then read a stale tracking ref, so an offline
// repository could report "HEAD is not contained in origin/main" from data that
// predated the landing -- which is exactly how a landed commit gets bound as the
// reviewed candidate.
func TestRunHarvestVerifyLandedRefusesResolutionWhenOriginCannotBeFetched(t *testing.T) {
	repo, _, _, binding := liveCarrierFixture(t, "work", "")
	binding.Candidate = ""
	binding.Ref = unpinnedRef
	cliProofBudget = mergeadmit.ProofBudget{}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	if out, err := exec.Command("git", "-C", repo, "remote", "set-url", "origin",
		filepath.Join(filepath.Dir(repo), "missing.git")).CombinedOutput(); err != nil {
		t.Fatalf("break origin: %v\n%s", err, out)
	}

	err := runHarvestVerifyLanded("work", binding)
	if err == nil {
		t.Fatal("a stale tracking ref answered the candidate-identity guard")
	}
	if !strings.Contains(err.Error(), "could not be fetched") || !strings.Contains(err.Error(), "--candidate") {
		t.Fatalf("err = %v, want a refusal naming the failed fetch and the --candidate remedy", err)
	}
	if receipt, disposition := publicEntryArtifacts(t, repo); receipt || disposition {
		t.Fatalf("a refused resolution left artifacts (receipt=%v disposition=%v)", receipt, disposition)
	}
}

// ERROR. A carrier whose HEAD is already contained in origin/main holds the
// LANDED commit, not the reviewed candidate (FAC-566). Resolving it would bind
// the merge SHA as the candidate, so it is refused with the remedy named.
func TestRunHarvestVerifyLandedRefusesACarrierHeadAlreadyContainedInOriginMain(t *testing.T) {
	repo, _, _, binding := liveCarrierFixture(t, "stale", "main")
	binding.Candidate = ""
	binding.Ref = unpinnedRef
	cliProofBudget = mergeadmit.ProofBudget{}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	err := runHarvestVerifyLanded("stale", binding)
	if err == nil {
		t.Fatal("a landed carrier head was resolved as the reviewed candidate")
	}
	if !strings.Contains(err.Error(), "already contained in origin/main") {
		t.Fatalf("err = %v, want the contained-HEAD refusal", err)
	}
	if receipt, disposition := publicEntryArtifacts(t, repo); receipt || disposition {
		t.Fatalf("a refused resolution left artifacts (receipt=%v disposition=%v)", receipt, disposition)
	}
}

// THE CARRIER LOOKUP IS PART OF THE INVOCATION, through the real route.
//
// Surface selection decides proof SCOPE, so a lookup that could not run must
// refuse rather than report "no worktree carries this branch" -- which is what
// authorises the invoking checkout to stand in. An allowance whose deadline has
// already passed makes the shipped lookup fail for a reason that is real and
// deterministic, without touching the repository.
//
// Two halves are asserted: the refusal NAMES the unanswered lookup, and it is
// not the absent-carrier answer. A route that collapsed the failure into "" would
// carry on from the retired-carrier branch and fail later with an unrelated
// message, leaving the scope question silently answered.
func TestRunHarvestVerifyLandedRefusesWhenTheCarrierLookupCannotRun(t *testing.T) {
	repo, _, binding := publicEntryFixture(t)
	cliProofBudget = mergeadmit.ProofBudget{Deadline: time.Nanosecond}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	err := runHarvestVerifyLanded("work", binding)
	if err == nil {
		t.Fatal("a carrier lookup that could not run still selected a proof surface")
	}
	if !strings.Contains(err.Error(), "whether a carrier worktree holds") {
		t.Fatalf("err = %v, want the refusal to name the unanswered carrier lookup", err)
	}
	if strings.Contains(err.Error(), "carrier retired") {
		t.Fatalf("a failed lookup was reported as an absent carrier: %v", err)
	}
	if receipt, disposition := publicEntryArtifacts(t, repo); receipt || disposition {
		t.Fatalf("an invocation that never selected a surface left artifacts (receipt=%v disposition=%v)", receipt, disposition)
	}
}
