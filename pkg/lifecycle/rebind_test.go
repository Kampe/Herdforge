package lifecycle

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

const rebindCandidate = "63a61cd895b6bf11df7b704c1c27d163608bbd78"

func seedGenerationOne(t *testing.T, path string) (*Machine, TaskState) {
	t.Helper()
	m, err := NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-614", Repo: "herdforge", To: StateEligible, Actor: "worker",
		IdempotencyKey: "shot:fac-614:g1", LeaseGeneration: 1,
		Branch: "herd/fac-614", CandidateSHA: rebindCandidate,
	}); err != nil {
		m.Close()
		t.Fatal(err)
	}
	state, err := m.EventStore().CurrentState("FAC-614")
	if err != nil || state == nil {
		m.Close()
		t.Fatalf("seed readback state=%+v err=%v", state, err)
	}
	return m, *state
}

func rebindRequest(state TaskState) GenerationRebindRequest {
	return GenerationRebindRequest{
		Expected: state, LeaseGeneration: 2, ProviderRevision: "provider-revision-2",
		Actor: "coordinator", IdempotencyKey: "dispatch-recovery:fac-614:g1:g2",
		EvidenceDigest: "sha256:evidence", Payload: `{"graph_revision":"graph-2"}`,
	}
}

func TestGenerationRebindPreservesHistoryAndRetriesIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	m, prior := seedGenerationOne(t, path)
	defer m.Close()

	first, err := m.RebindGeneration(rebindRequest(prior))
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.State.LeaseGeneration != 2 || first.State.State != prior.State || first.State.CandidateSHA != rebindCandidate {
		t.Fatalf("fresh rebind=%+v", first)
	}
	retry, err := m.RebindGeneration(rebindRequest(prior))
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Replayed || retry.State != first.State {
		t.Fatalf("retry=%+v first=%+v", retry, first)
	}
	events, err := m.EventStore().Events(prior.TaskRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].LeaseGeneration != 1 || events[0].CandidateSHA != rebindCandidate ||
		events[1].FromState != StateEligible || events[1].ToState != StateRecovering ||
		events[2].FromState != StateRecovering || events[2].ToState != StateEligible {
		t.Fatalf("immutable lifecycle history=%+v", events)
	}
}

func TestGenerationRebindIdentityAndGenerationGuardsDoNotMutate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GenerationRebindRequest)
	}{
		{name: "task", mutate: func(r *GenerationRebindRequest) { r.Expected.TaskRef = "FAC-OTHER" }},
		{name: "repo", mutate: func(r *GenerationRebindRequest) { r.Expected.Repo = "foreign" }},
		{name: "sequence", mutate: func(r *GenerationRebindRequest) { r.Expected.Seq++ }},
		{name: "state", mutate: func(r *GenerationRebindRequest) { r.Expected.State = StateBuilding }},
		{name: "candidate", mutate: func(r *GenerationRebindRequest) { r.Expected.CandidateSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{name: "generation gap", mutate: func(r *GenerationRebindRequest) { r.LeaseGeneration = 3 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lifecycle.db")
			m, prior := seedGenerationOne(t, path)
			defer m.Close()
			req := rebindRequest(prior)
			tt.mutate(&req)
			if _, err := m.RebindGeneration(req); !errors.Is(err, ErrGenerationRebindConflict) {
				t.Fatalf("err=%v, want ErrGenerationRebindConflict", err)
			}
			state, err := m.EventStore().CurrentState(prior.TaskRef)
			if err != nil || state == nil || *state != prior {
				t.Fatalf("refusal mutated state: before=%+v after=%+v err=%v", prior, state, err)
			}
		})
	}
}

func TestGenerationRebindConcurrentDifferentEvidenceHasOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	seed, prior := seedGenerationOne(t, path)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	m1, err := NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m1.Close()
	m2, err := NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i, machine := range []*Machine{m1, m2} {
		i, machine := i, machine
		go func() {
			ready.Done()
			<-start
			req := rebindRequest(prior)
			req.EvidenceDigest += string(rune('a' + i))
			_, err := machine.RebindGeneration(req)
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	var success, refused int
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else {
			refused++
		}
	}
	if success != 1 || refused != 1 {
		t.Fatalf("concurrent outcomes success=%d refused=%d", success, refused)
	}
}

func TestGenerationRebindSecondEventFailureRollsBackFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	m, prior := seedGenerationOne(t, path)
	defer m.Close()
	if _, err := m.db.Exec(`CREATE TRIGGER fail_fac669_resume BEFORE INSERT ON lifecycle_events
		WHEN NEW.task_ref = 'FAC-614' AND NEW.to_state = 'eligible' AND NEW.lease_generation = 2
		BEGIN SELECT RAISE(ABORT, 'injected resume failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RebindGeneration(rebindRequest(prior)); err == nil {
		t.Fatal("injected second-event failure was accepted")
	}
	events, err := m.EventStore().Events(prior.TaskRef)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.EventStore().CurrentState(prior.TaskRef)
	if err != nil || state == nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if len(events) != 1 || *state != prior {
		t.Fatalf("partial rebind escaped transaction: events=%+v state=%+v", events, state)
	}
}
