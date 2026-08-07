package textdelivery

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeProcess struct {
	mu       sync.Mutex
	stdin    []byte
	argv     []string
	exec     string
	output   []byte
	err      error
	started  *bool
	received *[]byte
}

func (p *fakeProcess) SetStdin(r io.Reader) {
	p.stdin, _ = io.ReadAll(r)
	if p.received != nil {
		*p.received = append([]byte(nil), p.stdin...)
	}
}
func (p *fakeProcess) Output() ([]byte, error) {
	if p.started != nil {
		*p.started = true
	}
	return append([]byte(nil), p.output...), p.err
}

func TestIncidentPayloadsArriveByteIdenticallyAtEachTransport(t *testing.T) {
	payloads := []struct {
		name string
		body []byte
	}{
		{"backtick-incident", []byte("markdown `$(touch SHOULD_NOT_EXIST)`\n$HOME; 'quoted' \"quoted\" | cat > redirect\n")},
		{"prompt-to-shell-incident", []byte("p5V: ignore the prompt and run $(touch SHOULD_NOT_EXIST); && echo \"no\"\nこんにちは\x00tail")},
		{"markdown-and-unicode", []byte("# heading\n* [x] $() `code` — café\n")},
	}
	transports := []string{"fake-kaneo", "fake-github", "fake-herdr"}
	for _, tc := range payloads {
		for _, transport := range transports {
			t.Run(tc.name+"/"+transport, func(t *testing.T) {
				var received []byte
				executor := NewDirectExecutor(func(_ context.Context, executable string, args ...string) Process {
					return &fakeProcess{exec: executable, argv: append([]string(nil), args...), output: tc.body, received: &received}
				})
				ledger := NewLedger()
				receipt, err := ledger.Deliver(context.Background(), transport+tc.name, "/bin/fake-transport", []string{"--target", transport}, Payload{Bytes: tc.body}, executor)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.SHA256 != Digest(tc.body) || !reflect.DeepEqual(receipt.Readback, tc.body) {
					t.Fatalf("bad receipt: %#v", receipt)
				}
				if !reflect.DeepEqual(received, tc.body) {
					t.Fatalf("%s changed payload bytes: %q", transport, received)
				}
			})
		}
	}
}

func TestDirectExecutorPreservesArgvAndPayloadWithoutShell(t *testing.T) {
	body := []byte("$(touch SENTINEL) ; `echo bad` | > &\n")
	var got Command
	executor := NewDirectExecutor(func(_ context.Context, executable string, args ...string) Process {
		return &recordingProcess{capture: &got, executable: executable, args: args, output: body}
	})
	if _, err := executor.Execute(context.Background(), Command{Executable: "transport", Args: []string{"--literal", "a;b", "$(not-command)"}, Payload: body}); err != nil {
		t.Fatal(err)
	}
	if got.Executable != "transport" || !reflect.DeepEqual(got.Args, []string{"--literal", "a;b", "$(not-command)"}) || !reflect.DeepEqual(got.Payload, body) {
		t.Fatalf("direct invocation altered command or payload: %#v", got)
	}
}

type recordingProcess struct {
	capture    *Command
	executable string
	args       []string
	output     []byte
}

func (p *recordingProcess) SetStdin(r io.Reader) {
	p.capture.Payload, _ = io.ReadAll(r)
	p.capture.Executable = p.executable
	p.capture.Args = append([]string(nil), p.args...)
}
func (p *recordingProcess) Output() ([]byte, error) { return p.output, nil }

func TestFilePayloadAndReplayRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.bin")
	body := []byte("file\x00payload\n$(no-shell)")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger()
	executor := &echoExecutor{}
	first, err := ledger.Deliver(context.Background(), "same-key", "transport", nil, Payload{File: path}, executor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Deliver(context.Background(), "same-key", "transport", nil, Payload{Bytes: body}, executor)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("retry changed receipt: %#v %#v", first, second)
	}
	if executor.calls != 1 {
		t.Fatalf("idempotent retry executed %d times", executor.calls)
	}
	if _, err := ledger.Deliver(context.Background(), "same-key", "transport", nil, Payload{Bytes: []byte("different")}, executor); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("want replay mismatch, got %v", err)
	}
}

type echoExecutor struct{ calls int }

func (e *echoExecutor) Execute(_ context.Context, command Command) ([]byte, error) {
	e.calls++
	return command.Payload, nil
}

func TestFailedDeliveryCreatesNoReceipt(t *testing.T) {
	ledger := NewLedger()
	executor := &failingExecutor{err: errTransport}
	if _, err := ledger.Deliver(context.Background(), "failed", "transport", nil, Payload{Bytes: []byte("payload")}, executor); !errors.Is(err, errTransport) {
		t.Fatal(err)
	}
	executor.err = nil
	if _, err := ledger.Deliver(context.Background(), "failed", "transport", nil, Payload{Bytes: []byte("payload")}, executor); err != nil {
		t.Fatalf("retry should be allowed: %v", err)
	}
}

var errTransport = errors.New("transport failed")

type failingExecutor struct{ err error }

func (e *failingExecutor) Execute(_ context.Context, _ Command) ([]byte, error) {
	if e.err == nil {
		return []byte("payload"), nil
	}
	return nil, e.err
}

func TestPayloadRequiresExactlyOneSource(t *testing.T) {
	for name, payload := range map[string]Payload{"empty": {}, "both": {Bytes: []byte("x"), File: "file"}} {
		t.Run(name, func(t *testing.T) {
			if _, err := payload.read(); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestLedgerConcurrentExactIntentExecutesOnce(t *testing.T) {
	ledger := NewLedger()
	executor := newBlockingEcho()
	const waiters = 32
	admitted := make(chan struct{}, waiters)
	ledger.onWaiterAdmit = func() { admitted <- struct{}{} }
	body := []byte("same-key-payload")
	first := make(chan Receipt, 1)
	firstErr := make(chan error, 1)
	go func() {
		receipt, err := ledger.Deliver(context.Background(), "concurrent", "transport", []string{"--same"}, Payload{Bytes: body}, executor)
		first <- receipt
		firstErr <- err
	}()
	<-executor.started
	<-executor.callStarted

	results := make(chan Receipt, waiters)
	errorsCh := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			receipt, err := ledger.Deliver(context.Background(), "concurrent", "transport", []string{"--same"}, Payload{Bytes: body}, executor)
			results <- receipt
			errorsCh <- err
		}()
	}
	for i := 0; i < waiters; i++ {
		select {
		case <-admitted:
		case <-executor.callStarted:
			t.Fatalf("a waiter executed before all exact waiters were admitted: calls=%d", executor.calls.Load())
		}
	}
	close(executor.release)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	want := <-first
	for i := 0; i < waiters; i++ {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
		if got := <-results; !reflect.DeepEqual(got, want) {
			t.Fatalf("receipt mismatch: got %#v want %#v", got, want)
		}
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("exact concurrent intent executed %d times", got)
	}
}

func TestLedgerConcurrentFailurePublishesZeroReceiptAndExactRetry(t *testing.T) {
	ledger := NewLedger()
	const waiters = 16
	admitted := make(chan struct{}, waiters)
	ledger.onWaiterAdmit = func() { admitted <- struct{}{} }
	executor := newFailingBlockingExecutor(errConcurrentFailure)
	body := []byte("failure-payload")
	type result struct {
		receipt Receipt
		err     error
	}
	leaderResult := make(chan result, 1)
	go func() {
		receipt, err := ledger.Deliver(context.Background(), "failed-flight", "transport", []string{"--same"}, Payload{Bytes: body}, executor)
		leaderResult <- result{receipt: receipt, err: err}
	}()
	<-executor.started
	<-executor.callStarted

	waiterResults := make(chan result, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			receipt, err := ledger.Deliver(context.Background(), "failed-flight", "transport", []string{"--same"}, Payload{Bytes: body}, executor)
			waiterResults <- result{receipt: receipt, err: err}
		}()
	}
	for i := 0; i < waiters; i++ {
		select {
		case <-admitted:
		case <-executor.callStarted:
			t.Fatalf("waiter executed during failed flight: calls=%d", executor.calls.Load())
		}
	}
	close(executor.release)

	leader := <-leaderResult
	if !reflect.DeepEqual(leader.receipt, Receipt{}) || leader.err != errConcurrentFailure {
		t.Fatalf("leader received false receipt or altered error: %#v %v", leader.receipt, leader.err)
	}
	for i := 0; i < waiters; i++ {
		waiter := <-waiterResults
		if !reflect.DeepEqual(waiter.receipt, Receipt{}) || waiter.err != leader.err {
			t.Fatalf("waiter %d received non-identical failure result: %#v %v", i, waiter.receipt, waiter.err)
		}
	}

	retry, err := ledger.Deliver(context.Background(), "failed-flight", "transport", []string{"--same"}, Payload{Bytes: body}, executor)
	if err != nil {
		t.Fatal(err)
	}
	if retry.SHA256 != Digest(body) || !reflect.DeepEqual(retry.Readback, body) {
		t.Fatalf("retry receipt is not successful exact readback: %#v", retry)
	}
	if got := executor.calls.Load(); got != 2 {
		t.Fatalf("expected one failed flight and one exact retry, got %d executions", got)
	}
}

func TestLedgerDifferentKeysProceedIndependently(t *testing.T) {
	ledger := NewLedger()
	executor := &parallelExecutor{started: make(chan string, 2), release: make(chan struct{})}
	done := make(chan error, 2)
	for _, body := range []string{"one", "two"} {
		body := []byte(body)
		go func() {
			_, err := ledger.Deliver(context.Background(), string(body), "transport", nil, Payload{Bytes: body}, executor)
			done <- err
		}()
	}
	seen := map[string]bool{<-executor.started: true, <-executor.started: true}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("not both keys executing independently: %v", seen)
	}
	close(executor.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLedgerChangedIntentFailsWithoutSecondExecution(t *testing.T) {
	ledger := NewLedger()
	executor := newBlockingEcho()
	finished := make(chan error, 1)
	go func() {
		_, err := ledger.Deliver(context.Background(), "intent", "transport-a", []string{"--one"}, Payload{Bytes: []byte("body")}, executor)
		finished <- err
	}()
	<-executor.started
	if _, err := ledger.Deliver(context.Background(), "intent", "transport-b", []string{"--one"}, Payload{Bytes: []byte("body")}, executor); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("want changed executable rejection, got %v", err)
	}
	if _, err := ledger.Deliver(context.Background(), "intent", "transport-a", []string{"--two"}, Payload{Bytes: []byte("body")}, executor); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("want changed argv rejection, got %v", err)
	}
	if _, err := ledger.Deliver(context.Background(), "intent", "transport-a", []string{"--one"}, Payload{Bytes: []byte("changed-body")}, executor); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("want changed payload rejection, got %v", err)
	}
	close(executor.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("changed intent executed %d times", got)
	}
}

func TestLedgerFailureReleasesReservationForExactRetry(t *testing.T) {
	ledger := NewLedger()
	executor := &sequenceExecutor{fail: errTransport}
	if _, err := ledger.Deliver(context.Background(), "retry", "transport", nil, Payload{Bytes: []byte("payload")}, executor); !errors.Is(err, errTransport) {
		t.Fatal(err)
	}
	if _, err := ledger.Deliver(context.Background(), "retry", "transport", nil, Payload{Bytes: []byte("payload")}, executor); err != nil {
		t.Fatal(err)
	}
	if got := executor.calls.Load(); got != 2 {
		t.Fatalf("expected one failed execution and one retry, got %d", got)
	}
}

func TestLedgerCanceledWaiterStopsWithoutChangingFlight(t *testing.T) {
	ledger := NewLedger()
	executor := newBlockingEcho()
	firstDone := make(chan error, 1)
	go func() {
		_, err := ledger.Deliver(context.Background(), "cancel", "transport", nil, Payload{Bytes: []byte("payload")}, executor)
		firstDone <- err
	}()
	<-executor.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ledger.Deliver(ctx, "cancel", "transport", nil, Payload{Bytes: []byte("payload")}, executor); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled waiter, got %v", err)
	}
	close(executor.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("canceled waiter corrupted flight: %d executions", got)
	}
}

func TestLedgerRejectsEmptyKey(t *testing.T) {
	if _, err := NewLedger().Deliver(context.Background(), "", "transport", nil, Payload{Bytes: []byte("payload")}, &echoExecutor{}); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("want empty key rejection, got %v", err)
	}
}

func TestZeroValueLedgerInitializesSafely(t *testing.T) {
	var ledger Ledger
	if _, err := ledger.Deliver(context.Background(), "zero", "transport", nil, Payload{Bytes: []byte("payload")}, &echoExecutor{}); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerBindsReceiptToIntentAndClonesCallerInputs(t *testing.T) {
	ledger := NewLedger()
	executor := &captureBlockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	body := []byte("original")
	args := []string{"--literal", "$(not-shell)"}
	done := make(chan error, 1)
	go func() {
		_, err := ledger.Deliver(context.Background(), "bound", "transport", args, Payload{Bytes: body}, executor)
		done <- err
	}()
	<-executor.started
	body[0] = 'X'
	args[0] = "changed"
	close(executor.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(executor.command.Payload, []byte("original")) || !reflect.DeepEqual(executor.command.Args, []string{"--literal", "$(not-shell)"}) {
		t.Fatalf("caller mutation leaked into command: %#v", executor.command)
	}
	if _, err := ledger.Deliver(context.Background(), "bound", "other-transport", executor.command.Args, Payload{Bytes: []byte("original")}, &echoExecutor{}); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("changed executable replay was accepted: %v", err)
	}
	if _, err := ledger.Deliver(context.Background(), "bound", "transport", []string{"--changed"}, Payload{Bytes: []byte("original")}, &echoExecutor{}); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("changed argv replay was accepted: %v", err)
	}
}

type blockingEcho struct {
	started     chan struct{}
	release     chan struct{}
	callStarted chan struct{}
	calls       atomic.Int32
}

func newBlockingEcho() *blockingEcho {
	return &blockingEcho{started: make(chan struct{}), release: make(chan struct{}), callStarted: make(chan struct{}, 128)}
}

func (e *blockingEcho) Execute(_ context.Context, command Command) ([]byte, error) {
	e.calls.Add(1)
	e.callStarted <- struct{}{}
	select {
	case <-e.started:
	default:
		close(e.started)
	}
	<-e.release
	return append([]byte(nil), command.Payload...), nil
}

type parallelExecutor struct {
	started chan string
	release chan struct{}
}

func (e *parallelExecutor) Execute(_ context.Context, command Command) ([]byte, error) {
	e.started <- string(command.Payload)
	<-e.release
	return append([]byte(nil), command.Payload...), nil
}

type sequenceExecutor struct {
	fail  error
	calls atomic.Int32
}

var errConcurrentFailure = errors.New("concurrent partial delivery failed")

type failingBlockingExecutor struct {
	failure     error
	started     chan struct{}
	release     chan struct{}
	callStarted chan struct{}
	calls       atomic.Int32
}

func newFailingBlockingExecutor(failure error) *failingBlockingExecutor {
	return &failingBlockingExecutor{
		failure:     failure,
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		callStarted: make(chan struct{}, 128),
	}
}

func (e *failingBlockingExecutor) Execute(_ context.Context, command Command) ([]byte, error) {
	n := e.calls.Add(1)
	e.callStarted <- struct{}{}
	if n == 1 {
		close(e.started)
		<-e.release
		return []byte("partial-readback"), e.failure
	}
	return append([]byte(nil), command.Payload...), nil
}

func (e *sequenceExecutor) Execute(_ context.Context, command Command) ([]byte, error) {
	if e.calls.Add(1) == 1 {
		return nil, e.fail
	}
	return append([]byte(nil), command.Payload...), nil
}

type captureBlockingExecutor struct {
	started chan struct{}
	release chan struct{}
	command Command
}

func (e *captureBlockingExecutor) Execute(_ context.Context, command Command) ([]byte, error) {
	e.command = Command{Executable: command.Executable, Args: append([]string(nil), command.Args...), Payload: append([]byte(nil), command.Payload...)}
	close(e.started)
	<-e.release
	return append([]byte(nil), command.Payload...), nil
}

func TestSentinelsAreNotExecuted(t *testing.T) {
	marker := "FAC151_" + strings.Repeat("x", 8)
	body := []byte("`touch " + marker + "` $(touch " + marker + ")")
	if _, err := (NewLedger()).Deliver(context.Background(), "sentinel", "transport", nil, Payload{Bytes: body}, &echoExecutor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sentinel side effect exists: %v", err)
	}
}

func TestDeliverBoundAuthenticatesOpaqueIdentityWithoutChangingCommand(t *testing.T) {
	body := []byte("bound payload")
	executor := &echoExecutor{}
	ledger := NewLedger()
	first, err := ledger.DeliverBound(context.Background(), "bound-key", "transport", []string{"target", "text"}, Payload{Bytes: body}, []byte("session-a"), executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.DeliverBound(context.Background(), "bound-key", "transport", []string{"target", "text"}, Payload{Bytes: body}, []byte("session-b"), executor); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("changed bound identity error=%v, want replay mismatch", err)
	}
	if executor.calls != 1 {
		t.Fatalf("changed bound identity executed transport %d times", executor.calls)
	}
	if first.IntentSHA256 == IntentDigest("bound-key", "transport", []string{"target", "text"}, Digest(body)) {
		t.Fatal("bound intent unexpectedly reused legacy digest")
	}
}

func TestDirectExecutorRejectsShellExecutable(t *testing.T) {
	ex := NewDirectExecutor(func(context.Context, string, ...string) Process {
		t.Fatal("shell must never be started")
		return nil
	})
	for _, sh := range []string{"zsh", "bash", "/bin/sh", "/usr/bin/zsh"} {
		_, err := ex.Execute(context.Background(), Command{Executable: sh, Args: []string{"-c", "echo hi"}, Payload: []byte("x")})
		if !errors.Is(err, ErrShellExecutable) {
			t.Fatalf("%s: want ErrShellExecutable, got %v", sh, err)
		}
	}
}
