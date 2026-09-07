package beat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func integrationFixture() IntegrationAction {
	return IntegrationAction{CandidateSHA: strings.Repeat("a", 40), PullRequest: 123, Task: "FAC-599", Owner: "coordinator", Target: "wK:p1", Session: "terminal-1", Action: "Run normal integration admission for the exact candidate and PR 123"}
}

func TestIntegrationWakeReplacesPerCandidateAndRejectsDelayedAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wakes.json")
	now := time.Now().UTC()
	a := integrationFixture()
	b := a
	b.CandidateSHA = strings.Repeat("b", 40)
	b.PullRequest = 124
	var sent []IntegrationWake
	send := func(_ context.Context, w IntegrationWake) error { sent = append(sent, w); return nil }
	run := func(actions []IntegrationAction, at time.Time) []IntegrationWake {
		t.Helper()
		w, err := ReconcileIntegrationWakes(context.Background(), path, actions, at, time.Minute, send)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	run([]IntegrationAction{a, b}, now)
	if len(sent) != 2 {
		t.Fatalf("same owner lost a candidate: %+v", sent)
	}
	run([]IntegrationAction{a, b}, now.Add(time.Second))
	if len(sent) != 2 {
		t.Fatal("unchanged observation resent successful delivery")
	}
	a.PullRequest = 125
	run([]IntegrationAction{a, b}, now.Add(2*time.Second))
	if len(sent) != 3 || sent[2].PullRequest != 125 || sent[2].Generation != 2 {
		t.Fatalf("replacement not delivered: %+v", sent)
	}
	if err := AcknowledgeIntegrationWake(path, a.CandidateSHA, 1, now); err == nil {
		t.Fatal("old ack consumed newer intent")
	}
	if err := AcknowledgeIntegrationWake(path, a.CandidateSHA, 2, now); err != nil {
		t.Fatal(err)
	}
	pending := run([]IntegrationAction{a, b}, now.Add(3*time.Second))
	if len(pending) != 1 || pending[0].CandidateSHA != b.CandidateSHA {
		t.Fatalf("consumed wake resurrected: %+v", pending)
	}
	run(nil, now.Add(4*time.Second))
	run([]IntegrationAction{b}, now.Add(5*time.Second))
	if len(sent) != 4 || sent[3].Generation != 2 {
		t.Fatal("new readiness after withdrawal failed to wake")
	}
}

func TestIntegrationWakeFailedDeliveryPersistsAndEscalationDoesNotResetAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wakes.json")
	now := time.Now().UTC()
	a := integrationFixture()
	attempts := 0
	send := func(_ context.Context, w IntegrationWake) error {
		attempts++
		if attempts == 1 {
			return errors.New("offline")
		}
		return nil
	}
	w, err := ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{a}, now, time.Minute, send)
	if err == nil || len(w) != 1 || !w[0].DeliveredAt.IsZero() {
		t.Fatalf("failure lost or treated as delivery: %v %+v", err, w)
	}
	if err := AcknowledgeIntegrationWake(path, a.CandidateSHA, 1, now); err == nil {
		t.Fatal("undelivered wake acknowledged")
	}
	w, err = ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{a}, now.Add(30*time.Second), time.Minute, send)
	if err != nil || w[0].Generation != 1 || w[0].CreatedAt != now {
		t.Fatalf("retry changed identity or age: %v %+v", err, w)
	}
	w, err = ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{a}, now.Add(time.Minute), time.Minute, send)
	if err != nil || !w[0].Escalated || w[0].Generation != 2 || attempts != 3 {
		t.Fatalf("unconsumed wake did not escalate: %v %+v attempts=%d", err, w, attempts)
	}
	_, err = ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{a}, now.Add(2*time.Minute), time.Minute, send)
	if err != nil || attempts != 3 {
		t.Fatal("same escalation repeatedly delivered", err)
	}
}

func TestIntegrationWakeInvalidSnapshotPreservesPriorState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wakes.json")
	now := time.Now().UTC()
	a := integrationFixture()
	calls := 0
	send := func(context.Context, IntegrationWake) error { calls++; return nil }
	if _, err := ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{a}, now, time.Minute, send); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	badAction, badOwner, badSHA, badTarget, badSession := a, a, a, a, a
	badAction.Action = ""
	badOwner.Owner = ""
	badSHA.CandidateSHA = "abc"
	badTarget.Target = ""
	badSession.Session = ""
	for _, bad := range []IntegrationAction{badAction, badOwner, badSHA, badTarget, badSession} {
		if _, err := ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{bad}, now, time.Minute, send); err == nil {
			t.Fatalf("accepted invalid action: %+v", bad)
		}
	}
	if _, err := ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{a, a}, now, time.Minute, send); err == nil {
		t.Fatal("duplicate candidate accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || calls != 1 {
		t.Fatal("invalid observation changed durable state or delivered")
	}
}

func TestIntegrationWakeCorruptStateIsNotAnEmptyQueue(t *testing.T) {
	for _, body := range []string{"null", "{}", "{", "{\"version\":1,\"wakes\":null}"} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wakes.json")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			called := false
			_, err := ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{integrationFixture()}, time.Now(), time.Minute, func(context.Context, IntegrationWake) error { called = true; return nil })
			if err == nil || called {
				t.Fatal("corrupt queue authorized delivery")
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != body {
				t.Fatal("corrupt evidence overwritten", readErr)
			}
		})
	}
}

func TestIntegrationWakeConcurrentBeatsDeliverOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wakes.json")
	now := time.Now().UTC()
	action := integrationFixture()
	var calls atomic.Int32
	start := make(chan struct{})
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			_, err := ReconcileIntegrationWakes(context.Background(), path, []IntegrationAction{action}, now, time.Minute, func(context.Context, IntegrationWake) error { calls.Add(1); return nil })
			results <- err
		}()
	}
	close(start)
	for i := 0; i < 8; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent beats sent %d times", calls.Load())
	}
}

func TestIntegrationWakeCancellationCannotDeliver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wakes.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := ReconcileIntegrationWakes(ctx, path, []IntegrationAction{integrationFixture()}, time.Now(), time.Minute, func(context.Context, IntegrationWake) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled beat delivered: %v called=%t", err, called)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled beat wrote state", err)
	}
}

func TestIntegrationWakePreservesCanonicalAdmissionOrder(t *testing.T) {
	a := integrationFixture()
	b := a
	a.CandidateSHA = strings.Repeat("f", 40)
	a.Task = "FAC-2"
	b.CandidateSHA = strings.Repeat("a", 40)
	b.Task = "FAC-10"
	var sent []string
	_, err := ReconcileIntegrationWakes(context.Background(), filepath.Join(t.TempDir(), "wakes.json"), []IntegrationAction{a, b}, time.Now(), time.Minute, func(_ context.Context, w IntegrationWake) error { sent = append(sent, w.Task); return nil })
	if err != nil || len(sent) != 2 || sent[0] != "FAC-2" || sent[1] != "FAC-10" {
		t.Fatalf("caller priority/ref order changed: %v %v", err, sent)
	}
}
