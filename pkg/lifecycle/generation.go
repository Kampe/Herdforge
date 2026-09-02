package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/timeline"
)

var (
	ErrWorkerGenerationReconcileState    = fmt.Errorf("lifecycle: worker generation reconcile requires eligible or recovering state")
	ErrWorkerGenerationReconcileConflict = fmt.Errorf("lifecycle: worker generation reconcile exact fence mismatch")
	ErrWorkerGenerationReconcileEncoding = fmt.Errorf("worker generation reconcile: encode evidence")
)

// WorkerGenerationReconcileRequest is the fully prevalidated authority packet
// for advancing canonical lifecycle generation (and binding the exact newer
// candidate) from a signed live worker callback. Storage does not infer
// identity: every durable fence is supplied and CAS-checked.
type WorkerGenerationReconcileRequest struct {
	TaskRef            string
	TaskID             string
	ProjectID          string
	Repo               string
	ExpectedSequence   int64
	OldLeaseGeneration int64
	NewLeaseGeneration int64
	Branch             string
	BaseSHA            string
	OldCandidateSHA    string
	NewCandidateSHA    string
	Worktree           string
	Actor              string
	SessionID          string
	BuilderSession     string
	BuilderModel       string
	BuilderFamily      string
	EvidenceDigest     string
	IdempotencyKey     string
}

// WorkerGenerationReconcileEvidence is embedded in the immutable event so the
// prior generation, candidate, sequence, and signed session remain auditable.
type WorkerGenerationReconcileEvidence struct {
	TaskID             string `json:"task_id"`
	ProjectID          string `json:"project_id"`
	BaseSHA            string `json:"base_sha"`
	Worktree           string `json:"worktree"`
	OldLeaseGeneration int64  `json:"old_lease_generation"`
	NewLeaseGeneration int64  `json:"new_lease_generation"`
	OldCandidateSHA    string `json:"old_candidate_sha"`
	NewCandidateSHA    string `json:"new_candidate_sha"`
	PriorSequence      int64  `json:"prior_lifecycle_sequence"`
	SessionID          string `json:"session_id"`
	BuilderSession     string `json:"builder_session"`
	BuilderModel       string `json:"builder_model"`
	BuilderFamily      string `json:"builder_family"`
}

// EncodeWorkerGenerationReconcileEvidence is the single owner of generation
// reconcile JSON encoding and its self-authenticating digest. The digest binds
// old/new lease generation, old/new candidate, prior sequence, and session;
// hashing a narrower facts struct cannot satisfy ReconcileWorkerGeneration.
func EncodeWorkerGenerationReconcileEvidence(evidence WorkerGenerationReconcileEvidence) (payload []byte, digest string, err error) {
	payload, err = EncodeCandidateSupersessionEvidence(evidence)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrWorkerGenerationReconcileEncoding, err)
	}
	sum := sha256.Sum256(payload)
	return payload, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func workerGenerationEvidence(req WorkerGenerationReconcileRequest) WorkerGenerationReconcileEvidence {
	return WorkerGenerationReconcileEvidence{
		TaskID: req.TaskID, ProjectID: req.ProjectID, BaseSHA: req.BaseSHA, Worktree: req.Worktree,
		OldLeaseGeneration: req.OldLeaseGeneration, NewLeaseGeneration: req.NewLeaseGeneration,
		OldCandidateSHA: req.OldCandidateSHA, NewCandidateSHA: req.NewCandidateSHA,
		PriorSequence: req.ExpectedSequence, SessionID: req.SessionID,
		BuilderSession: req.BuilderSession, BuilderModel: req.BuilderModel, BuilderFamily: req.BuilderFamily,
	}
}

func validateWorkerGenerationReconcileRequest(req WorkerGenerationReconcileRequest) error {
	for name, value := range map[string]string{
		"task_ref": req.TaskRef, "task_id": req.TaskID, "project_id": req.ProjectID,
		"repo": req.Repo, "branch": req.Branch, "worktree": req.Worktree,
		"actor": req.Actor, "session_id": req.SessionID,
		"builder_session": req.BuilderSession, "builder_model": req.BuilderModel,
		"builder_family": req.BuilderFamily, "idempotency_key": req.IdempotencyKey,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("worker generation reconcile: %s is required", name)
		}
	}
	if req.ExpectedSequence <= 0 || req.OldLeaseGeneration <= 0 || req.NewLeaseGeneration <= 0 {
		return fmt.Errorf("worker generation reconcile: positive prior sequence and lease generations are required")
	}
	if req.NewLeaseGeneration <= req.OldLeaseGeneration {
		return fmt.Errorf("worker generation reconcile: newer lease generation is required")
	}
	if req.SessionID != req.Actor {
		return fmt.Errorf("worker generation reconcile: actor is not the signed session")
	}
	if !validSHA(req.BaseSHA) || !validSHA(req.OldCandidateSHA) || !validSHA(req.NewCandidateSHA) {
		return fmt.Errorf("worker generation reconcile: exact base and candidate SHAs are required")
	}
	if !validDigest(req.EvidenceDigest) {
		return fmt.Errorf("worker generation reconcile: exact evidence digest is required")
	}
	return nil
}

func generationReconcileAllowed(state State) bool {
	return state == StateEligible || state == StateRecovering
}

// ReconcileWorkerGeneration atomically appends a same-state event that
// advances lease generation and binds the exact newer candidate. A winner
// retry returns the original event; mismatched fences refuse without mutation.
func (m *Machine) ReconcileWorkerGeneration(req WorkerGenerationReconcileRequest) (TransitionResult, error) {
	if err := validateWorkerGenerationReconcileRequest(req); err != nil {
		return TransitionResult{}, err
	}
	payloadBytes, digest, err := EncodeWorkerGenerationReconcileEvidence(workerGenerationEvidence(req))
	if err != nil {
		return TransitionResult{}, err
	}
	if req.EvidenceDigest != digest {
		return TransitionResult{}, fmt.Errorf("worker generation reconcile: evidence digest does not bind encoded generation fences")
	}
	payload := string(payloadBytes)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timeline != nil && req.TaskRef != m.binding.Task {
		return TransitionResult{}, fmt.Errorf("%w: binding=%q request=%q", ErrTimelineBindingTaskMismatch, m.binding.Task, req.TaskRef)
	}
	tx, err := m.db.Begin()
	if err != nil {
		return TransitionResult{}, fmt.Errorf("worker generation reconcile: begin: %w", err)
	}
	defer tx.Rollback()

	if existing, err := m.events.eventByIdempotencyKeyQuerier(tx, req.IdempotencyKey); err != nil {
		return TransitionResult{}, err
	} else if existing != nil {
		if !generationReconcileEventMatches(*existing, req, payload) {
			return TransitionResult{}, fmt.Errorf("%w: key=%s", ErrIdempotencyKeyConflict, req.IdempotencyKey)
		}
		result := TransitionResult{Event: *existing, Replayed: true}
		if err := tx.Rollback(); err != nil {
			return TransitionResult{}, fmt.Errorf("worker generation reconcile: close replay transaction: %w", err)
		}
		if err := m.readBackWorkerGenerationReconcile(req, result.Event); err != nil {
			return TransitionResult{}, err
		}
		return result, nil
	}

	current, err := m.events.currentStateQuerier(tx, req.TaskRef)
	if err != nil {
		return TransitionResult{}, err
	}
	if current == nil || !generationReconcileAllowed(current.State) {
		state := State("")
		if current != nil {
			state = current.State
		}
		return TransitionResult{}, fmt.Errorf("%w: task=%s state=%s", ErrWorkerGenerationReconcileState, req.TaskRef, state)
	}
	if current.Repo != req.Repo || current.Seq != req.ExpectedSequence ||
		current.LeaseGeneration != req.OldLeaseGeneration || current.Branch != req.Branch ||
		current.CandidateSHA != req.OldCandidateSHA {
		return TransitionResult{}, exactCandidateFenceMismatch(
			ErrWorkerGenerationReconcileConflict, req.TaskRef, req.ExpectedSequence, req.OldLeaseGeneration, req.Branch, req.OldCandidateSHA, current)
	}
	if locked, err := activeIntegrationTx(tx, req.TaskRef); err != nil {
		return TransitionResult{}, err
	} else if locked {
		return TransitionResult{}, fmt.Errorf("%w: task=%s", ErrCandidateSupersessionIntegration, req.TaskRef)
	}

	ev := Event{
		TaskRef: req.TaskRef, Repo: req.Repo, Seq: current.Seq + 1,
		FromState: current.State, ToState: current.State,
		LeaseGeneration: req.NewLeaseGeneration, Branch: req.Branch, CandidateSHA: req.NewCandidateSHA,
		Actor: req.Actor, EvidenceDigest: req.EvidenceDigest, Payload: payload,
		IdempotencyKey: req.IdempotencyKey, CreatedAt: time.Now().UTC(),
	}
	ev, err = insertSameStateLifecycleEvent(tx, ev)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("%w: insert worker generation reconcile event: %v", ErrConcurrentModification, err)
	}
	cas, err := tx.Exec(`UPDATE lifecycle_task_state SET
		seq = ?, lease_generation = ?, candidate_sha = ?, updated_at = ?
		WHERE task_ref = ? AND repo = ? AND state = ? AND seq = ? AND lease_generation = ? AND branch = ? AND candidate_sha = ?`,
		ev.Seq, ev.LeaseGeneration, ev.CandidateSHA, ev.CreatedAt, req.TaskRef, req.Repo, string(current.State),
		req.ExpectedSequence, req.OldLeaseGeneration, req.Branch, req.OldCandidateSHA)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("%w: update worker generation reconcile state: %v", ErrConcurrentModification, err)
	}
	if rows, _ := cas.RowsAffected(); rows != 1 {
		return TransitionResult{}, fmt.Errorf("%w: task=%s expected prior seq=%d", ErrConcurrentModification, req.TaskRef, req.ExpectedSequence)
	}
	if err := tx.Commit(); err != nil {
		return TransitionResult{}, fmt.Errorf("worker generation reconcile: commit: %w", err)
	}
	result := TransitionResult{Event: ev}
	if err := m.readBackWorkerGenerationReconcile(req, ev); err != nil {
		return TransitionResult{}, err
	}
	if m.timeline != nil {
		envelope, err := timeline.FromLifecycle(m.binding, timeline.LifecycleEvent{
			ID: ev.ID, ToState: string(ev.ToState), Actor: ev.Actor, Evidence: ev.EvidenceDigest, Time: ev.CreatedAt,
		})
		if err != nil {
			return TransitionResult{}, err
		}
		if err := m.timeline.Append(envelope); err != nil {
			return TransitionResult{}, fmt.Errorf("worker generation reconcile: append timeline event: %w", err)
		}
	}
	return result, nil
}

func generationReconcileEventMatches(ev Event, req WorkerGenerationReconcileRequest, payload string) bool {
	return ev.TaskRef == req.TaskRef && ev.Repo == req.Repo && ev.Seq == req.ExpectedSequence+1 &&
		ev.LeaseGeneration == req.NewLeaseGeneration && ev.Branch == req.Branch &&
		ev.CandidateSHA == req.NewCandidateSHA && ev.Actor == req.Actor &&
		ev.EvidenceDigest == req.EvidenceDigest && ev.Payload == payload
}

func (m *Machine) readBackWorkerGenerationReconcile(req WorkerGenerationReconcileRequest, event Event) error {
	state, err := m.events.CurrentState(req.TaskRef)
	if err != nil {
		return fmt.Errorf("worker generation reconcile: state readback: %w", err)
	}
	if state == nil || state.Repo != req.Repo || !generationReconcileAllowed(state.State) ||
		state.Seq != req.ExpectedSequence+1 || state.LeaseGeneration != req.NewLeaseGeneration ||
		state.Branch != req.Branch || state.CandidateSHA != req.NewCandidateSHA {
		return fmt.Errorf("worker generation reconcile: exact state readback mismatch")
	}
	stored, err := m.events.EventByIdempotencyKey(req.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("worker generation reconcile: event readback: %w", err)
	}
	if stored == nil || stored.ID != event.ID || !generationReconcileEventMatches(*stored, req, event.Payload) {
		return fmt.Errorf("worker generation reconcile: exact event readback mismatch")
	}
	return nil
}
