package lifecycle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupEligibleCandidate(t *testing.T, path, task, sha string, generation int64) *Machine {
	t.Helper()
	m, err := NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(TransitionRequest{
		TaskRef: task, Repo: "herdforge", To: StateEligible, Actor: "worker-session",
		IdempotencyKey: task + ":eligible", LeaseGeneration: generation, Branch: "herd/" + strings.ToLower(task), CandidateSHA: sha,
	}); err != nil {
		m.Close()
		t.Fatal(err)
	}
	return m
}

func generationReconcileRequest(task, oldSHA, newSHA string) WorkerGenerationReconcileRequest {
	req := WorkerGenerationReconcileRequest{
		TaskRef: task, TaskID: "task-id", ProjectID: "project-id", Repo: "herdforge",
		ExpectedSequence: 1, OldLeaseGeneration: 1, NewLeaseGeneration: 2,
		Branch: "herd/" + strings.ToLower(task), BaseSHA: testSHA("1"),
		OldCandidateSHA: oldSHA, NewCandidateSHA: newSHA,
		Worktree: "./.herd/worktrees/" + strings.ToLower(task),
		Actor:    "worker-session", SessionID: "worker-session",
		BuilderSession: "worker-session", BuilderModel: "gpt-test", BuilderFamily: "openai",
		IdempotencyKey: task + ":generation-reconcile:2:" + newSHA,
	}
	_, digest, err := EncodeWorkerGenerationReconcileEvidence(workerGenerationEvidence(req))
	if err != nil {
		panic(err)
	}
	req.EvidenceDigest = digest
	return req
}

func TestWorkerGenerationReconcilePreservesHistoryAndWinnerRetryIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	m := setupEligibleCandidate(t, path, "FAC-738", oldSHA, 1)
	defer m.Close()

	req := generationReconcileRequest("FAC-738", oldSHA, newSHA)
	first, err := m.ReconcileWorkerGeneration(req)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := m.ReconcileWorkerGeneration(req)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Replayed || retry.Event.ID != first.Event.ID {
		t.Fatalf("winner retry was not exact replay: first=%+v retry=%+v", first, retry)
	}
	state, err := m.EventStore().CurrentState(req.TaskRef)
	if err != nil || state == nil || state.State != StateEligible || state.Seq != 2 ||
		state.LeaseGeneration != 2 || state.CandidateSHA != newSHA {
		t.Fatalf("generation readback = %+v, err=%v", state, err)
	}
	events, err := m.EventStore().Events(req.TaskRef)
	if err != nil || len(events) != 2 || events[0].LeaseGeneration != 1 || events[0].CandidateSHA != oldSHA ||
		events[1].LeaseGeneration != 2 || events[1].CandidateSHA != newSHA {
		t.Fatalf("generation history = %+v, err=%v", events, err)
	}
	var payload WorkerGenerationReconcileEvidence
	if err := json.Unmarshal([]byte(events[1].Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OldLeaseGeneration != 1 || payload.NewLeaseGeneration != 2 ||
		payload.OldCandidateSHA != oldSHA || payload.NewCandidateSHA != newSHA ||
		payload.PriorSequence != 1 || payload.SessionID != req.SessionID {
		t.Fatalf("generation evidence = %+v", payload)
	}
}

func TestWorkerGenerationReconcileExactFencesRefuseWithoutMutation(t *testing.T) {
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	tests := []struct {
		name   string
		mutate func(*WorkerGenerationReconcileRequest)
	}{
		{"task", func(r *WorkerGenerationReconcileRequest) { r.TaskRef = "FAC-WRONG" }},
		{"repo", func(r *WorkerGenerationReconcileRequest) { r.Repo = "wrong" }},
		{"sequence", func(r *WorkerGenerationReconcileRequest) { r.ExpectedSequence++ }},
		{"generation rollback", func(r *WorkerGenerationReconcileRequest) { r.NewLeaseGeneration = 1 }},
		{"generation older", func(r *WorkerGenerationReconcileRequest) { r.NewLeaseGeneration = 0 }},
		{"old generation", func(r *WorkerGenerationReconcileRequest) { r.OldLeaseGeneration = 2 }},
		{"branch", func(r *WorkerGenerationReconcileRequest) { r.Branch = "herd/wrong" }},
		{"stale candidate", func(r *WorkerGenerationReconcileRequest) { r.OldCandidateSHA = testSHA("c") }},
		{"session", func(r *WorkerGenerationReconcileRequest) { r.SessionID = "other" }},
		{"actor session", func(r *WorkerGenerationReconcileRequest) { r.Actor = "other" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lifecycle.db")
			m := setupEligibleCandidate(t, path, "FAC-738", oldSHA, 1)
			defer m.Close()
			req := generationReconcileRequest("FAC-738", oldSHA, newSHA)
			tc.mutate(&req)
			if _, err := m.ReconcileWorkerGeneration(req); err == nil {
				t.Fatal("fenced generation reconcile was accepted")
			}
			state, err := m.EventStore().CurrentState("FAC-738")
			if err != nil || state == nil || state.Seq != 1 || state.LeaseGeneration != 1 || state.CandidateSHA != oldSHA {
				t.Fatalf("refusal mutated lifecycle: state=%+v err=%v", state, err)
			}
		})
	}
}

func TestWorkerGenerationReconcileRefusesIntegrationAndTerminalStates(t *testing.T) {
	for _, state := range []State{StateIntegrationQueued, StateIntegrated, StateReconciled, StateCleaned, StateClaimed, StateBuilding} {
		t.Run(string(state), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lifecycle.db")
			oldSHA := testSHA("a")
			m := setupEligibleCandidate(t, path, "FAC-738", oldSHA, 1)
			defer m.Close()
			if _, err := m.db.Exec(`UPDATE lifecycle_task_state SET state = ? WHERE task_ref = 'FAC-738'`, string(state)); err != nil {
				t.Fatal(err)
			}
			if _, err := m.ReconcileWorkerGeneration(generationReconcileRequest("FAC-738", oldSHA, testSHA("b"))); !errors.Is(err, ErrWorkerGenerationReconcileState) {
				t.Fatalf("state %s got %v", state, err)
			}
			current, err := m.EventStore().CurrentState("FAC-738")
			if err != nil || current == nil || current.State != state || current.Seq != 1 || current.LeaseGeneration != 1 || current.CandidateSHA != oldSHA {
				t.Fatalf("refusal mutated state: current=%+v err=%v", current, err)
			}
		})
	}
}

func TestWorkerGenerationReconcileKeepsPriorEvidenceSHABoundAndTransfersNoReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	m := setupEligibleCandidate(t, path, "FAC-738", oldSHA, 1)
	defer m.Close()
	if _, err := NewService(m); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := m.db.Exec(`INSERT INTO lifecycle_service_candidates
		(id, task_ref, git_sha, base_sha, evidence_digest, record_json, created_at)
		VALUES ('old-candidate', 'FAC-738', ?, ?, ?, '{}', ?)`, oldSHA, testSHA("1"), testDigest("2"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO lifecycle_service_receipts
		(id, candidate_id, candidate_sha, kind, outcome, evidence_digest, record_json, created_at)
		VALUES ('verify-pass', 'old-candidate', ?, 'verification', 'pass', ?, '{}', ?)`, oldSHA, testDigest("3"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReconcileWorkerGeneration(generationReconcileRequest("FAC-738", oldSHA, newSHA)); err != nil {
		t.Fatal(err)
	}
	var oldCount, newCount int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_service_receipts WHERE candidate_sha = ?`, oldSHA).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_service_receipts WHERE candidate_sha = ?`, newSHA).Scan(&newCount); err != nil {
		t.Fatal(err)
	}
	if oldCount == 0 || newCount != 0 {
		t.Fatalf("receipt evidence transfer: old=%d new=%d", oldCount, newCount)
	}
}

func TestWorkerGenerationReconcileDigestBindsOldNewGenerationCandidateAndSequence(t *testing.T) {
	oldSHA, newSHA := testSHA("a"), testSHA("b")
	req := generationReconcileRequest("FAC-738", oldSHA, newSHA)
	complete, completeDigest, err := EncodeWorkerGenerationReconcileEvidence(workerGenerationEvidence(req))
	if err != nil {
		t.Fatal(err)
	}
	if req.EvidenceDigest != completeDigest {
		t.Fatalf("fixture digest does not match encoded generation evidence")
	}

	narrow := req
	narrow.OldLeaseGeneration = 0
	narrow.NewLeaseGeneration = 0
	narrow.OldCandidateSHA = ""
	narrow.ExpectedSequence = 0
	_, narrowDigest, err := EncodeWorkerGenerationReconcileEvidence(workerGenerationEvidence(narrow))
	if err != nil {
		t.Fatal(err)
	}
	if narrowDigest == completeDigest {
		t.Fatal("digest is unchanged when old/new generation, old candidate, and prior sequence are removed")
	}

	mutations := []struct {
		name   string
		mutate func(*WorkerGenerationReconcileEvidence)
	}{
		{"old generation", func(e *WorkerGenerationReconcileEvidence) { e.OldLeaseGeneration = 9 }},
		{"new generation", func(e *WorkerGenerationReconcileEvidence) { e.NewLeaseGeneration = 9 }},
		{"old candidate", func(e *WorkerGenerationReconcileEvidence) { e.OldCandidateSHA = testSHA("c") }},
		{"new candidate", func(e *WorkerGenerationReconcileEvidence) { e.NewCandidateSHA = testSHA("d") }},
		{"prior sequence", func(e *WorkerGenerationReconcileEvidence) { e.PriorSequence = 9 }},
		{"session", func(e *WorkerGenerationReconcileEvidence) { e.SessionID = "other" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lifecycle.db")
			m := setupEligibleCandidate(t, path, "FAC-738", oldSHA, 1)
			defer m.Close()
			evidence := workerGenerationEvidence(req)
			tc.mutate(&evidence)
			_, wrongDigest, err := EncodeWorkerGenerationReconcileEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if wrongDigest == completeDigest {
				t.Fatalf("digest did not bind %s", tc.name)
			}
			forged := req
			forged.EvidenceDigest = wrongDigest
			if _, err := m.ReconcileWorkerGeneration(forged); err == nil {
				t.Fatal("digest that omits or mutates a generation fence was accepted")
			}
			state, err := m.EventStore().CurrentState("FAC-738")
			if err != nil || state == nil || state.Seq != 1 || state.LeaseGeneration != 1 || state.CandidateSHA != oldSHA {
				t.Fatalf("forged digest mutated lifecycle: state=%+v err=%v", state, err)
			}
		})
	}

	var payload WorkerGenerationReconcileEvidence
	if err := json.Unmarshal(complete, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OldLeaseGeneration != 1 || payload.NewLeaseGeneration != 2 ||
		payload.OldCandidateSHA != oldSHA || payload.NewCandidateSHA != newSHA || payload.PriorSequence != 1 {
		t.Fatalf("encoded generation evidence omitted fences: %+v", payload)
	}
}

func TestWorkerGenerationReconcileSourceKeepsGenerationSessionCandidateFences(t *testing.T) {
	source, err := os.ReadFile("generation.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(source)
	for _, needle := range []string{
		"current.LeaseGeneration != req.OldLeaseGeneration",
		"req.NewLeaseGeneration <= req.OldLeaseGeneration",
		"current.CandidateSHA != req.OldCandidateSHA",
		"req.SessionID",
		"AND lease_generation = ?",
		"AND candidate_sha = ?",
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("removing fence %q would silently mutate; production source is missing it", needle)
		}
	}
}
