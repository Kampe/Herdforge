package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// PR843/3d645a26: resolveVerifyLandedSurface used to prove the pinned object
// with its own `git cat-file -t` through exec.Command — no owned process group,
// no deadline, no command budget, no output budget — and it ran BEFORE the
// producer gate. These tests hold that guard at its new home: the bounded gate
// proof, which resolves every identity with `rev-parse --verify -q <sha>^{commit}`
// inside the one allowance the public entry installs.
//
// They drive the ACTUAL route: observeVerifyLanded(dir, gate, req), which is
// what runHarvestVerifyLanded calls, on a real hermetic origin/clone topology.

const pinProofRef = "FAC-843"

// pinProofBranch is the reviewed branch, and it NAMES THE TASK, which is the
// fleet's branch convention (`task/<ref>`) and what the shipped review
// authority matches on: rowNamesRef resolves a ref from a ledger row's branch,
// artifact or lane — never from its task field. A fixture whose branch did not
// name the ref recorded evidence that AdmittedPass cannot find, so the ref had
// no current admitted PASS and a retired carrier was correctly refused as
// unpinned (CI 34751721492).
const pinProofBranch = "task/" + pinProofRef

// landedPinFixture builds a squash landing whose reviewed branch is no longer
// checked out — the retired-carrier shape — and returns the identities.
func landedPinFixture(t *testing.T) (repo, base, candidate, landed string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo = filepath.Join(root, "repo")
	git := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "--bare", "-q", "-b", "main", origin)
	git(root, "clone", "-q", origin, repo)
	for _, kv := range [][2]string{{"user.name", "test"}, {"user.email", "test@example.invalid"}, {"commit.gpgsign", "false"}} {
		git(repo, "config", kv[0], kv[1])
	}
	commit := func(body, msg string) string {
		t.Helper()
		if e := os.WriteFile(filepath.Join(repo, "a"), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		git(repo, "add", "a")
		git(repo, "commit", "-q", "-m", msg)
		return git(repo, "rev-parse", "HEAD")
	}
	base = commit("base\n", "base")
	git(repo, "push", "-q", "origin", "main")
	git(repo, "checkout", "-q", "-b", pinProofBranch)
	commit("middle\n", "first")
	candidate = commit("final\n", "second")
	git(repo, "checkout", "-q", "main")
	git(repo, "merge", "--squash", pinProofBranch)
	git(repo, "commit", "-q", "-m", "squash")
	landed = git(repo, "rev-parse", "HEAD")
	git(repo, "push", "-q", "origin", "main")
	// Fixture bookkeeping -- the ledger, admissions, receipts and dispositions a
	// run writes -- lives under .herd/ and must not make the worktree DIRTY: the
	// landing proof refuses a dirty worktree, and that refusal would stand in for
	// the budget refusal these tests are about (CI 34749406649).
	if e := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(".herd/\n"), 0o600); e != nil {
		t.Fatal(e)
	}

	// The carrier is retired: no worktree stands on the reviewed branch, and
	// the invoking checkout is main. The candidate object is still here.
	return repo, base, candidate, landed
}

// receiptUnsealed fails if the proof step wrote a receipt. The seal itself is
// downstream of this call in runHarvestVerifyLanded (proof, then disposition,
// then ReconcileLanded); this asserts the proof stage writes nothing of its own.
func receiptUnsealed(t *testing.T, repo string) {
	t.Helper()
	if _, err := os.Stat(hsync.ReceiptPath(repo, pinProofRef)); err == nil {
		t.Fatal("the proof stage sealed a receipt")
	} else if !os.IsNotExist(err) {
		t.Fatalf("receipt path unreadable: %v", err)
	}
}

// POSITIVE, default allowance. Without this the refusals below could all pass
// against a gate that refuses everything.
func TestVerifyLandedGateProvesAPinnedRetiredCandidate(t *testing.T) {
	repo, base, candidate, landed := landedPinFixture(t)
	gate := &mergeadmit.Gate{RepoDir: repo, Live: mergeadmit.LiveState{OriginMainAt: originMainProbeContext(repo)}}
	req := mergeadmit.Request{Ref: pinProofRef, BaseSHA: base, CandidateSHA: candidate}

	proof, err := observeVerifyLandedUnderGate(t, repo, gate, req)
	if err != nil {
		t.Fatalf("a correct retired candidate was refused on the default allowance: %v", err)
	}
	if proof == nil || proof.MergeSHA != landed {
		t.Fatalf("proof did not name the landing %s: %+v", shortSHA12(landed), proof)
	}
}

// The object-presence guard, at its new home. A pin naming an object this
// repository does not hold is refused by the bounded proof.
func TestVerifyLandedGateRefusesAnAbsentPin(t *testing.T) {
	repo, base, _, _ := landedPinFixture(t)
	gate := &mergeadmit.Gate{RepoDir: repo, Live: mergeadmit.LiveState{OriginMainAt: originMainProbeContext(repo)}}
	req := mergeadmit.Request{Ref: pinProofRef, BaseSHA: base, CandidateSHA: strings.Repeat("0", 40)}

	if _, err := observeVerifyLandedUnderGate(t, repo, gate, req); err == nil {
		t.Fatal("a candidate absent from this repository was proved landed")
	}
	receiptUnsealed(t, repo)
}

// The object-TYPE guard, at its new home. A tree id resolves in git but is not
// a candidate; `^{commit}` is what refuses it.
func TestVerifyLandedGateRefusesANonCommitPin(t *testing.T) {
	repo, base, candidate, _ := landedPinFixture(t)
	tree := func() string {
		c := exec.Command("git", "rev-parse", candidate+"^{tree}")
		c.Dir = repo
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("resolve tree: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}()
	gate := &mergeadmit.Gate{RepoDir: repo, Live: mergeadmit.LiveState{OriginMainAt: originMainProbeContext(repo)}}
	req := mergeadmit.Request{Ref: pinProofRef, BaseSHA: base, CandidateSHA: tree}

	if _, err := observeVerifyLandedUnderGate(t, repo, gate, req); err == nil {
		t.Fatal("a tree id was proved landed as a candidate")
	}
	receiptUnsealed(t, repo)
}

// An empty pin must read as a missing pin, not as a repository problem. This is
// the semantics CI 34742740503 m03 established, preserved after the move:
// resolveCommit refuses a blank revision before spending any command.
func TestVerifyLandedGateRefusesAnEmptyPinAsAMissingPin(t *testing.T) {
	repo, base, _, _ := landedPinFixture(t)
	gate := &mergeadmit.Gate{RepoDir: repo, Live: mergeadmit.LiveState{OriginMainAt: originMainProbeContext(repo)}}
	req := mergeadmit.Request{Ref: pinProofRef, BaseSHA: base, CandidateSHA: "   "}

	_, err := observeVerifyLandedUnderGate(t, repo, gate, req)
	if err == nil {
		t.Fatal("an empty candidate identity was proved landed")
	}
	if !strings.Contains(err.Error(), "candidate revision is required") {
		t.Fatalf("err = %v, want the missing-pin refusal rather than a repository failure", err)
	}
	receiptUnsealed(t, repo)
}

// observationPrefixCost is the allowance every stage BEFORE the landing proof
// needs: the worktree status read, and the integration-tip fetch and rev-parse.
//
// It is measured through the REAL observeVerifyLanded, with a request carrying a
// reconstruction binding that this gate has no ledger to authorise.
// proveLandedContext refuses that on its FIRST line -- before
// reconstructionContentContext, and therefore before ProveEquivalentLandedContext
// and the allowance line inside it. So the smallest allowance that reaches the
// refusal is exactly what the prefix costs, and the number does not move when
// the proof's own budget handling is changed. An oracle that moved with the
// mutation it is meant to catch would certify nothing.
func observationPrefixCost(t *testing.T, repo, base string) int {
	t.Helper()
	for n := 1; n < 64; n++ {
		gate := &mergeadmit.Gate{
			RepoDir:     repo,
			ProofBudget: mergeadmit.ProofBudget{MaxCommands: n},
			Live:        mergeadmit.LiveState{OriginMainAt: originMainProbeContext(repo)},
		}
		req := mergeadmit.Request{
			Ref: pinProofRef, BaseSHA: base, CandidateSHA: base,
			Reconstruction: &mergeadmit.ReconstructionBinding{SHA: base, BaseSHA: base},
		}
		ctx, cancel := gate.ProofContext()
		_, err := observeVerifyLanded(ctx, repo, gate, req)
		cancel()
		if err != nil && strings.Contains(err.Error(), "reconstructed landing proof requires a ledger") {
			return n
		}
	}
	t.Fatal("the observation prefix never completed within a sane allowance")
	return 0
}

// The pin is proved inside the SAME finite allowance as the rest of the proof.
// An exhausted allowance stops the route with its cause intact, and seals
// nothing — it must never read as "the object is not there".
//
// THE ALLOWANCE MUST REACH THE PROOF. Hosted control run XI0WYE recorded this
// test PASSING against a mutant that gives the proof a brand new budget
// (m18, landed-proof-resets-the-injected-allowance). With MaxCommands: 1 the
// run exhausted during the tip probe and never entered the proof at all, so the
// mutated line was never executed and the refusal this asserts came from a stage
// the control says nothing about. Exhausting early produces the right sentinel
// for the wrong reason, which is a vacuous oracle.
//
// The allowance is therefore exactly the prefix: enough for the status read and
// the tip probe, and nothing for the proof that follows. Pristine, the proof's
// first command is refused. With the proof re-minting its own budget, it runs to
// completion and proves the landing on an allowance that was already spent --
// which is what the assertion below names.
func TestVerifyLandedGateStopsOnAnExhaustedAllowance(t *testing.T) {
	repo, base, candidate, _ := landedPinFixture(t)
	// Positive MaxCommands is mandatory: zero means DEFAULT, not exhausted.
	prefix := observationPrefixCost(t, repo, base)
	gate := &mergeadmit.Gate{RepoDir: repo, ProofBudget: mergeadmit.ProofBudget{MaxCommands: prefix}, Live: mergeadmit.LiveState{OriginMainAt: originMainProbeContext(repo)}}
	req := mergeadmit.Request{Ref: pinProofRef, BaseSHA: base, CandidateSHA: candidate}

	_, err := observeVerifyLandedUnderGate(t, repo, gate, req)
	if err == nil {
		t.Fatal("an exhausted allowance proved a landing")
	}
	if !errors.Is(err, mergeadmit.ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands through errors.Is", err)
	}
	receiptUnsealed(t, repo)
}

// A retired-carrier fallback is authorised by ONE identity. If the request
// carries another, the pin has authorised nothing and the route must refuse
// before the gate is built.
func TestRequirePinnedCandidateProvedBindsTheFallbackToThePin(t *testing.T) {
	pinned := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)

	retired := verifyLandedSurface{Dir: "/invoker", CarrierRetired: true, PinnedCandidate: pinned}
	if err := requirePinnedCandidateProved(retired, pinned); err != nil {
		t.Fatalf("the pinned candidate itself was refused: %v", err)
	}
	err := requirePinnedCandidateProved(retired, other)
	if err == nil {
		t.Fatal("a retired-carrier fallback authorised by one candidate proved a different one")
	}
	if !strings.Contains(err.Error(), "authorised as a proof surface by pinned candidate") {
		t.Fatalf("refusal reason = %v", err)
	}

	// A live carrier was never authorised by a pin, so it is unaffected.
	live := verifyLandedSurface{Dir: "/carrier"}
	if err := requirePinnedCandidateProved(live, other); err != nil {
		t.Fatalf("a live carrier was subjected to the pin binding: %v", err)
	}
}

// observeVerifyLandedUnderGate runs the observation on THE GATE'S OWN allowance.
//
// CI 34749904639: these tests passed context.Background(), which carries no
// budget, so ensureProofBudget installed the DEFAULTS and the gate's injected
// MaxCommands was never applied -- an exhausted-allowance test that could not
// exhaust. The context a caller owning the operation would use is the gate's.
func observeVerifyLandedUnderGate(t *testing.T, repo string, gate *mergeadmit.Gate, req mergeadmit.Request) (*mergeadmit.Proof, error) {
	t.Helper()
	ctx, cancel := gate.ProofContext()
	defer cancel()
	return observeVerifyLanded(ctx, repo, gate, req)
}
