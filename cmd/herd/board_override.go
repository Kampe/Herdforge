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
	stack *provider.ClaimStack,
	root, ref string,
	override *hsync.OverrideRequest,
) (*hsync.DoneResult, error) {
	return approveByOverrideWithAcceptance(ctx, cfg, tp, stack, root, ref, "", override)
}

// approveByOverrideWithAcceptance is approveByOverride carrying the operator's
// pasted acceptance output. Acceptance stays REQUIRED for an override: the
// override replaces proof that the work integrated, never proof that the work
// is what the card asked for.
func approveByOverrideWithAcceptance(
	ctx context.Context,
	cfg *config.Config,
	tp provider.TaskProvider,
	stack *provider.ClaimStack,
	root, ref, acceptanceEvidence string,
	override *hsync.OverrideRequest,
) (*hsync.DoneResult, error) {
	if override == nil {
		return nil, fmt.Errorf("override approve: no override request")
	}
	req, closeAuthority, err := buildDoneRequest(root, cfg.TaskProvider.ProjectID, ref, "", acceptanceEvidence, override)
	// Route B: admitted cross-family review evidence for cards groomed before
	// herd-acceptance-v1 existed. Supplying the authority does not authorize
	// anything by itself -- BoardDone still tries the acceptance block first and
	// only falls through for a legacy-policy override on a card with no block.
	req.LegacyReview = newLedgerLegacyReview(drainLedgerPath())
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
		"herd board-done: closing %s under policy %q by %s — no launch receipt, so the completion callback is skipped; the board write stays fenced\n",
		ref, override.Policy, override.Actor)
	fmt.Fprintf(os.Stderr, "herd board-done: reason: %s\n", override.Reason)
	fmt.Fprintf(os.Stderr, "herd board-done: evidence: %s\n", override.Evidence)

	// FAC-566: the board write MUST stay fenced. FAC-563 routed overrides
	// through the plain provider, and Kaneo correctly refused with "mutation
	// refused without X-Herd-Op (unfenced bypass; FAC-147 fail-closed)". That
	// refusal was right and the design was wrong: I had conflated the completion
	// RECEIPT with the mutation FENCE. An override replaces proof that the work
	// landed; it does not replace the coordinator's authority to write the board.
	//
	// fencedBoardDone acquires its own lease from the claim stack and mints
	// coordinator op identity, neither of which needs a launch receipt, so the
	// override closes through the same fenced path as a normal completion.
	if stack == nil {
		return nil, fmt.Errorf(
			"override close for %s requires a claim stack to write the board under a fence (FAC-147 fail-closed)", ref)
	}
	task, err := resolveTaskByRef(ctx, tp, cfg.TaskProvider.ProjectID, ref)
	if err != nil {
		return nil, err
	}
	if req.Ref == "" {
		req.Ref = task.Ref
	}
	if req.ProjectID == "" {
		req.ProjectID = cfg.TaskProvider.ProjectID
	}
	if req.RepoDir == "" {
		req.RepoDir = root
	}
	return fencedBoardDone(ctx, cfg, tp, stack, task, req)
}
