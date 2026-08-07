package outbox

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Kampe/Herdforge/pkg/textdelivery"
)

type durableEcho struct{ calls atomic.Int64 }

func (e *durableEcho) Execute(_ context.Context, c textdelivery.Command) ([]byte, error) {
	e.calls.Add(1)
	return append([]byte(nil), c.Payload...), nil
}

func openDurablePair(t *testing.T) (string, *Store, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "receipts.db")
	a, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewStore(path)
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return path, a, b
}

func TestDurableLedgerRestartReadsExactPersistedReceiptOnce(t *testing.T) {
	_, a, b := openDurablePair(t)
	executor := &durableEcho{}
	body := []byte("completed payload `$(never-shell)`\x00")
	first, err := textdelivery.NewDurableLedger(a, 7).Deliver(context.Background(), "receipt-1", "fake-herdr", []string{"--target", "worker"}, textdelivery.Payload{Bytes: body}, executor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := textdelivery.NewDurableLedger(b, 7).Deliver(context.Background(), "receipt-1", "fake-herdr", []string{"--target", "worker"}, textdelivery.Payload{Bytes: body}, executor)
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("executions=%d, want 1", executor.calls.Load())
	}
	if string(second.Readback) != string(body) || second.SHA256 != textdelivery.Digest(body) || second.IntentSHA256 != first.IntentSHA256 || second.Generation != 7 {
		t.Fatalf("persisted receipt changed: first=%#v second=%#v", first, second)
	}
}

func TestDurableLedgerReplayRejectsPolicyInvalidSHAValidReadback(t *testing.T) {
	_, a, b := openDurablePair(t)
	body := []byte("structured payload")
	policy := func(_, readback []byte) bool { return string(readback) == `{"valid":true}` }
	first, err := textdelivery.NewDurableLedgerWithReadbackPolicy(a, 8, policy).Deliver(context.Background(), "receipt-policy", "fake", nil, textdelivery.Payload{Bytes: body}, textdelivery.ExecutorFunc(func(context.Context, textdelivery.Command) ([]byte, error) {
		return []byte(`{"valid":true}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	invalid := []byte(`{"valid":false}`)
	if _, err := a.DB().Exec(`UPDATE text_delivery_receipts SET readback=?, readback_sha256=? WHERE key=?`, invalid, textdelivery.Digest(invalid), first.Key); err != nil {
		t.Fatal(err)
	}
	executor := &durableEcho{}
	if _, err := textdelivery.NewDurableLedgerWithReadbackPolicy(b, 8, policy).Deliver(context.Background(), first.Key, "fake", nil, textdelivery.Payload{Bytes: body}, executor); !errors.Is(err, textdelivery.ErrReadbackMismatch) {
		t.Fatalf("got %v, want policy readback mismatch", err)
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("policy-invalid completed receipt invoked executor: %d", executor.calls.Load())
	}
}

func TestDurableLedgerRejectsChangedIntentPayloadAndGeneration(t *testing.T) {
	_, a, _ := openDurablePair(t)
	executor := &durableEcho{}
	if _, err := textdelivery.NewDurableLedger(a, 3).Deliver(context.Background(), "receipt-2", "fake", []string{"one"}, textdelivery.Payload{Bytes: []byte("body")}, executor); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		generation int64
		args       []string
		body       []byte
	}{
		{"generation", 4, []string{"one"}, []byte("body")},
		{"args", 3, []string{"two"}, []byte("body")},
		{"payload", 3, []string{"one"}, []byte("changed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := textdelivery.NewDurableLedger(a, tc.generation).Deliver(context.Background(), "receipt-2", "fake", tc.args, textdelivery.Payload{Bytes: tc.body}, executor); !errors.Is(err, ErrDeliveryConflict) {
				t.Fatalf("got %v, want durable conflict", err)
			}
		})
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("changed intents executed: %d", executor.calls.Load())
	}
}

func TestDurableLedgerAcceptedAmbiguitySurvivesRestartWithoutResend(t *testing.T) {
	_, a, b := openDurablePair(t)
	body := []byte("accepted but unproven")
	intent := textdelivery.DeliveryIntent{Key: "receipt-3", Executable: "fake", Args: []string{"one"}, PayloadSHA256: textdelivery.Digest(body), IntentSHA256: textdelivery.IntentDigest("receipt-3", "fake", []string{"one"}, textdelivery.Digest(body)), Generation: 9}
	if _, err := a.ReserveDelivery(intent); err != nil {
		t.Fatal(err)
	}
	executor := &durableEcho{}
	if _, err := textdelivery.NewDurableLedger(b, 9).Deliver(context.Background(), intent.Key, intent.Executable, intent.Args, textdelivery.Payload{Bytes: body}, executor); !errors.Is(err, ErrDeliveryAmbiguous) {
		t.Fatalf("got %v, want accepted ambiguity", err)
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("ambiguous receipt was resent: %d", executor.calls.Load())
	}
}

type barrierExecutor struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (e *barrierExecutor) Execute(_ context.Context, c textdelivery.Command) ([]byte, error) {
	call := e.calls.Add(1)
	if call == 1 {
		close(e.entered)
		<-e.release
	}
	return append([]byte(nil), c.Payload...), nil
}

func TestDurableLedgerConcurrentIndependentInstancesExecuteAtMostOnce(t *testing.T) {
	path, a, b := openDurablePair(t)
	executor := &barrierExecutor{entered: make(chan struct{}), release: make(chan struct{})}
	body := []byte("one durable execution")
	winner := make(chan struct {
		receipt textdelivery.Receipt
		err     error
	}, 1)
	go func() {
		r, err := textdelivery.NewDurableLedger(a, 11).Deliver(context.Background(), "receipt-4", "fake", nil, textdelivery.Payload{Bytes: body}, executor)
		winner <- struct {
			receipt textdelivery.Receipt
			err     error
		}{r, err}
	}()
	<-executor.entered
	loser, err := textdelivery.NewDurableLedger(b, 11).Deliver(context.Background(), "receipt-4", "fake", nil, textdelivery.Payload{Bytes: body}, executor)
	if !errors.Is(err, ErrDeliveryAmbiguous) || !reflect.DeepEqual(loser, textdelivery.Receipt{}) {
		t.Fatalf("loser receipt=%#v err=%v, want durable ambiguity", loser, err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("loser executed transport: %d", executor.calls.Load())
	}
	close(executor.release)
	win := <-winner
	if win.err != nil {
		t.Fatal(win.err)
	}
	third, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { third.Close() })
	replay, err := textdelivery.NewDurableLedger(third, 11).Deliver(context.Background(), "receipt-4", "fake", nil, textdelivery.Payload{Bytes: body}, executor)
	if err != nil || !reflect.DeepEqual(replay, win.receipt) {
		t.Fatalf("replay=%#v err=%v winner=%#v", replay, err, win.receipt)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("post-completion replay executed transport: %d", executor.calls.Load())
	}
}

func TestCompleteDeliveryCASRejectsCompletionAfterExternalStateChange(t *testing.T) {
	_, a, b := openDurablePair(t)
	body := []byte("cas proof")
	intent := textdelivery.DeliveryIntent{Key: "receipt-cas", Executable: "fake", PayloadSHA256: textdelivery.Digest(body), IntentSHA256: textdelivery.IntentDigest("receipt-cas", "fake", nil, textdelivery.Digest(body)), Generation: 14}
	if _, err := a.ReserveDelivery(intent); err != nil {
		t.Fatal(err)
	}
	a.beforeCompleteCASHook = func() {
		if _, err := b.DB().Exec(`UPDATE text_delivery_receipts SET state='completed', readback=?, readback_sha256=?, updated_at=CURRENT_TIMESTAMP WHERE key=?`, body, textdelivery.Digest(body), intent.Key); err != nil {
			t.Fatalf("external completion: %v", err)
		}
	}
	if _, err := a.CompleteDelivery(intent, body); !errors.Is(err, ErrDeliveryCorrupt) {
		t.Fatalf("completion after external state change error=%v, want CAS corruption", err)
	}
}

func TestDurableLedgerMissingBackendFailsClosedBeforeExecutor(t *testing.T) {
	var ledger = textdelivery.NewDurableLedger(nil, 12)
	executor := &durableEcho{}
	if _, err := ledger.Deliver(context.Background(), "missing-backend", "fake", nil, textdelivery.Payload{Bytes: []byte("body")}, executor); !errors.Is(err, textdelivery.ErrDurableCorrupt) {
		t.Fatalf("got %v, want durable corruption", err)
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("missing backend executed transport: %d", executor.calls.Load())
	}
}

func TestDurableLedgerCorruptOrMixedReceiptFailsClosed(t *testing.T) {
	_, a, _ := openDurablePair(t)
	executor := &durableEcho{}
	body := []byte("corruption proof")
	if _, err := textdelivery.NewDurableLedger(a, 13).Deliver(context.Background(), "receipt-corrupt-completed", "fake", nil, textdelivery.Payload{Bytes: body}, executor); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().Exec(`UPDATE text_delivery_receipts SET readback_sha256=NULL WHERE key=?`, "receipt-corrupt-completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := textdelivery.NewDurableLedger(a, 13).Deliver(context.Background(), "receipt-corrupt-completed", "fake", nil, textdelivery.Payload{Bytes: body}, executor); !errors.Is(err, ErrDeliveryCorrupt) {
		t.Fatalf("got %v, want corrupt completed receipt", err)
	}

	accepted := textdelivery.DeliveryIntent{Key: "receipt-corrupt-accepted", Executable: "fake", PayloadSHA256: textdelivery.Digest(body), IntentSHA256: textdelivery.IntentDigest("receipt-corrupt-accepted", "fake", nil, textdelivery.Digest(body)), Generation: 13}
	if _, err := a.ReserveDelivery(accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().Exec(`UPDATE text_delivery_receipts SET readback=? WHERE key=?`, []byte("unproven"), accepted.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := textdelivery.NewDurableLedger(a, 13).Deliver(context.Background(), accepted.Key, accepted.Executable, nil, textdelivery.Payload{Bytes: body}, executor); !errors.Is(err, ErrDeliveryCorrupt) {
		t.Fatalf("got %v, want corrupt accepted receipt", err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("corrupt receipt caused execution: %d", executor.calls.Load())
	}
}
