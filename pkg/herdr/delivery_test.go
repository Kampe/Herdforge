package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/outbox"
	"github.com/Kampe/Herdforge/pkg/textdelivery"
)

type captureProcess struct {
	executable string
	args       []string
	stdin      []byte
	out        []byte
	err        error
}

func (p *captureProcess) SetStdin(r io.Reader) {
	if r == nil {
		p.stdin = nil
		return
	}
	b, _ := io.ReadAll(r)
	p.stdin = b
}

func (p *captureProcess) Output() ([]byte, error) { return p.out, p.err }

type captureStarter struct {
	last  *captureProcess
	calls int32
}

func (s *captureStarter) Start(_ context.Context, executable string, args ...string) textdelivery.Process {
	atomic.AddInt32(&s.calls, 1)
	p := &captureProcess{executable: executable, args: append([]string(nil), args...), out: []byte("ok")}
	s.last = p
	return p
}

func TestDeliverOperatorUsesInstalledHerdrPromptContract(t *testing.T) {
	prev := statusProbe
	t.Cleanup(func() { statusProbe = prev })
	var n int32
	statusProbe = func(target string) (string, error) {
		if target != "worker" {
			t.Fatalf("status probe target=%q", target)
		}
		if atomic.AddInt32(&n, 1) == 1 {
			return "idle", nil
		}
		return "working", nil
	}

	starter := &captureStarter{}
	state := filepath.Join(t.TempDir(), "delivery.db")
	payload := []byte("packet with `$(touch NOPE)` and $HOME; | pipes\n")
	proof, err := DeliverOperatorWithExecutor(context.Background(), OperatorDelivery{
		Key: "contract-183", Generation: 4, Target: "worker", Session: "session-4",
		Wait: true, Payload: textdelivery.Payload{Bytes: payload}, StatePath: state, Timeout: time.Second,
	}, textdelivery.NewDirectExecutor(starter.Start))
	if err != nil {
		t.Fatalf("DeliverOperator: %v", err)
	}
	if starter.last == nil {
		t.Fatal("expected herdr process start")
	}
	// Installed contract: herdr agent prompt <TARGET> <TEXT> --wait --until working --timeout MS
	wantPrefix := []string{"agent", "prompt", "worker", string(payload)}
	if len(starter.last.args) < 4 || !reflect.DeepEqual(starter.last.args[:4], wantPrefix) {
		t.Fatalf("argv prefix = %#v, want %#v", starter.last.args, wantPrefix)
	}
	joined := strings.Join(starter.last.args, " ")
	if !strings.Contains(joined, "--wait") || !strings.Contains(joined, "--until") || !strings.Contains(joined, "working") {
		t.Fatalf("wait flags missing from argv: %#v", starter.last.args)
	}
	if bytes.Contains(bytes.Join(byteArgs(starter.last.args), nil), []byte("--session")) {
		t.Fatalf("must not invent --session flag; got %#v", starter.last.args)
	}
	if !proof.Consumed || !proof.Verified || proof.FinalStatus != "working" || !proof.SawWorking {
		t.Fatalf("proof incomplete: %+v", proof)
	}
	if proof.PayloadSHA256 != textdelivery.Digest(payload) {
		t.Fatalf("payload digest mismatch")
	}
}

func byteArgs(args []string) [][]byte {
	out := make([][]byte, len(args))
	for i, a := range args {
		out[i] = []byte(a)
	}
	return out
}

func TestDeliverOperatorBindsIdentityAndKeepsPayloadExactInArgv(t *testing.T) {
	prev := statusProbe
	t.Cleanup(func() { statusProbe = prev })
	var n int32
	statusProbe = func(string) (string, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return "idle", nil
		}
		return "working", nil
	}
	starter := &captureStarter{}
	state := filepath.Join(t.TempDir(), "d.db")
	body := []byte("exact\nbytes\nwith `ticks`")
	proof, err := DeliverOperatorWithExecutor(context.Background(), OperatorDelivery{
		Key: "op-183", Generation: 4, Target: "worker", Session: "s4",
		Payload: textdelivery.Payload{Bytes: body}, StatePath: state, Timeout: time.Second,
	}, textdelivery.NewDirectExecutor(starter.Start))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if starter.last == nil || starter.last.args[3] != string(body) {
		t.Fatalf("TEXT argv element not exact: %#v", starter.last)
	}
	// DirectExecutor also feeds payload on stdin; herdr ignores stdin for prompt.
	// Payload must still be preserved byte-for-byte on the argv TEXT slot.
	if proof.Argv[3] != string(body) {
		t.Fatalf("proof argv text mismatch")
	}
}

func TestDeliverOperatorFreshInstanceReadsDurableProofWithoutResend(t *testing.T) {
	prev := statusProbe
	t.Cleanup(func() { statusProbe = prev })
	var n int32
	statusProbe = func(string) (string, error) {
		if atomic.AddInt32(&n, 1) <= 1 {
			return "idle", nil
		}
		return "working", nil
	}
	starter := &captureStarter{}
	state := filepath.Join(t.TempDir(), "restart.db")
	payload := []byte("restart-me")
	d := OperatorDelivery{Key: "restart-183", Generation: 8, Target: "worker", Session: "s8", Payload: textdelivery.Payload{Bytes: payload}, StatePath: state, Timeout: time.Second}
	if _, err := DeliverOperatorWithExecutor(context.Background(), d, textdelivery.NewDirectExecutor(starter.Start)); err != nil {
		t.Fatalf("first: %v", err)
	}
	firstCalls := atomic.LoadInt32(&starter.calls)
	// Second process: new starter that must not be called.
	starter2 := &captureStarter{}
	proof, err := DeliverOperatorWithExecutor(context.Background(), d, textdelivery.NewDirectExecutor(starter2.Start))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if atomic.LoadInt32(&starter2.calls) != 0 {
		t.Fatalf("restart re-sent prompt: calls=%d", starter2.calls)
	}
	if firstCalls < 1 || !proof.Consumed || proof.PayloadSHA256 != textdelivery.Digest(payload) {
		t.Fatalf("restart proof bad: calls=%d proof=%+v", firstCalls, proof)
	}
}

func TestDeliverOperatorDurableReplayRejectsSwappedTargetOrSession(t *testing.T) {
	prev := statusProbe
	t.Cleanup(func() { statusProbe = prev })
	var n int32
	statusProbe = func(string) (string, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return "idle", nil
		}
		return "working", nil
	}
	state := filepath.Join(t.TempDir(), "id.db")
	payload := []byte("identity")
	starter := &captureStarter{}
	first := OperatorDelivery{Key: "identity-183", Generation: 10, Target: "worker", Session: "session-10", Payload: textdelivery.Payload{Bytes: payload}, StatePath: state, Timeout: time.Second}
	if _, err := DeliverOperatorWithExecutor(context.Background(), first, textdelivery.NewDirectExecutor(starter.Start)); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same key/generation/payload but different session binding must conflict.
	swapped := first
	swapped.Session = "other-session"
	if _, err := DeliverOperatorWithExecutor(context.Background(), swapped, textdelivery.NewDirectExecutor(starter.Start)); err == nil {
		t.Fatal("expected session-binding conflict on durable replay")
	}
}

func TestDeliverOperatorIdleToDoneWithoutWorkingIsNotProof(t *testing.T) {
	prev := statusProbe
	t.Cleanup(func() { statusProbe = prev })
	// Always report idle→done without ever observing working.
	var n int32
	statusProbe = func(string) (string, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return "idle", nil
		}
		return "done", nil
	}
	starter := &captureStarter{}
	state := filepath.Join(t.TempDir(), "idle-done.db")
	_, err := DeliverOperatorWithExecutor(context.Background(), OperatorDelivery{
		Key: "idle-done-183", Generation: 3, Target: "worker",
		Payload: textdelivery.Payload{Bytes: []byte("x")}, StatePath: state, Timeout: 300 * time.Millisecond,
	}, textdelivery.NewDirectExecutor(starter.Start))
	if err == nil {
		t.Fatal("bare idle→done must not complete as consumed")
	}
	if !errors.Is(err, textdelivery.ErrDurableAmbiguous) && !strings.Contains(err.Error(), "no prompt-correlated") {
		t.Fatalf("want consumption ambiguity, got %v", err)
	}
	// Reservation left accepted (ambiguous) — restart must not re-send as success without proof.
	store, err := outbox.NewStore(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
}

func TestDeliverOperatorWorkingStatusIsProof(t *testing.T) {
	prev := statusProbe
	t.Cleanup(func() { statusProbe = prev })
	var n int32
	statusProbe = func(string) (string, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return "idle", nil
		}
		return "working", nil
	}
	state := filepath.Join(t.TempDir(), "work.db")
	proof, err := DeliverOperatorWithExecutor(context.Background(), OperatorDelivery{
		Key: "queued-turn-183", Generation: 12, Target: "worker", Session: "s12",
		Payload: textdelivery.Payload{Bytes: []byte("queued packet")}, StatePath: state, Timeout: time.Second,
	}, textdelivery.NewDirectExecutor((&captureStarter{}).Start))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !proof.Consumed || proof.FinalStatus != "working" || !proof.SawWorking {
		t.Fatalf("%+v", proof)
	}
	var rb operatorReadback
	if err := json.Unmarshal(proof.Readback, &rb); err != nil || !rb.SawWorking {
		t.Fatalf("readback sawWorking missing: %v %+v", err, rb)
	}
}

func TestDeliverOperatorRejectsIncompleteIdentityAndPayloadSource(t *testing.T) {
	state := filepath.Join(t.TempDir(), "bad.db")
	cases := []OperatorDelivery{
		{Key: "", Generation: 1, Target: "t", Payload: textdelivery.Payload{Bytes: []byte("x")}},
		{Key: "k", Generation: 0, Target: "t", Payload: textdelivery.Payload{Bytes: []byte("x")}},
		{Key: "k", Generation: 1, Target: "", Payload: textdelivery.Payload{Bytes: []byte("x")}},
		{Key: "k", Generation: 1, Target: "t", StatePath: state, Payload: textdelivery.Payload{}},
		{Key: "k", Generation: 1, Target: "t", StatePath: state, Payload: textdelivery.Payload{Bytes: []byte("x"), File: "both"}},
		{Key: "k", Generation: 1, Target: "t", StatePath: state, Payload: textdelivery.Payload{Bytes: []byte("x\x00y")}},
	}
	for i, d := range cases {
		if _, err := DeliverOperatorWithExecutor(context.Background(), d, textdelivery.NewDirectExecutor((&captureStarter{}).Start)); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}
