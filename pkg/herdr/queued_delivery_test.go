package herdr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/mail"
)

type interruptRecorder struct {
	mu              sync.Mutex
	calls           []string
	status          string
	pane            string
	paneAfterPrompt string
	enterErr        error
	afterPrompt     func()
	signals         int
	prompted        int
	keys            []string
}

func (r *interruptRecorder) run(args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	joined := strings.Join(args, " ")
	r.calls = append(r.calls, joined)
	switch {
	case len(args) >= 2 && args[0] == "agent" && args[1] == "list":
		return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","pane_id":"pane-live","workspace_id":"wK","agent_status":%q}]}}`, r.status), nil
	case len(args) >= 2 && args[0] == "pane" && args[1] == "read":
		return `{"result":{"text":` + jsonQuote(r.pane) + `}}`, nil
	case len(args) >= 2 && args[0] == "agent" && args[1] == "prompt":
		r.prompted++
		if r.paneAfterPrompt != "" {
			r.pane = r.paneAfterPrompt
		}
		if r.afterPrompt != nil {
			r.afterPrompt()
		}
		return `{"result":{"delivered":true}}`, nil
	case len(args) >= 2 && args[0] == "agent" && args[1] == "send-keys":
		r.keys = append(r.keys, strings.Join(args[3:], " "))
		if r.enterErr != nil && strings.EqualFold(strings.Join(args[3:], " "), "Enter") {
			return "composer refused", r.enterErr
		}
		return `{"result":{"ok":true}}`, nil
	case strings.Contains(joined, "kill") || strings.Contains(joined, "signal"):
		r.signals++
	}
	return `{"result":{"ok":true}}`, nil
}

func (r *interruptRecorder) snapshot() (calls []string, prompted int, keys []string, signals int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...), r.prompted, append([]string(nil), r.keys...), r.signals
}

func installBusyWorker(t *testing.T, status string) (*interruptRecorder, *mail.Mailbox) {
	t.Helper()
	t.Setenv("HERD_WORKSPACE", "wK")
	rec := &interruptRecorder{status: status, pane: "running: sleep 30"}
	t.Cleanup(SetRunHerdrForTest(rec.run))
	box := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	t.Cleanup(SetQueueMailbox(box))
	return rec, box
}

func TestRoutineBusySendQueuesWithoutKeystrokesOrSignals(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	body := "routine status\nsecond line"
	status, err := Send("worker", body, true, time.Second)
	if err != nil {
		t.Fatalf("busy routine send must succeed as durable queue: %v", err)
	}
	if status != StatusQueuedDurable {
		t.Fatalf("status = %q, want %s (pre-existing working is not consumption)", status, StatusQueuedDurable)
	}
	_, prompted, keys, signals := rec.snapshot()
	if prompted != 0 || len(keys) != 0 || signals != 0 {
		t.Fatalf("busy routine send interrupted the pane: prompted=%d keys=%v signals=%d calls=%v", prompted, keys, signals, rec.calls)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != body {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestStartingAndUnknownStatusesQueueRatherThanPreempt(t *testing.T) {
	for _, status := range []string{"starting", "", "unknown"} {
		t.Run(status, func(t *testing.T) {
			label := status
			if label == "" {
				label = "empty"
			}
			rec, _ := installBusyWorker(t, status)
			got, err := Send("worker", "routine "+label, false, time.Second)
			if err != nil || got != StatusQueuedDurable {
				t.Fatalf("status %q: got %q err %v; want queued-durable", status, got, err)
			}
			_, prompted, keys, _ := rec.snapshot()
			if prompted != 0 || len(keys) != 0 {
				t.Fatalf("status %q preempted: prompted=%d keys=%v", status, prompted, keys)
			}
		})
	}
}

func TestIdempotentBusySendWritesOneEnvelope(t *testing.T) {
	_, box := installBusyWorker(t, "working")
	body := "same routine"
	if _, err := Send("worker", body, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := Send("worker", body, false, time.Second); err != nil {
		t.Fatal(err)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("idempotent send duplicated envelopes: %+v", pending)
	}
}

func TestAuthenticatedUrgentStopStillPreempts(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	got, err := SendStatus("worker", "IMMEDIATE STOP from operator.", false, time.Second)
	if err != nil || got != "submitted" {
		t.Fatalf("urgent control = %q, %v; want submitted", got, err)
	}
	_, prompted, keys, _ := rec.snapshot()
	if prompted != 1 {
		t.Fatalf("urgent control must prompt, prompted=%d keys=%v", prompted, keys)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("urgent control must not file a routine envelope: %+v", pending)
	}
}

func TestSamePayloadWithoutAuthorityCannotPreempt(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	got, err := Send("worker", "IMMEDIATE STOP from operator.", false, time.Second)
	if err != nil || got != StatusQueuedDurable {
		t.Fatalf("unauthenticated stop-shaped payload = %q, %v; want queued-durable", got, err)
	}
	_, prompted, keys, _ := rec.snapshot()
	if prompted != 0 || len(keys) != 0 {
		t.Fatalf("unauthenticated stop-shaped payload preempted: prompted=%d keys=%v", prompted, keys)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("unauthenticated payload must queue: %+v", pending)
	}
}

func TestIdleKickoffStillSubmits(t *testing.T) {
	rec, box := installBusyWorker(t, "idle")
	got, err := Send("worker", "short kick", false, time.Second)
	if err != nil || got != "submitted" {
		t.Fatalf("idle kickoff = %q, %v; want submitted", got, err)
	}
	_, prompted, keys, _ := rec.snapshot()
	if prompted != 1 || len(keys) == 0 {
		t.Fatalf("idle kickoff must prompt+enter: prompted=%d keys=%v", prompted, keys)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("idle kickoff queued instead of submitting: %+v", pending)
	}
}

func TestDrainAtWorkingBoundaryDoesNotTouchPane(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	if _, err := Send("worker", "hold this", false, time.Second); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	rec.prompted, rec.keys = 0, nil
	rec.mu.Unlock()
	_, err := DrainQueuedAtBoundary("worker", "wK", box)
	if !errors.Is(err, ErrNotIdleBoundary) {
		t.Fatalf("drain while working: %v", err)
	}
	_, prompted, keys, _ := rec.snapshot()
	if prompted != 0 || len(keys) != 0 {
		t.Fatalf("working drain interrupted the pane: prompted=%d keys=%v", prompted, keys)
	}
}

func TestDrainAtIdleSurfacesThenAckPreventsDuplicate(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	body := "queued packet"
	if _, err := Send("worker", body, false, time.Second); err != nil {
		t.Fatal(err)
	}
	rec.status = "idle"
	rec.paneAfterPrompt = body
	rec.mu.Lock()
	rec.prompted, rec.keys, rec.calls = 0, nil, nil
	rec.mu.Unlock()

	first, err := DrainQueuedAtBoundary("worker", "wK", box)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || !first[0].Delivered || !first[0].Acknowledged {
		t.Fatalf("first drain = %+v", first)
	}
	_, prompted, keys, _ := rec.snapshot()
	if prompted != 1 || len(keys) == 0 {
		t.Fatalf("idle drain must surface: prompted=%d keys=%v", prompted, keys)
	}
	for _, key := range keys {
		if strings.EqualFold(key, "Escape") {
			t.Fatalf("routine drain sent Escape: %v", keys)
		}
	}

	rec.mu.Lock()
	rec.prompted, rec.keys = 0, nil
	rec.mu.Unlock()
	second, err := DrainQueuedAtBoundary("worker", "wK", box)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("acked envelope was processed again: %+v", second)
	}
	_, prompted, keys, _ = rec.snapshot()
	if prompted != 0 || len(keys) != 0 {
		t.Fatalf("repeat drain re-delivered: prompted=%d keys=%v", prompted, keys)
	}
}

func TestDrainEnterFailurePreservesPending(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	body := "enter-fail packet"
	if _, err := Send("worker", body, false, time.Second); err != nil {
		t.Fatal(err)
	}
	rec.status = "idle"
	rec.paneAfterPrompt = body
	rec.enterErr = errors.New("enter refused")
	rec.mu.Lock()
	rec.prompted, rec.keys, rec.calls = 0, nil, nil
	rec.mu.Unlock()

	results, err := DrainQueuedAtBoundary("worker", "wK", box)
	if err == nil {
		t.Fatal("Enter failure must fail closed")
	}
	if len(results) != 1 || !results[0].Delivered || results[0].Acknowledged {
		t.Fatalf("partial Enter failure = %+v", results)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != body {
		t.Fatalf("Enter failure must leave the envelope pending: %+v", pending)
	}
}

func TestDrainUnconsumedComposerPreservesPending(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	body := "composer staged packet"
	if _, err := Send("worker", body, false, time.Second); err != nil {
		t.Fatal(err)
	}
	rec.status = "idle"
	rec.pane = "[Pasted text #1]"
	rec.paneAfterPrompt = "[Pasted text #1]"
	t.Cleanup(SetQueuedProveTimeoutForTest(20 * time.Millisecond))
	rec.mu.Lock()
	rec.prompted, rec.keys, rec.calls = 0, nil, nil
	rec.mu.Unlock()

	results, err := DrainQueuedAtBoundary("worker", "wK", box)
	if err == nil {
		t.Fatal("unconsumed composer must not look acknowledged")
	}
	if len(results) != 1 && len(results) != 0 {
		t.Fatalf("unconsumed drain results = %+v", results)
	}
	for _, result := range results {
		if result.Acknowledged {
			t.Fatalf("unconsumed composer was acked: %+v", results)
		}
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("unconsumed composer must stay pending: %+v", pending)
	}
}

func TestDrainRefreshesSafeBoundaryBeforeEachEnvelope(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	if _, err := Send("worker", "alpha", false, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := Send("worker", "beta", false, time.Second); err != nil {
		t.Fatal(err)
	}
	rec.status = "idle"
	rec.afterPrompt = func() {
		if rec.prompted == 1 {
			rec.status = "working"
			rec.pane = "alpha"
		}
	}
	rec.mu.Lock()
	rec.prompted, rec.keys, rec.calls = 0, nil, nil
	rec.mu.Unlock()

	results, err := DrainQueuedAtBoundary("worker", "wK", box)
	if err != nil && !errors.Is(err, ErrNotIdleBoundary) {
		t.Fatalf("stale-boundary drain: %v", err)
	}
	if rec.prompted != 1 {
		t.Fatalf("second envelope used a stale idle snapshot: prompted=%d results=%+v", rec.prompted, results)
	}
	for _, key := range rec.keys {
		if strings.EqualFold(key, "Escape") {
			t.Fatalf("routine drain sent Escape: %v", rec.keys)
		}
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "beta" {
		t.Fatalf("second envelope must remain pending: %+v", pending)
	}
}

func TestDrainCrashBeforeAckReSurfacesAfterAckDoesNot(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	if _, err := Send("worker", "crash-window", false, time.Second); err != nil {
		t.Fatal(err)
	}
	rec.status = "idle"
	rec.paneAfterPrompt = "crash-window"
	crash := errors.New("simulated crash after deliver")
	first, err := drainQueuedAtBoundary("worker", "wK", box, func(*mail.Envelope) error { return crash })
	if !errors.Is(err, crash) {
		t.Fatalf("crash hook: %v", err)
	}
	if len(first) != 1 || !first[0].Delivered || first[0].Acknowledged {
		t.Fatalf("crash-before-ack result = %+v", first)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("unacked envelope disappeared: %+v", pending)
	}

	second, err := DrainQueuedAtBoundary("worker", "wK", box)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || !second[0].Acknowledged {
		t.Fatalf("replay after crash-before-ack = %+v", second)
	}
	third, err := DrainQueuedAtBoundary("worker", "wK", box)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("post-ack drain duplicated processing: %+v", third)
	}
}

func TestBusySendCancelDoesNotClaimConsumption(t *testing.T) {
	_, box := installBusyWorker(t, "working")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := queueRoutineLocked(ctx, "worker", "canceled")
	if err == nil {
		t.Fatal("canceled queue must fail")
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("canceled queue left mail: %+v", pending)
	}
}

func TestFormatSendResultNamesQueuedDurable(t *testing.T) {
	got := formatSendResult("worker", "", StatusQueuedDurable, "queued-abc")
	if !strings.Contains(got, StatusQueuedDurable) || !strings.Contains(got, "queued-abc") || !strings.Contains(got, "not consumed") {
		t.Fatalf("queued-durable receipt = %q", got)
	}
	if strings.Contains(got, "consumption confirmed") || strings.Contains(got, "UNVERIFIED") {
		t.Fatalf("queued-durable must not claim consumption: %q", got)
	}
}

func TestQueueMailboxFailsClosedWithoutProjectRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HERD_ROOT", t.TempDir())
	t.Setenv("HERD_PROJECT_ROOT", "")
	t.Setenv("HERD_MAIL_FILE", "")
	_, err := resolveQueueMailbox()
	if err == nil {
		t.Fatal("unavailable project root must fail closed, not guess cwd or HERD_ROOT")
	}
}

func TestQueueMailboxIgnoresLaneHERDROOT(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "fac773@test"},
		{"config", "user.name", "fac773"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	lane := t.TempDir()
	t.Chdir(repo)
	t.Setenv("HERD_ROOT", lane)
	t.Setenv("HERD_PROJECT_ROOT", "")
	t.Setenv("HERD_MAIL_FILE", "")
	box, err := resolveQueueMailbox()
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := gitroot.ProjectRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".herd", "control-mail.jsonl")
	if box.MailFile != want {
		t.Fatalf("mailbox = %q, want project root %q (not HERD_ROOT %q)", box.MailFile, want, lane)
	}
}
