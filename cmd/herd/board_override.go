package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// approveByOverride closes a card under a policy-limited, attributable operator
// override.
//
// FAC-563: the documented override flags could never work. approveOne calls
// boundBoardProvider first, which REQUIRES a launch receipt from the worktree or
// the canonical store, so an override failed with "no usable launch receipt"
// before authorization was ever reached. That made the override unusable for
// exactly the class it exists to serve: pre-receipt and legacy cards whose work
// provably landed outside the fleet integration path.
//
// This route is deliberately narrow:
//   - It is reachable ONLY with an override request, never by default.
//   - Authorization stays fail-closed. buildDoneRequest -> BoardDone require a
//     named policy from the closed set plus actor, reason, and evidence; a
//     missing or unknown one is refused here exactly as before.
//   - It uses the PLAIN provider, so it does not pretend to fenced authority it
//     cannot have. An override with no launch receipt has no task-bound
//     coordinator identity, so there is no callback to publish and none is
//     claimed.
//   - BoardDone still writes the attributable done-log record, so the closure
//     remains auditable and board-audit reports it as OVERRIDE rather than
//     NO_EVIDENCE.
//
// The bypass is announced on stderr. An override that closed a card quietly
// would be worse than the broken gate it replaces.
func approveByOverride(
	ctx context.Context,
	cfg *config.Config,
	tp provider.TaskProvider,
	root, ref string,
	override *hsync.OverrideRequest,
) (*hsync.DoneResult, error) {
	return approveByOverrideWithAcceptance(ctx, cfg, tp, root, ref, "", override)
}

// approveByOverrideWithAcceptance is approveByOverride carrying the operator's
// pasted acceptance output. Acceptance stays REQUIRED for an override: the
// override replaces proof that the work integrated, never proof that the work
// is what the card asked for.
func approveByOverrideWithAcceptance(
	ctx context.Context,
	cfg *config.Config,
	tp provider.TaskProvider,
	root, ref, acceptanceEvidence string,
	override *hsync.OverrideRequest,
) (*hsync.DoneResult, error) {
	if override == nil {
		return nil, fmt.Errorf("override approve: no override request")
	}
	req, closeAuthority, err := buildDoneRequest(root, cfg.TaskProvider.ProjectID, ref, "", acceptanceEvidence, override)
	if err != nil {
		closeAuthority()
		return nil, err
	}
	defer closeAuthority()
	if req.Override == nil {
		// buildDoneRequest resolved something other than an override; refuse
		// rather than close a card through a path its authority did not select.
		return nil, fmt.Errorf("override approve for %s: closing authority is not an override", ref)
	}

	fmt.Fprintf(os.Stderr,
		"herd board-done: closing %s under policy %q by %s — no launch receipt, so the fenced provider path and completion callback are bypassed\n",
		ref, override.Policy, override.Actor)
	fmt.Fprintf(os.Stderr, "herd board-done: reason: %s\n", override.Reason)
	fmt.Fprintf(os.Stderr, "herd board-done: evidence: %s\n", override.Evidence)

	res, err := hsync.BoardDone(ctx, tp, req)
	if err != nil {
		return nil, err
	}
	return res, nil
}
