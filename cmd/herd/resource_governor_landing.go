package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// governorLandingPredicate teaches the resource governor to recognize work
// that landed without keeping its object name. The census answered "is this
// lane merged" with ancestry alone, so a rebase or squash -- which replays the
// reviewed range under a new SHA -- left the lane reporting unmerged forever
// and its regenerable artifacts permanently unreclaimable.
//
// It is an adapter, not an algorithm. pkg/resources cannot import the proof
// (both harvest and mergeadmit already depend on resources), so the predicate
// is injected here, where the retirement path's own whole-range proof already
// lives. A fourth copy of that proof is exactly what must not be written.
//
// Refusals are the point of the proof, not a limitation of it: a reverted, a
// partially-landed, an ambiguous, or an unprovable range is NOT landed, and
// the lane keeps its artifacts.
func governorLandingPredicate(root string) resources.LandingPredicate {
	return func(ctx context.Context, probe resources.LandingProbe) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		base, head := strings.TrimSpace(probe.BaseSHA), strings.TrimSpace(probe.HeadSHA)
		if base == "" || head == "" {
			return false, fmt.Errorf("landing proof requires a pinned base and head")
		}
		// The pinned identities are used for the proof itself, so a branch that
		// moves after the census observed it cannot be proved landed on the
		// strength of a tip nobody inspected.
		ahead := commitsAhead(root, base, head)
		switch {
		case ahead < 0:
			// Unanswerable, which is not the same as unmerged.
			return false, fmt.Errorf("landing comparison of %s against %s is unreadable", shortSha(head), shortSha(base))
		case ahead == 0:
			return true, nil
		}
		if _, err := rangeLandedProof(root, base, head); err == nil {
			return true, nil
		}
		// Separate "the proof refused" from "git could not answer": only the
		// latter is unknown. Both keep the lane, but the recorded reason must
		// not claim knowledge the probe did not have.
		if _, resolveErr := resolveReapRefs(root, base, head); resolveErr != nil {
			return false, fmt.Errorf("landing refs for %s are unreadable: %w", shortSha(head), resolveErr)
		}
		return false, nil
	}
}
