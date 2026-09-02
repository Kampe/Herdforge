package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// shotGenerationFacts is a complete pre-mutation snapshot for advancing
// canonical lifecycle generation from a newer signed worker callback.
type shotGenerationFacts struct {
	shotSupersessionFacts
	PriorLeaseGeneration int64
	PriorCandidateSHA    string
	PriorSequence        int64
	PriorState           lifecycle.State
}

var runShotGenerationReconcile = reconcileShotLifecycleGeneration

func validateShotGenerationFacts(f shotGenerationFacts) error {
	if f.PriorLeaseGeneration <= 0 || f.ReportedLease <= f.PriorLeaseGeneration {
		return fmt.Errorf("worker generation reconcile: callback lease generation is not newer than canonical lifecycle")
	}
	if f.PriorSequence <= 0 || (f.PriorState != lifecycle.StateEligible && f.PriorState != lifecycle.StateRecovering) {
		return fmt.Errorf("worker generation reconcile: canonical lifecycle is not eligible or recovering")
	}
	if !validShotSHA(f.PriorCandidateSHA) {
		return fmt.Errorf("worker generation reconcile: prior candidate SHA is required")
	}
	if err := validateShotSupersessionFacts(f.shotSupersessionFacts); err != nil {
		return fmt.Errorf("worker generation reconcile: %w", err)
	}
	return nil
}

func reconcileShotLifecycleGeneration(ctx context.Context, root, ref string, lease int64, sha string, machine *lifecycle.Machine, current *lifecycle.TaskState) error {
	if current == nil || (current.State != lifecycle.StateEligible && current.State != lifecycle.StateRecovering) {
		return fmt.Errorf("worker generation reconcile: canonical lifecycle is not eligible or recovering")
	}
	if current.LeaseGeneration >= lease {
		return shotLeaseGenerationConflict(current.LeaseGeneration, lease)
	}
	facts, authority, err := collectShotSupersessionFacts(ctx, root, ref, lease, sha)
	if err != nil {
		return err
	}
	genFacts := shotGenerationFacts{
		shotSupersessionFacts: facts,
		PriorLeaseGeneration:  current.LeaseGeneration,
		PriorCandidateSHA:     current.CandidateSHA,
		PriorSequence:         current.Seq,
		PriorState:            current.State,
	}
	if err := validateShotGenerationFacts(genFacts); err != nil {
		return err
	}
	req := lifecycle.WorkerGenerationReconcileRequest{
		TaskRef: ref, TaskID: authority.TaskID, ProjectID: authority.ProjectID, Repo: current.Repo,
		ExpectedSequence: current.Seq, OldLeaseGeneration: current.LeaseGeneration, NewLeaseGeneration: lease,
		Branch: authority.Branch, BaseSHA: authority.BaseSHA, OldCandidateSHA: current.CandidateSHA, NewCandidateSHA: sha,
		Worktree: facts.Worktree, Actor: authority.SessionID, SessionID: authority.SessionID,
		BuilderSession: facts.BuilderSession, BuilderModel: facts.Model, BuilderFamily: facts.Family,
		IdempotencyKey: fmt.Sprintf("shot:%s:lease:%d:generation-reconcile:%s", strings.ToLower(ref), lease, sha),
	}
	_, digest, err := lifecycle.EncodeWorkerGenerationReconcileEvidence(lifecycle.WorkerGenerationReconcileEvidence{
		TaskID: req.TaskID, ProjectID: req.ProjectID, BaseSHA: req.BaseSHA, Worktree: req.Worktree,
		OldLeaseGeneration: req.OldLeaseGeneration, NewLeaseGeneration: req.NewLeaseGeneration,
		OldCandidateSHA: req.OldCandidateSHA, NewCandidateSHA: req.NewCandidateSHA,
		PriorSequence: req.ExpectedSequence, SessionID: req.SessionID,
		BuilderSession: req.BuilderSession, BuilderModel: req.BuilderModel, BuilderFamily: req.BuilderFamily,
	})
	if err != nil {
		return err
	}
	req.EvidenceDigest = digest
	result, err := machine.ReconcileWorkerGeneration(req)
	if err != nil {
		return fmt.Errorf("shot: reconcile worker generation: %w", err)
	}
	if result.Event.CandidateSHA != sha || result.Event.LeaseGeneration != lease {
		return fmt.Errorf("shot: worker generation reconcile returned non-exact readback")
	}
	if !result.Replayed && result.Event.Seq != current.Seq+1 {
		return fmt.Errorf("shot: worker generation reconcile returned non-exact readback")
	}
	return nil
}
