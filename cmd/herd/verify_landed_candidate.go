package main

import (
	"fmt"
	"os/exec"
	"strings"
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
func resolveVerifyLandedCandidate(wtDir, branch string, binding verifyLandedBinding) (string, error) {
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

	out, err := exec.Command("git", "-C", wtDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolve candidate tip on %s: %w", branch, err)
	}
	head := strings.TrimSpace(string(out))
	if head == "" {
		return "", fmt.Errorf("candidate sha is required for --verify-landed receipt reconcile")
	}
	if headContainedInOriginMain(wtDir, head) {
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
// origin/main.
//
// The fetch is opportunistic and its failure must NOT gate the answer. An
// earlier version returned false when fetch failed, which meant an offline or
// remote-less repository silently reported "not contained" -- reintroducing the
// exact bug this guard exists to prevent. If origin/main is not resolvable at
// all there is nothing to compare against, and only then is the answer false.
func headContainedInOriginMain(wtDir, sha string) bool {
	_ = exec.Command("git", "-C", wtDir, "fetch", "-q", "origin", "main").Run()
	if err := exec.Command("git", "-C", wtDir, "rev-parse", "--verify", "-q", "origin/main").Run(); err != nil {
		return false
	}
	return exec.Command("git", "-C", wtDir, "merge-base", "--is-ancestor", sha, "origin/main").Run() == nil
}
