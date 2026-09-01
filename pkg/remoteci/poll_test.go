package remoteci

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type watchResult struct {
	settlement Settlement
	err        error
}

type sequenceWatcher struct {
	mu      sync.Mutex
	results []watchResult
	calls   int
}

func (w *sequenceWatcher) Watch(_ context.Context, _ Binding) (Settlement, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if len(w.results) == 0 {
		return Settlement{}, errors.New("unexpected provider poll")
	}
	result := w.results[0]
	w.results = w.results[1:]
	return result.settlement, result.err
}

func (w *sequenceWatcher) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

type countingFailureRouter struct {
	mu    sync.Mutex
	calls int
}

func (r *countingFailureRouter) RouteTerminalFailure(context.Context, Settlement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func (r *countingFailureRouter) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestPollerPendingToTerminalPassAndIdempotentRetry(t *testing.T) {
	binding := testBinding("a")
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	watcher := &sequenceWatcher{results: []watchResult{
		{settlement: Settlement{Version: Version1, Binding: binding, State: StatePending}, err: ErrPending},
		{settlement: Settlement{Version: Version1, Binding: binding, State: StatePassed}},
	}}
	poller := Poller{Watcher: watcher, Store: store, PollInterval: time.Millisecond, MaxPolls: 3}

	got, err := poller.Run(context.Background(), binding)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if !got.Registered || got.Polls != 2 || got.Observation != ObservationPassed || got.Settlement.State != StatePassed {
		t.Fatalf("first Run = %+v, want newly registered two-poll PASS", got)
	}

	retry, err := poller.Run(context.Background(), binding)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if retry.Registered || retry.Polls != 0 || retry.Observation != ObservationPassed || retry.Settlement.State != StatePassed {
		t.Fatalf("retry = %+v, want canonical zero-poll PASS", retry)
	}
	if calls := watcher.callCount(); calls != 2 {
		t.Fatalf("provider calls after retry = %d, want 2", calls)
	}
}

func TestPollerTerminalFailurePersistsBlocksAndRoutesOnce(t *testing.T) {
	binding := testBinding("b")
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	watcher := &sequenceWatcher{results: []watchResult{{
		settlement: Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: "required job failed"},
	}}}
	router := &countingFailureRouter{}
	poller := Poller{Watcher: watcher, Store: store, Router: router, PollInterval: time.Millisecond, MaxPolls: 2}

	got, err := poller.Run(context.Background(), binding)
	if !errors.Is(err, ErrTerminalFailure) || got.Observation != ObservationFailed || got.Settlement.State != StateFailed {
		t.Fatalf("first Run = %+v, %v; want durable terminal failure", got, err)
	}
	retry, err := poller.Run(context.Background(), binding)
	if !errors.Is(err, ErrTerminalFailure) || retry.Polls != 0 || retry.Settlement.State != StateFailed {
		t.Fatalf("retry = %+v, %v; want canonical terminal failure", retry, err)
	}
	if calls := watcher.callCount(); calls != 1 {
		t.Fatalf("provider calls = %d, want one", calls)
	}
	if calls := router.callCount(); calls != 1 {
		t.Fatalf("failure route calls = %d, want one", calls)
	}
}

func TestPollerRejectsProviderBindingMismatch(t *testing.T) {
	binding := testBinding("c")
	for name, tc := range map[string]struct {
		mutate  func(*Binding)
		wantErr error
		wantObs Observation
	}{
		"candidate SHA": {
			mutate:  func(b *Binding) { b.CandidateSHA = strings.Repeat("d", 40) },
			wantErr: ErrStale, wantObs: ObservationStale,
		},
		"repository": {
			mutate:  func(b *Binding) { b.Repository = "github.com/Kampe/Other" },
			wantErr: ErrInvalid, wantObs: ObservationAmbiguous,
		},
		"attempt": {
			mutate:  func(b *Binding) { b.Attempt++ },
			wantErr: ErrInvalid, wantObs: ObservationAmbiguous,
		},
		"policy": {
			mutate:  func(b *Binding) { b.PolicyRevision = "other-policy" },
			wantErr: ErrInvalid, wantObs: ObservationAmbiguous,
		},
		"required checks": {
			mutate:  func(b *Binding) { b.RequiredChecks = []string{"other-check"} },
			wantErr: ErrInvalid, wantObs: ObservationAmbiguous,
		},
	} {
		t.Run(name, func(t *testing.T) {
			providerBinding := binding
			providerBinding.RequiredChecks = append([]string(nil), binding.RequiredChecks...)
			tc.mutate(&providerBinding)
			store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			watcher := &sequenceWatcher{results: []watchResult{{
				settlement: Settlement{Version: Version1, Binding: providerBinding, State: StatePassed},
			}}}
			got, err := (Poller{Watcher: watcher, Store: store, PollInterval: time.Millisecond, MaxPolls: 1}).Run(context.Background(), binding)
			if !errors.Is(err, tc.wantErr) || got.Observation != tc.wantObs {
				t.Fatalf("Run = %+v, %v; want %v/%s", got, err, tc.wantErr, tc.wantObs)
			}
		})
	}
}

func TestPollerRejectsMismatchedPendingObservation(t *testing.T) {
	binding := testBinding("3")
	stale := binding
	stale.CandidateSHA = strings.Repeat("4", 40)
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	watcher := &sequenceWatcher{results: []watchResult{{
		settlement: Settlement{Version: Version1, Binding: stale, State: StatePending},
		err:        ErrPending,
	}}}
	got, err := (Poller{Watcher: watcher, Store: store, PollInterval: time.Millisecond, MaxPolls: 2}).Run(context.Background(), binding)
	if !errors.Is(err, ErrStale) || got.Observation != ObservationStale || got.Polls != 1 {
		t.Fatalf("Run = %+v, %v; want immediate stale pending refusal", got, err)
	}
}

func TestPollerFailsOnTerminalReadbackFailure(t *testing.T) {
	binding := testBinding("e")
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	flaky := &terminalReadbackFailureStore{SettlementStore: store}
	watcher := &sequenceWatcher{results: []watchResult{{settlement: Settlement{Version: Version1, Binding: binding, State: StatePassed}}}}
	got, err := (Poller{Watcher: watcher, Store: flaky, PollInterval: time.Millisecond, MaxPolls: 1}).Run(context.Background(), binding)
	if err == nil || got.Observation != ObservationReadbackFailed {
		t.Fatalf("Run = %+v, %v; want terminal readback failure", got, err)
	}
	canonical, loadErr := store.Load(binding)
	if loadErr != nil || canonical.State != StatePassed {
		t.Fatalf("underlying canonical settlement = %+v, %v; want persisted PASS", canonical, loadErr)
	}
}

func TestPollerMutationControlRequiresExactCanonicalReadback(t *testing.T) {
	binding := testBinding("f")
	store := &spoofCanonicalStore{binding: binding}
	watcher := &sequenceWatcher{results: []watchResult{{settlement: Settlement{Version: Version1, Binding: binding, State: StatePassed}}}}
	got, err := (Poller{Watcher: watcher, Store: store, PollInterval: time.Millisecond, MaxPolls: 1}).Run(context.Background(), binding)
	if !errors.Is(err, ErrInvalid) || got.Observation != ObservationAmbiguous {
		t.Fatalf("Run = %+v, %v; want fail-closed mismatched canonical readback", got, err)
	}
}

func TestPollerTimeoutAndRetryBounds(t *testing.T) {
	binding := testBinding("1")
	t.Run("deadline", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		watcher := &repeatingWatcher{err: ErrPending}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		got, err := (Poller{Watcher: watcher, Store: store, PollInterval: time.Second, MaxPolls: 100}).Run(ctx, binding)
		if !errors.Is(err, ErrPollingTimeout) || got.Observation != ObservationTimeout {
			t.Fatalf("Run = %+v, %v; want polling timeout", got, err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("poll deadline exceeded bound: %s", elapsed)
		}
	})
	t.Run("retry count", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		watcher := &repeatingWatcher{err: ErrUnavailable}
		got, err := (Poller{Watcher: watcher, Store: store, PollInterval: time.Millisecond, MaxPolls: 2}).Run(context.Background(), binding)
		if !errors.Is(err, ErrRetryExhausted) || got.Observation != ObservationRetryExhausted || got.Polls != 2 {
			t.Fatalf("Run = %+v, %v; want two-poll retry exhaustion", got, err)
		}
	})
}

type repeatingWatcher struct{ err error }

func (w *repeatingWatcher) Watch(_ context.Context, binding Binding) (Settlement, error) {
	if errors.Is(w.err, ErrPending) {
		return Settlement{Version: Version1, Binding: binding, State: StatePending}, w.err
	}
	return Settlement{}, w.err
}

type terminalReadbackFailureStore struct {
	SettlementStore
	loads int
}

func (s *terminalReadbackFailureStore) Load(binding Binding) (Settlement, error) {
	s.loads++
	if s.loads > 1 {
		return Settlement{}, errors.New("injected terminal readback failure")
	}
	return s.SettlementStore.Load(binding)
}

type spoofCanonicalStore struct {
	binding  Binding
	terminal Settlement
	loads    int
}

func (s *spoofCanonicalStore) Register(binding Binding) (Settlement, bool, error) {
	return Settlement{Version: Version1, Binding: binding, State: StatePending}, true, nil
}

func (s *spoofCanonicalStore) PersistTerminal(settlement Settlement) (bool, error) {
	s.terminal = settlement
	return true, nil
}

func (s *spoofCanonicalStore) Load(binding Binding) (Settlement, error) {
	s.loads++
	if s.loads == 1 {
		return Settlement{Version: Version1, Binding: binding, State: StatePending}, nil
	}
	spoofed := s.terminal
	spoofed.Binding.PolicyRevision = "spoofed-policy"
	return spoofed, nil
}

func testBinding(digit string) Binding {
	return Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat(digit, 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"Build, Preflight & Test Suite"},
	}
}
