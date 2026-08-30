package lifecycle

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrGenerationRebindConflict = errors.New("lifecycle: generation rebind conflict")

// GenerationRebindRequest advances only the lifecycle lease fence while
// preserving the exact prior state, branch, and candidate. Expected is a CAS
// snapshot read by the coordinator after it has authenticated the recovered
// dispatch evidence.
type GenerationRebindRequest struct {
	Expected         TaskState
	LeaseGeneration  int64
	ProviderRevision string
	Actor            string
	IdempotencyKey   string
	EvidenceDigest   string
	Payload          string
}

// GenerationRebindResult contains both immutable recovery events and the
// mandatory materialized-state readback. Replayed is true only when the exact
// two-event transaction had already committed.
type GenerationRebindResult struct {
	EnterRecovering Event
	ResumeState     Event
	State           TaskState
	Replayed        bool
}

// RebindGeneration atomically records Expected.State -> Recovering ->
// Expected.State at exactly the next lease generation. The canonical edge set
// remains unchanged; both events commit together or neither does. This keeps
// prior candidate history immutable while advancing the lifecycle fence.
func (m *Machine) RebindGeneration(req GenerationRebindRequest) (GenerationRebindResult, error) {
	if m == nil || m.db == nil || m.events == nil {
		return GenerationRebindResult{}, errors.New("lifecycle: generation rebind machine is required")
	}
	if err := validateGenerationRebindRequest(req); err != nil {
		return GenerationRebindResult{}, err
	}

	var lastBusy error
	for attempt := 0; attempt < 8; attempt++ {
		result, err := m.rebindGenerationAttempt(req)
		if err == nil {
			return result, nil
		}
		if !generationRebindBusy(err) {
			return GenerationRebindResult{}, err
		}
		lastBusy = err
		time.Sleep(time.Duration(attempt+1) * 2 * time.Millisecond)
	}
	return GenerationRebindResult{}, fmt.Errorf("generation rebind: concurrent writer did not settle: %w", lastBusy)
}

func (m *Machine) rebindGenerationAttempt(req GenerationRebindRequest) (GenerationRebindResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, err := m.db.Begin()
	if err != nil {
		return GenerationRebindResult{}, fmt.Errorf("generation rebind: begin tx: %w", err)
	}
	defer tx.Rollback()

	result, fresh, err := m.rebindGenerationTx(tx, req)
	if err != nil {
		return GenerationRebindResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return GenerationRebindResult{}, fmt.Errorf("generation rebind: commit: %w", err)
	}

	// Mandatory post-commit readback. A successful return always proves the
	// exact generation/state/candidate materialization the launcher will rely
	// on; a diverted or partial write is never reported as launchable.
	state, err := m.events.CurrentState(req.Expected.TaskRef)
	if err != nil {
		return GenerationRebindResult{}, fmt.Errorf("generation rebind: readback: %w", err)
	}
	if state == nil || !reboundStateMatches(*state, req) {
		return GenerationRebindResult{}, fmt.Errorf("%w: materialized readback does not match committed recovery", ErrGenerationRebindConflict)
	}
	result.State = *state
	result.Replayed = !fresh
	return result, nil
}

func generationRebindBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "sqlite_busy")
}

func validateGenerationRebindRequest(req GenerationRebindRequest) error {
	e := req.Expected
	if strings.TrimSpace(e.TaskRef) == "" || strings.TrimSpace(e.Repo) == "" || e.Seq < 1 ||
		e.LeaseGeneration < 1 || strings.TrimSpace(e.Branch) == "" || strings.TrimSpace(e.CandidateSHA) == "" {
		return fmt.Errorf("%w: exact prior task/repo/sequence/lease/branch/candidate is required", ErrGenerationRebindConflict)
	}
	if req.LeaseGeneration != e.LeaseGeneration+1 {
		return fmt.Errorf("%w: generation must advance exactly once (held=%d requested=%d)", ErrGenerationRebindConflict, e.LeaseGeneration, req.LeaseGeneration)
	}
	if e.State == StateRecovering || e.State == StateBlocked || IsTerminal(e.State) ||
		!ValidTransition(e.State, StateRecovering) || !ValidTransition(StateRecovering, e.State) {
		return fmt.Errorf("%w: state %s cannot be rebound through recovery", ErrGenerationRebindConflict, e.State)
	}
	if strings.TrimSpace(req.ProviderRevision) == "" || strings.TrimSpace(req.Actor) == "" ||
		strings.TrimSpace(req.IdempotencyKey) == "" || strings.TrimSpace(req.EvidenceDigest) == "" ||
		strings.TrimSpace(req.Payload) == "" {
		return fmt.Errorf("%w: provider, actor, idempotency, evidence, and payload bindings are required", ErrGenerationRebindConflict)
	}
	return nil
}

func (m *Machine) rebindGenerationTx(tx *sql.Tx, req GenerationRebindRequest) (GenerationRebindResult, bool, error) {
	enterKey := req.IdempotencyKey + ":enter"
	resumeKey := req.IdempotencyKey + ":resume"
	enter, err := m.events.eventByIdempotencyKeyQuerier(tx, enterKey)
	if err != nil {
		return GenerationRebindResult{}, false, err
	}
	resume, err := m.events.eventByIdempotencyKeyQuerier(tx, resumeKey)
	if err != nil {
		return GenerationRebindResult{}, false, err
	}
	if enter != nil || resume != nil {
		if enter == nil || resume == nil || !rebindEventsMatch(*enter, *resume, req) {
			return GenerationRebindResult{}, false, fmt.Errorf("%w: partial or conflicting idempotent recovery", ErrGenerationRebindConflict)
		}
		current, err := m.events.currentStateQuerier(tx, req.Expected.TaskRef)
		if err != nil {
			return GenerationRebindResult{}, false, err
		}
		if current == nil || !reboundStateMatches(*current, req) {
			return GenerationRebindResult{}, false, fmt.Errorf("%w: replay state does not match recovery events", ErrGenerationRebindConflict)
		}
		return GenerationRebindResult{EnterRecovering: *enter, ResumeState: *resume, State: *current}, false, nil
	}

	current, err := m.events.currentStateQuerier(tx, req.Expected.TaskRef)
	if err != nil {
		return GenerationRebindResult{}, false, err
	}
	if current == nil || !taskStateExactlyMatches(*current, req.Expected) {
		return GenerationRebindResult{}, false, fmt.Errorf("%w: lifecycle CAS snapshot changed", ErrGenerationRebindConflict)
	}

	enterResult, err := m.transitionTx(tx, TransitionRequest{
		TaskRef: req.Expected.TaskRef, Repo: req.Expected.Repo, To: StateRecovering,
		Actor: req.Actor, IdempotencyKey: enterKey, LeaseGeneration: req.LeaseGeneration,
		ProviderRevision: req.ProviderRevision, Branch: req.Expected.Branch,
		CandidateSHA: req.Expected.CandidateSHA, EvidenceDigest: req.EvidenceDigest, Payload: req.Payload,
	})
	if err != nil {
		return GenerationRebindResult{}, false, fmt.Errorf("generation rebind: enter recovery: %w", err)
	}
	resumeResult, err := m.transitionTx(tx, TransitionRequest{
		TaskRef: req.Expected.TaskRef, Repo: req.Expected.Repo, To: req.Expected.State,
		Actor: req.Actor, IdempotencyKey: resumeKey, LeaseGeneration: req.LeaseGeneration,
		ProviderRevision: req.ProviderRevision, Branch: req.Expected.Branch,
		CandidateSHA: req.Expected.CandidateSHA, EvidenceDigest: req.EvidenceDigest, Payload: req.Payload,
	})
	if err != nil {
		return GenerationRebindResult{}, false, fmt.Errorf("generation rebind: resume prior state: %w", err)
	}
	if enterResult.Replayed || resumeResult.Replayed || !rebindEventsMatch(enterResult.Event, resumeResult.Event, req) {
		return GenerationRebindResult{}, false, fmt.Errorf("%w: fresh recovery produced inconsistent events", ErrGenerationRebindConflict)
	}
	return GenerationRebindResult{EnterRecovering: enterResult.Event, ResumeState: resumeResult.Event}, true, nil
}

func taskStateExactlyMatches(got, want TaskState) bool {
	return got.TaskRef == want.TaskRef && got.Repo == want.Repo && got.State == want.State &&
		got.Seq == want.Seq && got.LeaseGeneration == want.LeaseGeneration &&
		got.Branch == want.Branch && got.CandidateSHA == want.CandidateSHA && got.UpdatedAt.Equal(want.UpdatedAt)
}

func reboundStateMatches(got TaskState, req GenerationRebindRequest) bool {
	want := req.Expected
	return got.TaskRef == want.TaskRef && got.Repo == want.Repo && got.State == want.State &&
		got.Seq == want.Seq+2 && got.LeaseGeneration == req.LeaseGeneration &&
		got.Branch == want.Branch && got.CandidateSHA == want.CandidateSHA
}

func rebindEventsMatch(enter, resume Event, req GenerationRebindRequest) bool {
	want := req.Expected
	return enter.TaskRef == want.TaskRef && enter.Repo == want.Repo && enter.Seq == want.Seq+1 &&
		enter.FromState == want.State && enter.ToState == StateRecovering &&
		enter.LeaseGeneration == req.LeaseGeneration && enter.ProviderRevision == req.ProviderRevision &&
		enter.Branch == want.Branch && enter.CandidateSHA == want.CandidateSHA &&
		enter.Actor == req.Actor && enter.EvidenceDigest == req.EvidenceDigest && enter.Payload == req.Payload &&
		resume.TaskRef == want.TaskRef && resume.Repo == want.Repo && resume.Seq == want.Seq+2 &&
		resume.FromState == StateRecovering && resume.ToState == want.State &&
		resume.LeaseGeneration == req.LeaseGeneration && resume.ProviderRevision == req.ProviderRevision &&
		resume.Branch == want.Branch && resume.CandidateSHA == want.CandidateSHA &&
		resume.Actor == req.Actor && resume.EvidenceDigest == req.EvidenceDigest && resume.Payload == req.Payload
}
