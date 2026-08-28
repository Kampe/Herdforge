package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// approveByCompletionReceipt closes a card on completion evidence when no
// dispatch task-context authorization is available.
//
// FAC-629: `herd approve` reported failed=17 for cards whose work had
// genuinely landed and passed review — boundBoardProvider refused every one
// of them for lacking a dispatch task-context, and the error named the wrong
// artifact ("no usable launch receipt"). Dispatch task-context authorization
// bounds what an agent may do WHILE WORKING and legitimately expires (or, for
// cards dispatched outside herd's own dispatch path, was never issued);
// requiring it as proof that work FINISHED is a category error.
//
// This does not weaken the evidence gate: buildDoneRequest loads and
// hsync.BoardDone(Fenced) fully validates the SAME CompletionReceipt
// (digest, lifecycle state, verdict) that the dispatch-authorized path
// already relies on — see CompletionReceipt.Validate, called unconditionally
// by BoardDone. What is skipped is only what a dispatch task-context adds on
// top: a signed coordinator identity to bind an agent-notification callback
// to. Exactly the same trade-off FAC-563's override route already accepts
// ("no launch receipt, so the completion callback is skipped").
//
// The board write still goes through the SAME fenced path
// (fencedBoardDone: claim-stack lease + coordinator-minted op identity) the
// override route uses, so this is not a fencing bypass — only a bypass of
// the dispatch-context REQUIREMENT, gated on a real, validated completion
// receipt existing. No receipt is ever minted or backfilled here.
func approveByCompletionReceipt(
	ctx context.Context,
	cfg *config.Config,
	tp provider.TaskProvider,
	stack *provider.ClaimStack,
	root, ref, receiptPath, acceptanceEvidence string,
	authErr error,
) (*hsync.DoneResult, error) {
	req, closeAuthority, err := buildDoneRequest(root, cfg.TaskProvider.ProjectID, ref, receiptPath, acceptanceEvidence, nil)
	if err != nil {
		closeAuthority()
		return nil, err
	}
	defer closeAuthority()
	if req.Receipt == nil {
		return nil, fmt.Errorf("no dispatch task-context authorization for %s (%v), and no completion receipt at %s: "+
			"nothing evidences either authorization to work this card or that the work finished",
			ref, authErr, hsync.ReceiptPath(root, ref))
	}

	fmt.Fprintf(os.Stderr,
		"herd approve: closing %s from completion receipt evidence (candidate %s, merge %s, verdict %s) — "+
			"no dispatch task-context authorization was available: %v\n",
		ref, shortSHA(req.Receipt.CandidateSHA), shortSHA(req.Receipt.MergeSHA), req.Receipt.Verdict, authErr)
	fmt.Fprintf(os.Stderr,
		"herd approve: the completion callback binds a builder notification to a dispatch-issued coordinator identity, "+
			"which this path has none of, so it is skipped; the board write stays fenced\n")

	if stack == nil {
		return nil, fmt.Errorf(
			"completion-receipt close for %s requires a claim stack to write the board under a fence (FAC-147 fail-closed)", ref)
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
