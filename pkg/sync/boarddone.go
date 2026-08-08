package sync

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// Port of bin/herd-board-done: move a ticket to done ONLY when its work is
// provably on origin/main.
//
// WHY (incident, 2026-07-28, chainseer): marking done is a claim about
// reality, and nothing checked it. Cards were moved to done while the merge
// had been REFUSED by a gate, because the board write was chained behind a
// pipe whose tail exited 0.
//
// The first fix proved by CONTENT rather than commit message alone, accepting
// an explicit --evidence ancestor commit as first-class proof. FAC-132 removed
// that too: an arbitrary ancestor says nothing about a specific task, and a
// commit subject says nothing about what it contains. Closing authority now
// lives entirely in CompletionReceipt (donereceipt.go).

// ErrNoEvidence marks an honest refusal: nothing proves this task's accepted
// candidate is on origin/main.
var ErrNoEvidence = errors.New("no completion receipt proves this task landed")

var zeroPadRef = regexp.MustCompile(`^([A-Za-z]+-)0+([0-9])`)

// NormalizeRef strips zero padding from a ticket ref: FAC-018 and FAC-18 are
// the same ticket; a padded ref misses the board.
func NormalizeRef(ref string) string {
	return zeroPadRef.ReplaceAllString(ref, "${1}${2}")
}

func git(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// MergeEvidence returns a human-readable HINT that ref's work may be on
// origin/main, or "" when nothing matches. Order:
//  1. an explicit evidenceSHA that is an ancestor of origin/main (hard error
//     if given but NOT an ancestor — a wrong claim must not fall through), or
//  2. a commit on origin/main naming the ref, with an explicit non-digit
//     boundary so FAC-18 does not match FAC-180 (git's POSIX ERE has no \b).
//
// FAC-132: this is a DISCOVERY HINT, not closing authority. Neither form
// proves a specific task's accepted candidate landed — an empty commit whose
// subject names the ticket satisfies (2), and any unrelated ancestor satisfies
// (1). BoardDone no longer consults it; only CompletionReceipt closes a card.
// It is still used for post-merge ancestry readback in the harvest pipeline
// and to surface suspicious historical closures in AuditDone.
func MergeEvidence(repoDir, ref, evidenceSHA string) (string, error) {
	// Refresh origin/main; offline is fine, we check against the local ref.
	_, _ = git(repoDir, "fetch", "-q", "origin", "main")
	if _, err := git(repoDir, "rev-parse", "--verify", "-q", "origin/main"); err != nil {
		return "", fmt.Errorf("no origin/main in %s", repoDir)
	}

	if evidenceSHA != "" {
		if _, err := git(repoDir, "merge-base", "--is-ancestor", evidenceSHA, "origin/main"); err != nil {
			return "", fmt.Errorf("REFUSING: evidence %s is not an ancestor of origin/main", evidenceSHA)
		}
		short, _ := git(repoDir, "rev-parse", "--short", evidenceSHA)
		return fmt.Sprintf("explicit evidence commit %s is an ancestor of origin/main", short), nil
	}

	return commitHint(repoDir, ref), nil
}

// commitHint is the commit-subject match on its own, WITHOUT the fetch, so a
// caller sweeping many refs pays for one refresh instead of one per ref. The
// non-digit boundary is explicit because git's POSIX ERE has no \b.
func commitHint(repoDir, ref string) string {
	hit, err := git(repoDir, "log", "origin/main", "--format=%h %s", "-E",
		"--grep="+ref+`([^0-9]|$)`, "-1")
	if err != nil || hit == "" {
		return ""
	}
	return fmt.Sprintf("origin/main carries a commit naming %s: %s", ref, hit)
}

// DoneResult reports what BoardDone did and why it was allowed to.
type DoneResult struct {
	Ref           string
	TaskID        string
	Proof         string
	Overridden    bool
	CommentPosted bool
	// Idempotent is true when this exact receipt had already been consumed
	// and no provider mutation was attempted.
	Idempotent    bool
	ReceiptDigest string
}

// DoneRequest is everything BoardDone needs to decide whether a card may
// close. Exactly one of Receipt or Override supplies the authority; supplying
// neither refuses, and supplying both refuses (an override is for the case
// where there IS no receipt).
type DoneRequest struct {
	RepoDir   string
	ProjectID string
	Ref       string
	// Receipt is the automatic authority: a sealed, task-bound completion
	// receipt. Requires Lifecycle.
	Receipt *CompletionReceipt
	// Lifecycle is the durable state authority consulted for receipt-driven
	// transitions. Nil refuses.
	Lifecycle LifecycleAuthority
	// Override is the manual authority: explicit, policy-limited,
	// attributable, and appended to the append-only done log.
	Override *OverrideRequest
}

// BoardDone moves the ticket with the given ref to done, gated on a task-bound
// completion receipt (or an explicit policy-limited manual override), and
// verifies the write by READ-BACK: board APIs are known to report success on
// writes that did not persist.
//
// FAC-132: a commit subject naming the ticket, and an unrelated origin/main
// ancestor, are both refused here. Neither is task evidence.
func BoardDone(ctx context.Context, tp provider.TaskProvider, req DoneRequest) (*DoneResult, error) {
	ref := NormalizeRef(req.Ref)
	repoDir := req.RepoDir
	if repoDir == "" {
		repoDir = "."
	}

	var proof string
	var override *OverrideRecord
	switch {
	case req.Receipt != nil && req.Override != nil:
		return nil, fmt.Errorf("%w for %s: a manual override cannot accompany a receipt; "+
			"drop one of them so the closing authority is unambiguous", ErrNoEvidence, ref)

	case req.Receipt != nil:
		if req.Lifecycle == nil {
			return nil, fmt.Errorf("%w for %s: no lifecycle state authority configured; "+
				"a receipt alone cannot prove the task is past integration", ErrNoEvidence, ref)
		}
		st, err := req.Lifecycle.CurrentState(ref)
		if err != nil {
			return nil, fmt.Errorf("lifecycle state for %s: %w", ref, err)
		}
		if err := req.Receipt.Validate(repoDir, ref, st); err != nil {
			return nil, fmt.Errorf("%w for %s: %v", ErrNoEvidence, ref, err)
		}
		proof = fmt.Sprintf("completion receipt %s: candidate %s merged as %s, patch %s, verification %s, tier %s, %s reviewed by %s",
			shortDigest(req.Receipt.Digest), shortSHA(req.Receipt.CandidateSHA), shortSHA(req.Receipt.MergeSHA),
			shortDigest(req.Receipt.PatchID), shortDigest(req.Receipt.VerificationDigest),
			req.Receipt.RiskTier, req.Receipt.AuthorFamily, req.Receipt.ReviewerFamily)

	case req.Override != nil:
		rec, err := authorizeOverride(*req.Override)
		if err != nil {
			return nil, fmt.Errorf("%w for %s: %v", ErrNoEvidence, ref, err)
		}
		override = rec
		proof = fmt.Sprintf("manual override by %s under policy %s (%s): %s [evidence: %s]",
			rec.Actor, rec.Policy, rec.Decision, rec.Reason, rec.Evidence)

	default:
		return nil, fmt.Errorf("%w for %s: no completion receipt at %s. A commit naming the ref is a "+
			"discovery hint, not proof. Supply the receipt the integration produced, or close it manually with "+
			"--override-policy/--override-actor/--override-reason/--override-evidence",
			ErrNoEvidence, ref, ReceiptPath(repoDir, ref))
	}

	// Exactly-once: a receipt already recorded in the append-only done log
	// never advances the card a second time. Read errors refuse rather than
	// look like "not yet consumed".
	log, err := ReadDoneLog(repoDir)
	if err != nil {
		return nil, err
	}
	if req.Receipt != nil {
		for _, rec := range log {
			if rec.ReceiptDigest != "" && rec.ReceiptDigest == req.Receipt.Digest {
				return &DoneResult{
					Ref: ref, TaskID: rec.TaskID, Proof: proof,
					Idempotent: true, ReceiptDigest: req.Receipt.Digest,
				}, nil
			}
		}
	}

	// Resolve the ref against the task provider. ListTasks + ref match is the
	// primary path, but a card created directly (coordinator-authored tickets,
	// CI repairs, split tickets) may carry an empty Ref field — the kaneo CLI's
	// `task create` has no --ref flag. GetTask(ref) is a second resolution path
	// through the same provider: the MemoryProvider resolves by ref, and the
	// KaneoProvider's decodeKaneoTaskBody matches dto.Ref == wantID. A card the
	// provider knows about must be closeable with evidence (FAC-211).
	task, err := resolveTaskByRef(ctx, tp, req.ProjectID, ref)
	if err != nil {
		return nil, err
	}
	// Task binding: the receipt names the provider task it was minted for. A
	// ref match alone is not enough — refs are re-minted across board
	// rollbacks, and a re-minted card is a different task.
	if req.Receipt != nil && req.Receipt.TaskID != task.ID {
		return nil, fmt.Errorf("%w for %s: receipt is bound to task id %s but the board card is %s",
			ErrNoEvidence, ref, req.Receipt.TaskID, task.ID)
	}

	if err := tp.UpdateStatus(ctx, task.ID, "done"); err != nil {
		return nil, boardCallErr(fmt.Sprintf("status write for %s", ref), err)
	}

	back, err := tp.GetTask(ctx, task.ID)
	if err != nil {
		return nil, boardCallErr(fmt.Sprintf("read-back for %s after status write", ref), err)
	}
	if back.Status != "done" {
		return nil, fmt.Errorf("write reported success but %s reads back as %q", ref, back.Status)
	}

	// The durable record is appended AFTER the readback, so a crash between
	// the two leaves a done card with no record — which replays safely (the
	// status write is idempotent) — rather than a record for a write that
	// never landed.
	rec := DoneRecord{
		Timestamp: nowStamp(), Ref: ref, TaskID: task.ID,
		ProviderReadback: back.Status, Override: override,
	}
	if req.Receipt != nil {
		rec.ReceiptDigest = req.Receipt.Digest
		rec.MergeSHA = req.Receipt.MergeSHA
	}
	if err := appendDoneRecord(repoDir, rec); err != nil {
		return nil, fmt.Errorf("%s reads back as done but its closure could not be recorded (re-run to record): %w", ref, err)
	}

	res := &DoneResult{Ref: ref, TaskID: task.ID, Proof: proof, Overridden: override != nil, ReceiptDigest: rec.ReceiptDigest}
	// Comment is best-effort only when the call is non-timeout; timeout/ambiguous
	// must not look like success with CommentPosted (FAC-150).
	if err := tp.AddComment(ctx, task.ID, "board-done: "+proof); err != nil {
		if provider.IsTimeout(err) || provider.IsAmbiguous(err) {
			return nil, boardCallErr("board-done comment", err)
		}
		// Non-timeout comment failure: status is done; leave CommentPosted false.
	} else {
		res.CommentPosted = true
	}
	return res, nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func shortDigest(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

// resolveTaskByRef finds the board card for ref through the task provider.
// It tries ListTasks + Ref field match first, then falls back to GetTask(ref)
// so a card the provider knows about but whose Ref field is empty (a
// directly-created card) is still closeable (FAC-211).
func resolveTaskByRef(ctx context.Context, tp provider.TaskProvider, projectID, ref string) (*provider.Task, error) {
	tasks, err := tp.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, boardCallErr("list tasks", err)
	}
	for _, t := range tasks {
		if strings.EqualFold(NormalizeRef(t.Ref), ref) {
			return t, nil
		}
	}
	// Fallback: a directly-created card may have an empty Ref field. GetTask
	// resolves through the provider's own lookup, which may match by ref or id.
	if t, err := tp.GetTask(ctx, ref); err == nil && t != nil {
		return t, nil
	}
	return nil, fmt.Errorf("no task with ref %s on the board", ref)
}

// boardCallErr projects provider timeout/ambiguous as BLOCKED(provider_timeout).
func boardCallErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if provider.IsTimeout(err) || provider.IsAmbiguous(err) || provider.ClassifyOpError(err) == provider.OpTimeout || provider.ClassifyOpError(err) == provider.OpAmbiguous {
		return fmt.Errorf("%s: BLOCKED(provider_timeout): %w", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// requireLiveLease rejects BoardDoneFenced when ownerID+generation is not the
// current active claim for key (FAC-147: live lease is mandatory even for the
// already-done idempotent short-circuit).
func requireLiveLease(ctx context.Context, mgr *claim.ClaimManager, key claim.LeaseKey, ownerID string, generation int64) error {
	if mgr == nil {
		return fmt.Errorf("%w: nil ClaimManager", claim.ErrLeaseNotCurrent)
	}
	claims, err := mgr.ActiveClaims(ctx)
	if err != nil {
		return fmt.Errorf("verify live lease: %w", err)
	}
	for _, l := range claims {
		if l == nil || l.LeaseKey != key {
			continue
		}
		if l.OwnerID != ownerID {
			return fmt.Errorf("%w: %s is held by %s, not %s", claim.ErrLeaseNotCurrent, key.TaskRef, l.OwnerID, ownerID)
		}
		if l.Generation != generation {
			return fmt.Errorf("%w: %s active generation is %d, caller had %d", claim.ErrLeaseNotCurrent, key.TaskRef, l.Generation, generation)
		}
		return nil
	}
	return fmt.Errorf("%w: no active lease for %s", claim.ErrLeaseNotCurrent, key.TaskRef)
}

// BoardDoneFenced is the FAC-147 production approve path. Requires stack,
// ownerID, and generation > 0 (live lease). Closing authority is FAC-132
// DoneRequest (completion receipt or attributable override). Status mutation
// goes through BeginProviderTransition/CompleteProviderTransition only — no
// raw UpdateStatus and no generation minting on conflict.
func BoardDoneFenced(
	ctx context.Context,
	tp provider.TaskProvider,
	stack *provider.ClaimStack,
	key claim.LeaseKey,
	ownerID string,
	generation int64,
	req DoneRequest,
) (*DoneResult, error) {
	if stack == nil || stack.Board == nil || stack.Manager == nil {
		return nil, fmt.Errorf("sync: BoardDoneFenced requires ClaimStack (FAC-147 fail-closed)")
	}
	if ownerID == "" || generation <= 0 {
		return nil, fmt.Errorf("sync: BoardDoneFenced requires live lease owner+generation (FAC-147 fail-closed)")
	}

	ref := NormalizeRef(req.Ref)
	repoDir := req.RepoDir
	if repoDir == "" {
		repoDir = "."
	}

	var proof string
	var override *OverrideRecord
	switch {
	case req.Receipt != nil && req.Override != nil:
		return nil, fmt.Errorf("%w for %s: a manual override cannot accompany a receipt; "+
			"drop one of them so the closing authority is unambiguous", ErrNoEvidence, ref)

	case req.Receipt != nil:
		if req.Lifecycle == nil {
			return nil, fmt.Errorf("%w for %s: no lifecycle state authority configured; "+
				"a receipt alone cannot prove the task is past integration", ErrNoEvidence, ref)
		}
		st, err := req.Lifecycle.CurrentState(ref)
		if err != nil {
			return nil, fmt.Errorf("lifecycle state for %s: %w", ref, err)
		}
		if err := req.Receipt.Validate(repoDir, ref, st); err != nil {
			return nil, fmt.Errorf("%w for %s: %v", ErrNoEvidence, ref, err)
		}
		proof = fmt.Sprintf("completion receipt %s: candidate %s merged as %s, patch %s, verification %s, tier %s, %s reviewed by %s",
			shortDigest(req.Receipt.Digest), shortSHA(req.Receipt.CandidateSHA), shortSHA(req.Receipt.MergeSHA),
			shortDigest(req.Receipt.PatchID), shortDigest(req.Receipt.VerificationDigest),
			req.Receipt.RiskTier, req.Receipt.AuthorFamily, req.Receipt.ReviewerFamily)

	case req.Override != nil:
		rec, err := authorizeOverride(*req.Override)
		if err != nil {
			return nil, fmt.Errorf("%w for %s: %v", ErrNoEvidence, ref, err)
		}
		override = rec
		proof = fmt.Sprintf("manual override by %s under policy %s (%s): %s [evidence: %s]",
			rec.Actor, rec.Policy, rec.Decision, rec.Reason, rec.Evidence)

	default:
		return nil, fmt.Errorf("%w for %s: no completion receipt at %s. A commit naming the ref is a "+
			"discovery hint, not proof. Supply the receipt the integration produced, or close it manually with "+
			"--override-policy/--override-actor/--override-reason/--override-evidence",
			ErrNoEvidence, ref, ReceiptPath(repoDir, ref))
	}

	// Exactly-once: a receipt already recorded never advances the card again.
	log, err := ReadDoneLog(repoDir)
	if err != nil {
		return nil, err
	}
	if req.Receipt != nil {
		for _, rec := range log {
			if rec.ReceiptDigest != "" && rec.ReceiptDigest == req.Receipt.Digest {
				if err := requireLiveLease(ctx, stack.Manager, key, ownerID, generation); err != nil {
					return nil, boardCallErr(fmt.Sprintf("fenced done short-circuit for %s", ref), err)
				}
				return &DoneResult{
					Ref: ref, TaskID: rec.TaskID, Proof: proof,
					Idempotent: true, ReceiptDigest: req.Receipt.Digest,
				}, nil
			}
		}
	}

	tasks, err := tp.ListTasks(ctx, req.ProjectID, "")
	if err != nil {
		return nil, boardCallErr("list tasks", err)
	}
	var task *provider.Task
	for _, t := range tasks {
		if strings.EqualFold(NormalizeRef(t.Ref), ref) {
			task = t
			break
		}
	}
	if task == nil {
		return nil, fmt.Errorf("no task with ref %s on the board", ref)
	}
	if req.Receipt != nil && req.Receipt.TaskID != task.ID {
		return nil, fmt.Errorf("%w for %s: receipt is bound to task id %s but the board card is %s",
			ErrNoEvidence, ref, req.Receipt.TaskID, task.ID)
	}

	// Already done: idempotent success only for the live lease holder.
	if provider.NormalizeStatus(task.Status) == provider.StatusDone {
		if err := requireLiveLease(ctx, stack.Manager, key, ownerID, generation); err != nil {
			return nil, boardCallErr(fmt.Sprintf("fenced done short-circuit for %s", ref), err)
		}
		return &DoneResult{Ref: ref, TaskID: task.ID, Proof: proof, Overridden: override != nil}, nil
	}

	if err := stack.Board.MutateStatus(ctx, stack.Manager, key, ownerID, generation, task.ID, "done"); err != nil {
		return nil, boardCallErr(fmt.Sprintf("fenced status write for %s", ref), err)
	}

	back, err := tp.GetTask(ctx, task.ID)
	if err != nil {
		return nil, boardCallErr(fmt.Sprintf("read-back for %s after status write", ref), err)
	}
	if back.Status != "done" {
		return nil, fmt.Errorf("write reported success but %s reads back as %q", ref, back.Status)
	}

	rec := DoneRecord{
		Timestamp: nowStamp(), Ref: ref, TaskID: task.ID,
		ProviderReadback: back.Status, Override: override,
	}
	if req.Receipt != nil {
		rec.ReceiptDigest = req.Receipt.Digest
		rec.MergeSHA = req.Receipt.MergeSHA
	}
	if err := appendDoneRecord(repoDir, rec); err != nil {
		return nil, fmt.Errorf("%s reads back as done but its closure could not be recorded (re-run to record): %w", ref, err)
	}

	res := &DoneResult{Ref: ref, TaskID: task.ID, Proof: proof, Overridden: override != nil, ReceiptDigest: rec.ReceiptDigest}
	commentBody := "board-done: " + proof
	if commentErr := stack.Board.MutateComment(ctx, stack.Manager, key, ownerID, generation, task.ID, commentBody); commentErr != nil {
		if provider.IsTimeout(commentErr) || provider.IsAmbiguous(commentErr) {
			return nil, boardCallErr("board-done comment", commentErr)
		}
	} else {
		res.CommentPosted = true
	}
	return res, nil
}
