package main

import (
	"fmt"
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
// This is SELECTION ONLY. It runs no git and proves nothing about the
// repository it returns. A live carrier is used exactly as before — this
// changes nothing for it, and its dirty/active protection is unchanged. Only
// when NO worktree carries the branch does the invoking checkout stand in, and
// then only when the candidate is PINNED, by an explicit --candidate or by the
// ref's current admitted PASS. The branch head is never consulted, because for
// a retired carrier it either does not exist or has moved on.
//
// Selecting a surface authorises NOTHING on its own. Whether the invoking
// repository actually holds the pinned object, and whether that object is a
// commit at all, is proved by the bounded gate proof downstream:
// ProveEquivalentLandedContext resolves every identity with
// `rev-parse --verify -q <sha>^{commit}` inside the ONE allowance the public
// entry installs, using the package's owned process group. That proof runs
// before the landed disposition is written and before any receipt is sealed.
//
// PR843/3d645a26: this function used to run its own `git cat-file -t` through
// exec.Command — outside that process group, deadline, command budget and
// output budget, and against the PINNED sha, which is not necessarily the sha
// the proof then used. The check has not been dropped; it has moved to the one
// place that checks the object actually being proved, under the allowance.
// requirePinnedCandidateProved below closes the gap between the two.
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
	return verifyLandedSurface{Dir: root, CarrierRetired: true, PinnedCandidate: pinned}, nil
}

// requirePinnedCandidateProved refuses when a retired-carrier fallback was
// authorised by one identity and the proof would then be run against another.
//
// The fallback's whole justification is that the PINNED candidate ties the
// invoking checkout to the reviewed work. If the request carries a different
// candidate — an admission record for the ref can hold one — then the identity
// that authorised standing in is not the identity being proved, and the pin has
// authorised nothing. Refuse instead of proving the wrong object in a surface
// that was never justified for it.
//
// This runs no git: it compares the two identities the caller already holds.
func requirePinnedCandidateProved(surface verifyLandedSurface, candidate string) error {
	if !surface.CarrierRetired {
		return nil
	}
	got, want := strings.TrimSpace(candidate), strings.TrimSpace(surface.PinnedCandidate)
	if got != want {
		return fmt.Errorf(
			"carrier retired: the invoking checkout was authorised as a proof surface by pinned candidate %s, "+
				"but the request would prove %s. Re-run with --candidate %s, or resolve the admission record "+
				"that names the other object; a pin authorises only the object it names",
			shortSHA12(want), shortSHA12(got), shortSHA12(want))
	}
	return nil
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

// invokingRepoRoot is the production proof surface when the carrier is gone.
func invokingRepoRoot() (string, error) { return filepath.Abs(".") }
