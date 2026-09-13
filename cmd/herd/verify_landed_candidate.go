package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
)

// resolveVerifyLandedCandidate determines the exact reviewed candidate object.
//
// FAC-566: resolution order matters, because the previous fallback silently
// bound the wrong object. A branch head moves; candidate identity does not.
//
//  1. An explicit --candidate always wins. The operator named it. The flag is
//     --candidate, not --candidate-sha: an earlier version of this message and
//     of my own operator guidance named a flag that does not exist, which sent
//     a caller chasing a nonexistent option instead of the real one.
//  2. Otherwise, with --ref, the candidate comes from the ref's current admitted
//     PASS. That is the identity the review is ABOUT, so it is the only
//     defensible answer for already-merged work.
//  3. Otherwise the worktree HEAD, but ONLY if HEAD is not already contained in
//     origin/main. A HEAD that is already contained is the landed commit, not a
//     reviewed candidate, and using it produced a disposition that bound the
//     merge SHA as the candidate.
//
// It runs INSIDE the caller's allowance. Review b70054a6: every command below
// used bare exec.Command before the shared budget existed, so the shipped route
// spent an unbounded fetch, rev-parse and ancestry probe -- with no deadline,
// no command or output cap and no owned process group -- before anything it
// later measured. Candidate identity is part of the proof, so it is charged
// like the rest of it.
func resolveVerifyLandedCandidate(ctx context.Context, wtDir, branch string, binding verifyLandedBinding) (string, error) {
	if explicit := strings.TrimSpace(binding.Candidate); explicit != "" {
		return explicit, nil
	}

	ref := strings.TrimSpace(binding.Ref)
	if ref != "" {
		if ev, err := newLedgerLegacyReview(drainLedgerPath()).AdmittedPass(ref); err == nil {
			if sha := strings.TrimSpace(ev.CandidateSHA); sha != "" {
				return sha, nil
			}
		}
	}

	git := mergeadmit.BoundedGit(ctx, wtDir)
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve candidate tip on %s: %w", branch, err)
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return "", fmt.Errorf("candidate sha is required for --verify-landed receipt reconcile")
	}
	contained, err := headContainedInOriginMain(ctx, wtDir, head)
	if err != nil {
		return "", err
	}
	if contained {
		return "", fmt.Errorf(
			"refusing to use branch HEAD %s as the candidate: it is already contained in origin/main, "+
				"so it is the landed commit rather than the reviewed candidate. "+
				"Supply the reviewed object explicitly with --candidate, or ensure the ref has a "+
				"current admitted PASS so its candidate can be resolved from the review ledger",
			shortSHA12(head))
	}
	return head, nil
}

// headContainedInOriginMain reports whether an object is already an ancestor of
// origin/main, INSIDE the caller's allowance.
//
// A FAILED FETCH IS NOW A REFUSAL. The previous version discarded it, so an
// offline or remote-less repository answered this guard from a stale tracking
// ref -- and answering "not contained" from stale data is exactly how a landed
// commit gets bound as the reviewed candidate, which is the bug the guard
// exists to prevent. An earlier revision returned false on fetch failure and
// reintroduced it; refusing is the only answer that cannot be wrong, and the
// caller can still name the object explicitly with --candidate.
//
// A missing origin/main is different and stays non-fatal: there is genuinely
// nothing to compare against, so nothing is contained.
func headContainedInOriginMain(ctx context.Context, wtDir, sha string) (bool, error) {
	git := mergeadmit.BoundedGit(ctx, wtDir)
	if _, err := git("fetch", "-q", "origin", "main"); err != nil {
		// An exhausted allowance or a cancelled run is returned AS ITSELF. It is
		// not evidence about origin, and describing it as "origin could not be
		// fetched" would bury a budget sentinel under a network story -- the
		// same flattening CI 34741746509 caught inside the package.
		if mergeadmit.ProofRunAborted(ctx, err) {
			return false, err
		}
		return false, fmt.Errorf(
			"refusing to resolve the candidate from branch HEAD: the current origin could not be fetched (%w). "+
				"A stale tracking ref cannot answer whether HEAD is already landed. "+
				"Re-run with --candidate <sha>, or restore access to origin",
			err)
	}
	if _, err := git("rev-parse", "--verify", "-q", "origin/main"); err != nil {
		// ONLY GIT'S OWN "this ref does not resolve" IS AN ANSWER. `rev-parse
		// --verify -q` exits 1 for exactly that, and there is then genuinely
		// nothing to compare against. Every other outcome -- an exhausted
		// allowance, a cancelled run, a broken repository, a signal -- is "I
		// could not look", and reading any of them as "nothing to compare
		// against" admits a landed HEAD as the reviewed candidate, which is the
		// bug this guard exists to prevent.
		var exitErr *exec.ExitError
		if !mergeadmit.ProofRunAborted(ctx, err) && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("resolve origin/main for the candidate guard: %w", err)
	}
	// Ancestry goes through the package's single charged definition, which
	// separates a genuine non-ancestor (exit 1) from a killed or refused
	// command. Reading the raw exit status here would collapse the two.
	return mergeadmit.AncestorProven(ctx, wtDir, sha, "origin/main")
}
