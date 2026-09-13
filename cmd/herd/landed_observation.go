package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
)

// Observe immutable candidate content using the same proof as reconciliation,
// INSIDE the allowance the caller owns.
//
// FAC-831 review 6c93cc2b: this ran two unbounded git commands and then minted
// a fresh budget in ProveLanded, so the shipped `harvest-merge --verify-landed`
// route spent several independent allowances and proved the same landing twice.
// Every command here now goes through the gate's bounded runner on the caller's
// context, and the proof continues on that same context.
//
// The worktree status check is checkout hygiene rather than proof, and the
// review allows excluding it. It is bounded anyway: it is one charge on the same
// code path, and leaving one unbounded exec beside a bounded one is how this
// file gets found again.
func observeVerifyLanded(ctx context.Context, dir string, gate *mergeadmit.Gate, req mergeadmit.Request) (*mergeadmit.Proof, error) {
	git := mergeadmit.BoundedGit(ctx, dir)
	status, err := git("status", "--porcelain", gitroot.StatusUntrackedNormal)
	if err != nil {
		return nil, fmt.Errorf("landing proof: read worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return nil, fmt.Errorf("landing proof refuses dirty worktree")
	}
	// The tip comes from the GATE'S injected probe on this context, not from a
	// second origin path built here. A composition that constructs its own probe
	// is another escape hatch: it cannot be substituted, and it would not be the
	// reading the gate itself would take.
	landed, err := gate.CurrentOriginMain(ctx, "origin_main_verify_landed")
	if err != nil {
		return nil, err
	}
	return gate.ProveLandedContext(ctx, req, landed)
}
