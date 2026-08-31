package lifecycle

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setupRecoveringCandidate(t *testing.T, path, task, oldSHA string) *Machine {
	t.Helper()
	m, err := NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []TransitionRequest{
		{TaskRef: task, Repo: "herdforge", To: StateEligible, Actor: "worker-session", IdempotencyKey: task + ":eligible", LeaseGeneration: 1, Branch: "herd/" + task, CandidateSHA: oldSHA},
		{TaskRef: task, Repo: "herdforge", To: StateRecovering, Actor: "worker-session", IdempotencyKey: task + ":recovering", LeaseGeneration: 1, Branch: "herd/" + task, CandidateSHA: oldSHA},
	} {
		if _, err := m.Transition(req); err != nil {
			m.Close()
			t.Fatal(err)
		}
	}
	return m
}

func supersessionRequest(task, oldSHA, newSHA string) CandidateSupersessionRequest {
	return CandidateSupersessionRequest{
		TaskRef: task, TaskID: "task-id", ProjectID: "project-id", Repo: "herdforge",
		ExpectedSequence: 2, LeaseGeneration: 1, Branch: "herd/" + task,
		BaseSHA: testSHA("1"), OldCandidateSHA: oldSHA, NewCandidateSHA: newSHA,
		Worktree: "./.herd/worktrees/" + task, Actor: "worker-session",
		BuilderSession: "worker-session", BuilderModel: "gpt-test", BuilderFamily: "openai",
		EvidenceDigest: testDigest("a"), IdempotencyKey: task + ":supersede:" + newSHA,
	}
}

func TestEncodeCandidateSupersessionEvidenceOwnsUnsupportedTypeError(t *testing.T) {
	_, err := EncodeCandidateSupersessionEvidence(make(chan struct{}))
	if err == nil {
		t.Fatal("unsupported evidence type encoded successfully")
	}
	if !errors.Is(err, ErrCandidateSupersessionEncoding) {
		t.Fatalf("encoding error lost shared owner: %v", err)
	}
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("encoding error lost JSON cause: %v", err)
	}
}

func TestCandidateSupersessionPreservesHistoryAndWinnerRetryIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	m := setupRecoveringCandidate(t, path, "FAC-662", oldSHA)
	defer m.Close()

	req := supersessionRequest("FAC-662", oldSHA, newSHA)
	first, err := m.SupersedeCandidate(req)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := m.SupersedeCandidate(req)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Replayed || retry.Event.ID != first.Event.ID {
		t.Fatalf("winner retry was not exact replay: first=%+v retry=%+v", first, retry)
	}
	state, err := m.EventStore().CurrentState(req.TaskRef)
	if err != nil || state == nil || state.State != StateRecovering || state.Seq != 3 || state.CandidateSHA != newSHA {
		t.Fatalf("replacement readback = %+v, err=%v", state, err)
	}
	events, err := m.EventStore().Events(req.TaskRef)
	if err != nil || len(events) != 3 || events[1].CandidateSHA != oldSHA || events[2].CandidateSHA != newSHA {
		t.Fatalf("supersession history = %+v, err=%v", events, err)
	}
	var payload CandidateSupersessionEvidence
	if err := json.Unmarshal([]byte(events[2].Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OldCandidateSHA != oldSHA || payload.NewCandidateSHA != newSHA || payload.PriorSequence != 2 || payload.BuilderSession != req.BuilderSession {
		t.Fatalf("supersession evidence = %+v", payload)
	}
}

func TestCandidateSupersessionExactFencesRefuseWithoutMutation(t *testing.T) {
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	tests := []struct {
		name   string
		mutate func(*CandidateSupersessionRequest)
	}{
		{"task", func(r *CandidateSupersessionRequest) { r.TaskRef = "FAC-WRONG" }},
		{"repo", func(r *CandidateSupersessionRequest) { r.Repo = "wrong" }},
		{"sequence", func(r *CandidateSupersessionRequest) { r.ExpectedSequence++ }},
		{"lease", func(r *CandidateSupersessionRequest) { r.LeaseGeneration++ }},
		{"branch", func(r *CandidateSupersessionRequest) { r.Branch = "herd/wrong" }},
		{"candidate", func(r *CandidateSupersessionRequest) { r.OldCandidateSHA = testSHA("c") }},
		{"same candidate", func(r *CandidateSupersessionRequest) { r.NewCandidateSHA = r.OldCandidateSHA }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lifecycle.db")
			m := setupRecoveringCandidate(t, path, "FAC-662", oldSHA)
			defer m.Close()
			req := supersessionRequest("FAC-662", oldSHA, newSHA)
			tc.mutate(&req)
			if _, err := m.SupersedeCandidate(req); err == nil {
				t.Fatal("fenced supersession was accepted")
			}
			state, err := m.EventStore().CurrentState("FAC-662")
			if err != nil || state == nil || state.Seq != 2 || state.CandidateSHA != oldSHA {
				t.Fatalf("refusal mutated lifecycle: state=%+v err=%v", state, err)
			}
		})
	}
}

func TestCandidateSupersessionRefusesNonRecoveringAndActiveIntegration(t *testing.T) {
	t.Run("non-recovering", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lifecycle.db")
		m, err := NewMachine(path)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		oldSHA := testSHA("a")
		if _, err := m.Transition(TransitionRequest{TaskRef: "FAC-662", Repo: "herdforge", To: StateEligible, Actor: "worker-session", IdempotencyKey: "eligible", LeaseGeneration: 1, Branch: "herd/FAC-662", CandidateSHA: oldSHA}); err != nil {
			t.Fatal(err)
		}
		req := supersessionRequest("FAC-662", oldSHA, testSHA("b"))
		req.ExpectedSequence = 1
		if _, err := m.SupersedeCandidate(req); !errors.Is(err, ErrCandidateSupersessionState) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("active integration", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lifecycle.db")
		m := setupRecoveringCandidate(t, path, "FAC-662", testSHA("a"))
		defer m.Close()
		if _, err := NewService(m); err != nil {
			t.Fatal(err)
		}
		if _, err := m.db.Exec(`INSERT INTO lifecycle_service_integration_locks
			(target_branch, task_ref, candidate_id, candidate_sha, owner_principal_id, command_key, acquired_at)
			VALUES ('main', 'FAC-662', 'old', ?, 'owner', 'merge', CURRENT_TIMESTAMP)`, testSHA("a")); err != nil {
			t.Fatal(err)
		}
		if _, err := m.SupersedeCandidate(supersessionRequest("FAC-662", testSHA("a"), testSHA("b"))); !errors.Is(err, ErrCandidateSupersessionIntegration) {
			t.Fatalf("got %v", err)
		}
		state, err := m.EventStore().CurrentState("FAC-662")
		if err != nil || state == nil || state.Seq != 2 || state.CandidateSHA != testSHA("a") {
			t.Fatalf("integration refusal mutated lifecycle: state=%+v err=%v", state, err)
		}
	})
}

func TestCandidateSupersessionKeepsPriorEvidenceSHABoundAndTransfersNoReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	m := setupRecoveringCandidate(t, path, "FAC-662", oldSHA)
	defer m.Close()
	if _, err := NewService(m); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := m.db.Exec(`INSERT INTO lifecycle_service_candidates
		(id, task_ref, git_sha, base_sha, evidence_digest, record_json, created_at)
		VALUES ('old-candidate', 'FAC-662', ?, ?, ?, '{}', ?)`, oldSHA, testSHA("1"), testDigest("2"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO lifecycle_service_receipts
		(id, candidate_id, candidate_sha, kind, outcome, evidence_digest, record_json, created_at)
		VALUES ('verify-pass', 'old-candidate', ?, 'verification', 'pass', ?, '{}', ?)`, oldSHA, testDigest("3"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO lifecycle_service_receipts
		(id, candidate_id, candidate_sha, kind, outcome, evidence_digest, record_json, created_at)
		VALUES ('verify-fail', 'old-candidate', ?, 'verification', 'fail', ?, '{}', ?)`, oldSHA, testDigest("4"), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, outcome string }{{"review-pass", "pass"}, {"review-fail", "fail"}} {
		if _, err := m.db.Exec(`INSERT INTO lifecycle_service_reviews
			(id, candidate_id, candidate_sha, outcome, record_json, created_at)
			VALUES (?, 'old-candidate', ?, ?, '{}', ?)`, row.id, oldSHA, row.outcome, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.db.Exec(`INSERT INTO lifecycle_service_approvals
		(id, candidate_id, candidate_sha, decision, approver_principal_id, record_json, created_at)
		VALUES ('old-approval', 'old-candidate', ?, 'approved', 'operator', '{}', ?)`, oldSHA, now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SupersedeCandidate(supersessionRequest("FAC-662", oldSHA, newSHA)); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"lifecycle_service_receipts", "lifecycle_service_reviews", "lifecycle_service_approvals"} {
		var oldCount, newCount int
		if err := m.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE candidate_sha = ?`, oldSHA).Scan(&oldCount); err != nil {
			t.Fatal(err)
		}
		if err := m.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE candidate_sha = ?`, newSHA).Scan(&newCount); err != nil {
			t.Fatal(err)
		}
		if oldCount == 0 || newCount != 0 {
			t.Fatalf("%s evidence transfer: old=%d new=%d", table, oldCount, newCount)
		}
	}
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := requirePassingEvidenceTx(tx, "old-candidate", newSHA); !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("replacement inherited passing evidence: %v", err)
	}
	service := &Service{machine: m}
	mergeReq := BeginIntegrationRequest{Command: CommandContext{TaskRef: "FAC-662"}, CandidateID: "old-candidate", CandidateSHA: oldSHA, ApprovalID: "old-approval"}
	if err := service.admitMergeTx(tx, mergeReq); !errors.Is(err, ErrApprovalStale) {
		t.Fatalf("replacement inherited prior merge readiness: %v", err)
	}
}

func TestCandidateSupersessionRollsBackEventWhenStateWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	m := setupRecoveringCandidate(t, path, "FAC-662", oldSHA)
	defer m.Close()
	if _, err := m.db.Exec(`CREATE TRIGGER fail_supersession_state
		BEFORE UPDATE OF candidate_sha ON lifecycle_task_state
		WHEN NEW.candidate_sha = '` + newSHA + `'
		BEGIN SELECT RAISE(ABORT, 'injected state write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SupersedeCandidate(supersessionRequest("FAC-662", oldSHA, newSHA)); err == nil {
		t.Fatal("injected partial write did not fail")
	}
	events, err := m.EventStore().Events("FAC-662")
	if err != nil || len(events) != 2 {
		t.Fatalf("failed transaction leaked event: events=%+v err=%v", events, err)
	}
	state, err := m.EventStore().CurrentState("FAC-662")
	if err != nil || state == nil || state.Seq != 2 || state.CandidateSHA != oldSHA {
		t.Fatalf("failed transaction changed state: state=%+v err=%v", state, err)
	}
}

func TestCandidateSupersessionMandatoryReadbackRejectsEveryMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	m := setupRecoveringCandidate(t, path, "FAC-662", oldSHA)
	defer m.Close()
	req := supersessionRequest("FAC-662", oldSHA, newSHA)
	result, err := m.SupersedeCandidate(req)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*CandidateSupersessionRequest, *Event)
	}{
		{"state candidate", func(r *CandidateSupersessionRequest, _ *Event) { r.NewCandidateSHA = testSHA("c") }},
		{"state sequence", func(r *CandidateSupersessionRequest, _ *Event) { r.ExpectedSequence++ }},
		{"event identity", func(_ *CandidateSupersessionRequest, e *Event) { e.ID++ }},
		{"event payload", func(_ *CandidateSupersessionRequest, e *Event) { e.Payload = `{}` }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			changedReq, changedEvent := req, result.Event
			tc.mutate(&changedReq, &changedEvent)
			if err := m.readBackCandidateSupersession(changedReq, changedEvent); err == nil {
				t.Fatal("mandatory exact readback accepted mismatched durable state")
			}
		})
	}
}

func TestCandidateSupersessionRefusesIntegrationAndTerminalLifecycleStates(t *testing.T) {
	for _, state := range []State{StateIntegrationQueued, StateIntegrated, StateReconciled, StateCleaned} {
		t.Run(string(state), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lifecycle.db")
			oldSHA := testSHA("a")
			m := setupRecoveringCandidate(t, path, "FAC-662", oldSHA)
			defer m.Close()
			if _, err := m.db.Exec(`UPDATE lifecycle_task_state SET state = ? WHERE task_ref = 'FAC-662'`, string(state)); err != nil {
				t.Fatal(err)
			}
			if _, err := m.SupersedeCandidate(supersessionRequest("FAC-662", oldSHA, testSHA("b"))); !errors.Is(err, ErrCandidateSupersessionState) {
				t.Fatalf("state %s got %v", state, err)
			}
			current, err := m.EventStore().CurrentState("FAC-662")
			if err != nil || current == nil || current.State != state || current.Seq != 2 || current.CandidateSHA != oldSHA {
				t.Fatalf("refusal mutated state: current=%+v err=%v", current, err)
			}
		})
	}
}

func TestCandidateSupersessionConcurrentDistinctReplacementsHasOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	oldSHA := testSHA("a")
	first := setupRecoveringCandidate(t, path, "FAC-662", oldSHA)
	defer first.Close()
	second, err := NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	requests := []CandidateSupersessionRequest{
		supersessionRequest("FAC-662", oldSHA, testSHA("b")),
		supersessionRequest("FAC-662", oldSHA, testSHA("c")),
	}
	machines := []*Machine{first, second}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = machines[i].SupersedeCandidate(requests[i])
		}(i)
	}
	wg.Wait()
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("distinct replacements successes=%d errors=%v", successes, errs)
	}
	state, err := first.EventStore().CurrentState("FAC-662")
	if err != nil || state == nil || state.Seq != 3 || (state.CandidateSHA != testSHA("b") && state.CandidateSHA != testSHA("c")) {
		t.Fatalf("winner readback=%+v err=%v", state, err)
	}
}
