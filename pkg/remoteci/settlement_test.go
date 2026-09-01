package remoteci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/recovery"
)

func TestSettleRejectsStaleCandidateSHA(t *testing.T) {
	watch := Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("a", 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
	}
	settlement := Settlement{
		Version: Version1,
		Binding: Binding{
			Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("b", 40),
			PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
		},
		State: StatePassed,
	}
	if err := Settle(watch, settlement); !errors.Is(err, ErrStale) {
		t.Fatalf("Settle stale candidate error = %v, want ErrStale", err)
	}
}

func TestNewBindingIncludesCanonicalRequiredCheckIdentity(t *testing.T) {
	first, err := NewBinding(
		"github.com/Kampe/Herdforge", strings.Repeat("2", 40), "merge-policy-v2:example", 3,
		[]string{" lint ", "Build", "build"},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewBinding(
		"github.com/Kampe/Herdforge", strings.Repeat("2", 40), "merge-policy-v2:example", 3,
		[]string{"Build", "lint"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !sameBinding(first, second) || strings.Join(first.RequiredChecks, ",") != "Build,lint" {
		t.Fatalf("canonical bindings differ: first=%+v second=%+v", first, second)
	}
	differentChecks, err := NewBinding(
		"github.com/Kampe/Herdforge", strings.Repeat("2", 40), "merge-policy-v2:example", 3,
		[]string{"Build"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.PolicyRevision == differentChecks.PolicyRevision {
		t.Fatal("required-check set did not change the remote-CI policy identity")
	}
}

func TestStoreDeduplicatesCandidateAndPolicyAndRedactsDiagnostics(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("c", 40), PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"}}
	if _, created, err := store.Register(binding); err != nil || !created {
		t.Fatalf("Register first = created=%v err=%v", created, err)
	}
	if _, created, err := store.Register(binding); err != nil || created {
		t.Fatalf("Register duplicate = created=%v err=%v", created, err)
	}
	if _, _, err := store.Register(Binding{Repository: binding.Repository, CandidateSHA: binding.CandidateSHA, PolicyRevision: binding.PolicyRevision, Attempt: 2, RequiredChecks: []string{"gate"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Register changed attempt = %v, want ErrInvalid", err)
	}
	written, err := store.PersistTerminal(Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: "Authorization: secret-token\n" + strings.Repeat("x", 2048)})
	if err != nil || !written {
		t.Fatalf("PersistTerminal = written=%v err=%v", written, err)
	}
	if written, err := store.PersistTerminal(Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: "Authorization: secret-token\n" + strings.Repeat("x", 2048)}); err != nil || written {
		t.Fatalf("duplicate terminal = written=%v err=%v", written, err)
	}
	records, err := store.readLocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || strings.Contains(records[0].Diagnostic, "secret-token") || len(records[0].Diagnostic) > maxDiagnosticBytes+len("…") {
		t.Fatalf("stored redacted bounded record = %+v", records)
	}
}

func TestTerminalFailureRoutesToRecoveryOnce(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := recovery.Open(filepath.Join(t.TempDir(), "recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("f", 40), PolicyRevision: "p", Attempt: 1, RequiredChecks: []string{"gate"}}
	if _, _, err := store.Register(binding); err != nil {
		t.Fatal(err)
	}
	settlement := Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: "failed"}
	router := RecoveryRouter{Store: r, Run: "run", Task: "task", Actor: "remote-ci", Revision: 1, Graph: "graph"}
	if err := PersistAndRouteTerminal(context.Background(), store, settlement, router); err != nil {
		t.Fatal(err)
	}
	if err := PersistAndRouteTerminal(context.Background(), store, settlement, router); err != nil {
		t.Fatal(err)
	}
	if decisions := r.Decisions("run", "task"); len(decisions) != 1 {
		t.Fatalf("recovery decisions = %+v", decisions)
	}
}

func TestSettlementRequiresExactBinding(t *testing.T) {
	settlement := Settlement{Version: Version1, Binding: Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: "refs/heads/main", PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
	}, State: StatePassed}
	if err := settlement.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
}

func TestStoreConcurrentSameAttemptHasOneCanonicalTerminalSettlement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settlements.jsonl")
	binding := Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("7", 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
	}
	settlement := Settlement{Version: Version1, Binding: binding, State: StatePassed}

	const workers = 24
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			<-start
			if _, _, err := store.Register(binding); err != nil {
				errs <- err
				return
			}
			if _, err := store.PersistTerminal(settlement); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent settlement: %v", err)
		}
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(binding)
	if err != nil {
		t.Fatalf("canonical readback: %v", err)
	}
	if got.State != StatePassed || !sameBinding(got.Binding, binding) {
		t.Fatalf("canonical settlement = %+v, want exact passed binding", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rows := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; rows != 2 {
		t.Fatalf("ledger rows = %d, want one watch plus one terminal settlement\n%s", rows, data)
	}
}
