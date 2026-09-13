package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// FAC-831: a merged candidate whose source carrier has been safely retired has
// NO worktree, by definition — and that is exactly the class --verify-landed
// exists to serve (the FAC-566 note in runHarvestVerifyLanded says so: "work
// already rebase-merged outside the fleet").
//
// runHarvestVerifyLanded nevertheless demanded one before reading any durable
// evidence, so PR832 could not reconcile although its candidate, its review and
// its merge all still existed. Nothing downstream actually needs the
// CANDIDATE'S carrier: the admission record and the landed disposition are read
// from the invoking checkout already, the gate takes no directory, and the
// dirty refusal and origin/main probe are properties of A CHECKOUT rather than
// of the retired carrier.
//
// What the fallback must NOT become is "prove it against whatever happens to be
// checked out". Identity has to be PINNED before the invoker is used as a proof
// surface — not left to the FAC-566 containment check to reject incidentally,
// which would silently accept an unrelated HEAD in any repository where that
// HEAD is not yet on origin/main.

// verifyLandedSurface is the directory a landing proof is observed in, plus why.
type verifyLandedSurface struct {
	// Dir is the proof surface.
	Dir string
	// CarrierRetired is true when no worktree carries the branch and the
	// invoking checkout is standing in for it.
	CarrierRetired bool
	// PinnedCandidate is the candidate identity that authorised the fallback.
	// Empty when a live carrier was found.
	PinnedCandidate string
}

// errRetiredCarrierUnpinned is returned when the carrier is gone and nothing
// pins the candidate. Kept as a value so the CLI test can match it exactly.
var errRetiredCarrierUnpinned = fmt.Errorf(
	"carrier retired and candidate unpinned: no worktree carries this branch, so the branch head cannot " +
		"identify the reviewed object. Re-run with an explicit --candidate <sha>, or ensure --ref names a task " +
		"whose review ledger holds a current admitted PASS. Refusing to observe a landing against the invoking " +
		"checkout's HEAD, which is unrelated to the reviewed candidate")

// resolveVerifyLandedSurface picks the directory the landing proof is observed
// in, and refuses rather than guessing.
//
// A live carrier is used exactly as before — this changes nothing for it, and
// its dirty/active protection is unchanged. Only when NO worktree carries the
// branch does the invoking checkout stand in, and then only after:
//
//  1. the candidate is PINNED, by an explicit --candidate or by the ref's
//     current admitted PASS. The branch head is never consulted, because for a
//     retired carrier it either does not exist or has moved on;
//  2. the pinned candidate object is PRESENT in the invoking repository, which
//     is what makes it the right repository rather than a foreign one that
//     happens to be the working directory.
//
// The dirty refusal still runs downstream, against the surface actually used.
func resolveVerifyLandedSurface(branch string, binding verifyLandedBinding, lookup func(string) string, invokerRoot func() (string, error)) (verifyLandedSurface, error) {
	if dir := strings.TrimSpace(lookup(branch)); dir != "" {
		return verifyLandedSurface{Dir: dir}, nil
	}

	pinned := pinnedCandidateFor(binding)
	if pinned == "" {
		return verifyLandedSurface{}, errRetiredCarrierUnpinned
	}

	root, err := invokerRoot()
	if err != nil {
		return verifyLandedSurface{}, fmt.Errorf("resolve invoking repository root: %w", err)
	}
	if err := requireObjectPresent(root, pinned); err != nil {
		return verifyLandedSurface{}, fmt.Errorf(
			"carrier retired and the pinned candidate %s is not present in the invoking repository: %w. "+
				"Run this from the repository the candidate was reviewed in; a landing cannot be proved "+
				"against a repository that does not hold the reviewed object",
			shortSHA12(pinned), err)
	}
	return verifyLandedSurface{Dir: root, CarrierRetired: true, PinnedCandidate: pinned}, nil
}

// pinnedCandidateFor returns the candidate identity that may authorise a
// retired-carrier fallback: an explicit --candidate, else the ref's current
// admitted PASS. It never falls back to a branch head.
func pinnedCandidateFor(binding verifyLandedBinding) string {
	if explicit := strings.TrimSpace(binding.Candidate); explicit != "" {
		return explicit
	}
	ref := strings.TrimSpace(binding.Ref)
	if ref == "" {
		return ""
	}
	ev, err := newLedgerLegacyReview(drainLedgerPath()).AdmittedPass(ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ev.CandidateSHA)
}

// requireObjectPresent proves the repository actually holds the object, so a
// foreign checkout cannot stand in for the reviewed one.
func requireObjectPresent(repoDir, sha string) error {
	// An empty identity is refused BEFORE any command. `git cat-file -t ""`
	// spends a subprocess to answer "Not a valid object name" and reports it
	// with a blank SHA, which reads as a repository problem rather than the
	// missing pin it actually is. Seen in CI 34742740503 m03, where removing
	// the pin guard let an empty candidate reach here.
	if strings.TrimSpace(sha) == "" {
		return fmt.Errorf("no candidate identity to look up")
	}
	out, err := exec.Command("git", "-C", repoDir, "cat-file", "-t", sha).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git cat-file -t %s: %s", shortSHA12(sha), strings.TrimSpace(string(out)))
	}
	if got := strings.TrimSpace(string(out)); got != "commit" {
		return fmt.Errorf("object %s is a %s, not a commit", shortSHA12(sha), got)
	}
	return nil
}

// invokingRepoRoot is the production proof surface when the carrier is gone.
func invokingRepoRoot() (string, error) { return filepath.Abs(".") }
