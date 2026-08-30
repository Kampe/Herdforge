package lifecycle

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/timeline"
)

var (
	ErrCandidateSupersessionState       = errors.New("lifecycle: candidate supersession requires recovering state")
	ErrCandidateSupersessionConflict    = errors.New("lifecycle: candidate supersession exact fence mismatch")
	ErrCandidateSupersessionIntegration = errors.New("lifecycle: candidate supersession refused during active integration")
)

// CandidateSupersessionRequest is the fully prevalidated authority packet for
// replacing one exact candidate while a task is already Recovering. The
// storage transaction does not discover or infer any identity: every durable
// authority and Git fact is supplied explicitly and fenced against the current
// lifecycle row before the append-only event is written.
type CandidateSupersessionRequest struct {
	TaskRef          string
	TaskID           string
	ProjectID        string
	Repo             string
	ExpectedSequence int64
	LeaseGeneration  int64
	Branch           string
	BaseSHA          string
	OldCandidateSHA  string
	NewCandidateSHA  string
	Worktree         string
	Actor            string
	BuilderSession   string
	BuilderModel     string
	BuilderFamily    string
	EvidenceDigest   string
	IdempotencyKey   string
}

// CandidateSupersessionEvidence is embedded in the immutable lifecycle event.
// The old SHA remains in its prior event; this payload makes the replacement
// edge, authority, and prior sequence directly auditable without reconstructing
// state from commit messages or branch history.
type CandidateSupersessionEvidence struct {
	TaskID          string `json:"task_id"`
	ProjectID       string `json:"project_id"`
	BaseSHA         string `json:"base_sha"`
	Worktree        string `json:"worktree"`
	OldCandidateSHA string `json:"old_candidate_sha"`
	NewCandidateSHA string `json:"new_candidate_sha"`
	PriorSequence   int64  `json:"prior_lifecycle_sequence"`
	BuilderSession  string `json:"builder_session"`
	BuilderModel    string `json:"builder_model"`
	BuilderFamily   string `json:"builder_family"`
}

func validateCandidateSupersessionRequest(req CandidateSupersessionRequest) error {
	for name, value := range map[string]string{
		"task_ref": req.TaskRef, "task_id": req.TaskID, "project_id": req.ProjectID,
		"repo": req.Repo, "branch": req.Branch, "worktree": req.Worktree,
		"actor": req.Actor, "builder_session": req.BuilderSession,
		"builder_model": req.BuilderModel, "builder_family": req.BuilderFamily,
		"idempotency_key": req.IdempotencyKey,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("candidate supersession: %s is required", name)
		}
	}
	if req.ExpectedSequence <= 0 || req.LeaseGeneration <= 0 {
		return fmt.Errorf("candidate supersession: positive prior sequence and lease generation are required")
	}
	if !validSHA(req.BaseSHA) || !validSHA(req.OldCandidateSHA) || !validSHA(req.NewCandidateSHA) || req.OldCandidateSHA == req.NewCandidateSHA {
		return fmt.Errorf("candidate supersession: exact distinct base, old, and replacement SHAs are required")
	}
	if !validDigest(req.EvidenceDigest) {
		return fmt.Errorf("candidate supersession: exact evidence digest is required")
	}
	return nil
}

// SupersedeCandidate atomically appends a Recovering->Recovering event and
// advances the materialized exact candidate. It is the sole sanctioned
// same-generation candidate replacement operation. A winner retry returns the
// original event; a distinct concurrent replacement loses an exact CAS fence.
func (m *Machine) SupersedeCandidate(req CandidateSupersessionRequest) (TransitionResult, error) {
	if err := validateCandidateSupersessionRequest(req); err != nil {
		return TransitionResult{}, err
	}
	evidence := CandidateSupersessionEvidence{
		TaskID: req.TaskID, ProjectID: req.ProjectID, BaseSHA: req.BaseSHA, Worktree: req.Worktree,
		OldCandidateSHA: req.OldCandidateSHA, NewCandidateSHA: req.NewCandidateSHA,
		PriorSequence: req.ExpectedSequence, BuilderSession: req.BuilderSession,
		BuilderModel: req.BuilderModel, BuilderFamily: req.BuilderFamily,
	}
	payloadBytes, err := json.Marshal(evidence)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("candidate supersession: encode evidence: %w", err)
	}
	payload := string(payloadBytes)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timeline != nil && req.TaskRef != m.binding.Task {
		return TransitionResult{}, fmt.Errorf("%w: binding=%q request=%q", ErrTimelineBindingTaskMismatch, m.binding.Task, req.TaskRef)
	}
	tx, err := m.db.Begin()
	if err != nil {
		return TransitionResult{}, fmt.Errorf("candidate supersession: begin: %w", err)
	}
	defer tx.Rollback()

	if existing, err := m.events.eventByIdempotencyKeyQuerier(tx, req.IdempotencyKey); err != nil {
		return TransitionResult{}, err
	} else if existing != nil {
		if !supersessionEventMatches(*existing, req, payload) {
			return TransitionResult{}, fmt.Errorf("%w: key=%s", ErrIdempotencyKeyConflict, req.IdempotencyKey)
		}
		result := TransitionResult{Event: *existing, Replayed: true}
		if err := tx.Rollback(); err != nil {
			return TransitionResult{}, fmt.Errorf("candidate supersession: close replay transaction: %w", err)
		}
		if err := m.readBackCandidateSupersession(req, result.Event); err != nil {
			return TransitionResult{}, err
		}
		return result, nil
	}

	current, err := m.events.currentStateQuerier(tx, req.TaskRef)
	if err != nil {
		return TransitionResult{}, err
	}
	if current == nil || current.State != StateRecovering {
		return TransitionResult{}, fmt.Errorf("%w: task=%s", ErrCandidateSupersessionState, req.TaskRef)
	}
	if current.Repo != req.Repo || current.Seq != req.ExpectedSequence || current.LeaseGeneration != req.LeaseGeneration ||
		current.Branch != req.Branch || current.CandidateSHA != req.OldCandidateSHA {
		return TransitionResult{}, fmt.Errorf("%w: task=%s expected seq=%d lease=%d branch=%s candidate=%s; held seq=%d lease=%d branch=%s candidate=%s",
			ErrCandidateSupersessionConflict, req.TaskRef, req.ExpectedSequence, req.LeaseGeneration, req.Branch, req.OldCandidateSHA,
			current.Seq, current.LeaseGeneration, current.Branch, current.CandidateSHA)
	}
	if locked, err := activeIntegrationTx(tx, req.TaskRef); err != nil {
		return TransitionResult{}, err
	} else if locked {
		return TransitionResult{}, fmt.Errorf("%w: task=%s", ErrCandidateSupersessionIntegration, req.TaskRef)
	}

	ev := Event{
		TaskRef: req.TaskRef, Repo: req.Repo, Seq: current.Seq + 1,
		FromState: StateRecovering, ToState: StateRecovering,
		LeaseGeneration: req.LeaseGeneration, Branch: req.Branch, CandidateSHA: req.NewCandidateSHA,
		Actor: req.Actor, EvidenceDigest: req.EvidenceDigest, Payload: payload,
		IdempotencyKey: req.IdempotencyKey, CreatedAt: time.Now().UTC(),
	}
	res, err := tx.Exec(`INSERT INTO lifecycle_events (
		task_ref, repo, seq, from_state, to_state, provider_revision,
		lease_generation, branch, candidate_sha, actor, evidence_digest,
		payload, idempotency_key, created_at
	) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.TaskRef, ev.Repo, ev.Seq, string(ev.FromState), string(ev.ToState), ev.LeaseGeneration,
		ev.Branch, ev.CandidateSHA, ev.Actor, ev.EvidenceDigest, ev.Payload, ev.IdempotencyKey, ev.CreatedAt)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("%w: insert candidate supersession event: %v", ErrConcurrentModification, err)
	}
	ev.ID, _ = res.LastInsertId()
	cas, err := tx.Exec(`UPDATE lifecycle_task_state SET
		seq = ?, candidate_sha = ?, updated_at = ?
		WHERE task_ref = ? AND repo = ? AND state = ? AND seq = ? AND lease_generation = ? AND branch = ? AND candidate_sha = ?`,
		ev.Seq, ev.CandidateSHA, ev.CreatedAt, req.TaskRef, req.Repo, string(StateRecovering),
		req.ExpectedSequence, req.LeaseGeneration, req.Branch, req.OldCandidateSHA)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("%w: update candidate supersession state: %v", ErrConcurrentModification, err)
	}
	if rows, _ := cas.RowsAffected(); rows != 1 {
		return TransitionResult{}, fmt.Errorf("%w: task=%s expected prior seq=%d", ErrConcurrentModification, req.TaskRef, req.ExpectedSequence)
	}
	if err := tx.Commit(); err != nil {
		return TransitionResult{}, fmt.Errorf("candidate supersession: commit: %w", err)
	}
	result := TransitionResult{Event: ev}
	if err := m.readBackCandidateSupersession(req, ev); err != nil {
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
			return TransitionResult{}, fmt.Errorf("candidate supersession: append timeline event: %w", err)
		}
	}
	return result, nil
}

func supersessionEventMatches(ev Event, req CandidateSupersessionRequest, payload string) bool {
	return ev.TaskRef == req.TaskRef && ev.Repo == req.Repo && ev.Seq == req.ExpectedSequence+1 &&
		ev.FromState == StateRecovering && ev.ToState == StateRecovering &&
		ev.LeaseGeneration == req.LeaseGeneration && ev.Branch == req.Branch &&
		ev.CandidateSHA == req.NewCandidateSHA && ev.Actor == req.Actor &&
		ev.EvidenceDigest == req.EvidenceDigest && ev.Payload == payload
}

func activeIntegrationTx(tx *sql.Tx, taskRef string) (bool, error) {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'lifecycle_service_integration_locks'`).Scan(&exists); err != nil {
		return false, fmt.Errorf("candidate supersession: inspect integration admission: %w", err)
	}
	if exists == 0 {
		return false, nil
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM lifecycle_service_integration_locks WHERE task_ref = ?`, taskRef).Scan(&count); err != nil {
		return false, fmt.Errorf("candidate supersession: inspect integration admission: %w", err)
	}
	return count > 0, nil
}

func (m *Machine) readBackCandidateSupersession(req CandidateSupersessionRequest, event Event) error {
	state, err := m.events.CurrentState(req.TaskRef)
	if err != nil {
		return fmt.Errorf("candidate supersession: state readback: %w", err)
	}
	if state == nil || state.Repo != req.Repo || state.State != StateRecovering ||
		state.Seq != req.ExpectedSequence+1 || state.LeaseGeneration != req.LeaseGeneration ||
		state.Branch != req.Branch || state.CandidateSHA != req.NewCandidateSHA {
		return fmt.Errorf("candidate supersession: exact state readback mismatch")
	}
	stored, err := m.events.EventByIdempotencyKey(req.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("candidate supersession: event readback: %w", err)
	}
	if stored == nil || stored.ID != event.ID || !supersessionEventMatches(*stored, req, event.Payload) {
		return fmt.Errorf("candidate supersession: exact event readback mismatch")
	}
	return nil
}
