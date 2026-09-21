package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/beat"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/coordinator"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/outbox"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/textdelivery"
)

func TestIntegrationWakeExactReadinessProducesExecutablePrompt(t *testing.T) {
	item := attention.CandidateItem{SHA: strings.Repeat("a", 40), Branch: "fix/fac599", Task: "FAC-599", PullRequest: 123, Status: "ready-but-open", ReviewReady: true}
	owner := &coordinator.Registration{Name: "coordinator", PaneID: "wK:p1", TerminalID: "term1"}
	actions, err := integrationActions([]attention.CandidateItem{item}, owner)
	if err != nil || len(actions) != 1 {
		t.Fatalf("no exact action: %v %+v", err, actions)
	}
	if !strings.Contains(actions[0].Action, item.SHA) || !strings.Contains(actions[0].Action, "#123") || !strings.Contains(actions[0].Action, "admission") {
		t.Fatal("wake action is not executable", actions[0])
	}
	var delivered []beat.IntegrationWake
	path := filepath.Join(t.TempDir(), "wakes.json")
	_, err = beat.ReconcileIntegrationWakes(context.Background(), path, actions, time.Now(), time.Minute, func(_ context.Context, w beat.IntegrationWake) error { delivered = append(delivered, w); return nil })
	if err != nil || len(delivered) != 1 || delivered[0].Owner != owner.Name || delivered[0].Target != owner.PaneID || delivered[0].Session != owner.TerminalID {
		t.Fatalf("readiness did not reach exact owner delivery: %v %+v", err, delivered)
	}
	for _, status := range []string{"ready-but-ci-pending", "ready-but-ci-failed", "ready-without-pr", "ready-callback-blocked", "ready-landing-recorded"} {
		item.Status = status
		a, err := integrationActions([]attention.CandidateItem{item}, owner)
		if err != nil || len(a) != 0 {
			t.Fatalf("%s emitted integration wake", status)
		}
	}
	item.Status = "ready-evidence-unknown"
	if _, err := integrationActions([]attention.CandidateItem{item}, owner); err == nil {
		t.Fatal("unknown evidence mistaken for empty readiness")
	}
	item.Status = "ready-but-open"
	item.ReviewReady = false
	if _, err := integrationActions([]attention.CandidateItem{item}, owner); err == nil {
		t.Fatal("unready review emitted wake")
	}
}

func TestIntegrationWakeOwnerRequiresExactLiveIncarnation(t *testing.T) {
	root := t.TempDir()
	if _, err := coordinator.Register(root, "coordinator", "wK"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.BindTab(root, "wK", "wK:t1", "wK:p1", "term1"); err != nil {
		t.Fatal(err)
	}
	ready := true
	a := herdr.AgentEntry{Kind: "codex", Status: "idle", Workspace: "wK", TabID: "wK:t1", PaneID: "wK:p1", TerminalID: "term1", InteractiveReady: &ready}
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "herdr" || strings.Join(args, " ") != "agent list" {
			t.Fatal("unexpected route command", name, args)
		}
		return json.Marshal(map[string]any{"result": map[string]any{"agents": []herdr.AgentEntry{a}}})
	}
	if _, err := liveIntegrationOwner(context.Background(), root, run, true); err != nil {
		t.Fatal(err)
	}
	a.TerminalID = "reused"
	if _, err := liveIntegrationOwner(context.Background(), root, run, true); err == nil {
		t.Fatal("reused pane authorized")
	}
	a.TerminalID = "term1"
	a.Status = "working"
	if _, err := liveIntegrationOwner(context.Background(), root, run, false); err != nil {
		t.Fatal("busy owner cannot retain a wake", err)
	}
	if _, err := liveIntegrationOwner(context.Background(), root, run, true); err == nil {
		t.Fatal("busy coordinator was interrupted")
	}
	if _, err := liveIntegrationOwner(context.Background(), t.TempDir(), run, true); err == nil {
		t.Fatal("unregistered default owner authorized")
	}
	bad := func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"error":{"message":"offline"}}`), nil
	}
	if _, err := liveIntegrationOwner(context.Background(), root, bad, true); err == nil {
		t.Fatal("error envelope authorized owner")
	}
	failed := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("timeout") }
	if _, err := liveIntegrationOwner(context.Background(), root, failed, true); err == nil {
		t.Fatal("failed roster authorized owner")
	}
}

func TestIntegrationWakeProductionForgeComposition(t *testing.T) {
	// Keep the real root composition wired to the tested event callback. The
	// behavioral tick test separately proves execution and error propagation.
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(body), "func forgeLoopMain(")
	if start < 0 {
		t.Fatal("missing production loop")
	}
	source := string(body)[start:]
	if !strings.Contains(source, "coordinator.Register(forgeControlRoot,") || !strings.Contains(source, "bindCoordinatorControlTab(forgeControlRoot,") || !strings.Contains(source, "forgeControlRoot, rootErr := canonicalHerdRoot()") {
		t.Fatal("forge registration and binding do not share canonical wake authority")
	}
	if !strings.Contains(source, "IntegrationWakes:") || !strings.Contains(source, "return runForgeIntegrationWakes(ctx, cfg, tp)") {
		t.Fatal("production forge does not drive integration wakes")
	}
	wakeSrc, err := os.ReadFile("integration_wake.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wakeSrc), "return reconcileReadyIntegrationWakes(ctx, root, cfg, tp, attentionCommandAt(root), time.Now().UTC(), integrationWakeAge, deliverIntegrationWake)") {
		t.Fatal("forge tick does not drive the ready-but-open wake path")
	}
}

func TestIntegrationWakeAckCommandUsesExactGeneration(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(root)
	sha := strings.Repeat("c", 40)
	action := beat.IntegrationAction{CandidateSHA: sha, PullRequest: 12, Task: "FAC-599", Owner: "coordinator", Target: "wK:p1", Session: "term1", Action: "Evaluate exact candidate " + sha + " PR 12 admission"}
	_, err := beat.ReconcileIntegrationWakes(context.Background(), integrationWakePath(root), []beat.IntegrationAction{action}, time.Now(), time.Minute, func(context.Context, beat.IntegrationWake) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := runIntegrationWakeAck([]string{"--candidate", sha, "--generation", "2"}); err == nil {
		t.Fatal("wrong-generation CLI ack accepted")
	}
	if err := runIntegrationWakeAck([]string{"--candidate", sha, "--generation", "1"}); err != nil {
		t.Fatal(err)
	}
	pending, err := beat.ReconcileIntegrationWakes(context.Background(), integrationWakePath(root), []beat.IntegrationAction{action}, time.Now(), time.Minute, func(context.Context, beat.IntegrationWake) error {
		t.Fatal("acknowledged wake redelivered")
		return nil
	})
	if err != nil || len(pending) != 0 {
		t.Fatalf("CLI ack not retained: %v %+v", err, pending)
	}
}

func TestIntegrationWakeGenerationsUseDistinctDurableIntents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0700); err != nil {
		t.Fatal(err)
	}
	store, err := outbox.NewStore(herdr.OperatorDeliveryStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sha := strings.Repeat("d", 40)
	wake := beat.IntegrationWake{IntegrationAction: beat.IntegrationAction{CandidateSHA: sha, PullRequest: 12, Task: "FAC-599", Owner: "coordinator", Target: "wK:p1", Session: "term1", Action: "Evaluate exact candidate " + sha + " PR 12"}, Generation: 1, CreatedAt: time.Now().UTC()}
	calls := 0
	executor := textdelivery.ExecutorFunc(func(_ context.Context, c textdelivery.Command) ([]byte, error) { calls++; return c.Payload, nil })
	deliver := func(w beat.IntegrationWake) {
		t.Helper()
		request, err := integrationDeliveryRequest(root, w)
		if err != nil {
			t.Fatal(err)
		}
		if request.StatePath != herdr.OperatorDeliveryStatePath(root) {
			t.Fatal("wrong durable receipt authority")
		}
		ledger := textdelivery.NewDurableLedger(store, request.Generation)
		if _, err := ledger.Deliver(context.Background(), request.Key, "fake", []string{request.Target, request.Session}, request.Payload, executor); err != nil {
			t.Fatal(err)
		}
	}
	deliver(wake)
	deliver(wake)
	if calls != 1 {
		t.Fatal("restart resent completed intent", calls)
	}
	wake.Generation = 2
	wake.Escalated = true
	deliver(wake)
	deliver(wake)
	if calls != 2 {
		t.Fatal("escalation lost or duplicated", calls)
	}
	wake.Generation = 3
	wake.Session = "term2"
	deliver(wake)
	if calls != 3 {
		t.Fatal("new owner incarnation lost", calls)
	}
}

func TestIntegrationWakeNativeCLIDisposableEmit(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	sha := strings.Repeat("e", 40)
	owner := "forge-orchestrator-39a9827d2b"
	action := "merge SHA " + sha + " of PR 77"
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(buildHerd(t), append([]string{"integration-wake"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode()
		}
		t.Fatalf("herd integration-wake %v: %v\n%s", args, err, out)
		return "", -1
	}
	emit := func(at time.Time, next string) (string, int) {
		t.Helper()
		return run("--emit", "--candidate", sha, "--pr", "77", "--task", "FAC-599", "--owner", owner, "--target", "wK:pZ6", "--session", "term-1", "--action", next, "--now", at.Format(time.RFC3339), "--max-age", "1m", "--json")
	}
	emptyOut, emptyCode := emit(now, "")
	if emptyCode == 0 {
		t.Fatalf("empty action was accepted:\n%s", emptyOut)
	}
	if _, err := os.Stat(integrationWakePath(root)); !os.IsNotExist(err) {
		t.Fatal("empty action wrote wake state")
	}
	namelessOut, namelessCode := emit(now, "review something")
	if namelessCode == 0 {
		t.Fatalf("action without SHA/PR was accepted:\n%s", namelessOut)
	}
	out, code := emit(now, action)
	if code != 0 {
		t.Fatalf("emit failed: %s", out)
	}
	var report struct {
		Wake   beat.IntegrationWake `json:"wake"`
		Merged bool                 `json:"merged"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("emit json: %v\n%s", err, out)
	}
	if report.Merged || report.Wake.Owner != owner || report.Wake.Action != action || report.Wake.Generation != 1 || report.Wake.PullRequest != 77 {
		t.Fatalf("wake is not executable for the exact owner: %+v", report)
	}
	if !strings.Contains(report.Wake.Action, sha) || !strings.Contains(report.Wake.Action, "77") {
		t.Fatal("wake action omitted exact SHA or PR")
	}
	again, againCode := emit(now.Add(time.Second), action)
	if againCode != 0 {
		t.Fatalf("coalesce emit failed: %s", again)
	}
	if err := json.Unmarshal([]byte(again), &report); err != nil {
		t.Fatal(err)
	}
	if report.Wake.Generation != 1 || report.Merged {
		t.Fatalf("stale identical wake was not coalesced: %+v", report)
	}
	escalated, escCode := emit(now.Add(time.Minute), action)
	if escCode != 0 {
		t.Fatalf("escalation emit failed: %s", escalated)
	}
	if err := json.Unmarshal([]byte(escalated), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Wake.Escalated || report.Wake.Generation != 2 || report.Merged {
		t.Fatalf("unconsumed wake did not escalate: %+v", report)
	}
	ackOut, ackCode := run("--candidate", sha, "--generation", "2")
	if ackCode != 0 || !strings.Contains(ackOut, "no merge") {
		t.Fatalf("ack failed: %d %s", ackCode, ackOut)
	}
	redeliver, redeliverCode := emit(now.Add(2*time.Minute), action)
	if redeliverCode != 0 {
		t.Fatalf("acked emit failed: %s", redeliver)
	}
	if err := json.Unmarshal([]byte(redeliver), &report); err != nil {
		t.Fatal(err)
	}
	if report.Wake.Generation != 2 || report.Merged {
		t.Fatalf("acknowledged wake changed identity: %+v", report)
	}
}

type readyWakeTaskReader struct {
	provider.TaskProvider
	task *provider.Task
}

func (p *readyWakeTaskReader) GetTask(ctx context.Context, ref string) (*provider.Task, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("task read has no deadline")
	}
	if p.task == nil || ref != p.task.Ref {
		return nil, errors.New("wrong task ref")
	}
	return p.task, nil
}

type readyWakeProcess struct {
	args []string
	out  []byte
}

func (p *readyWakeProcess) SetStdin(io.Reader) {}
func (p *readyWakeProcess) Output() ([]byte, error) {
	return p.out, nil
}

type readyWakeStarter struct {
	last  *readyWakeProcess
	calls atomic.Int32
}

func (s *readyWakeStarter) Start(_ context.Context, _ string, args ...string) textdelivery.Process {
	s.calls.Add(1)
	p := &readyWakeProcess{args: append([]string(nil), args...), out: []byte("ok")}
	s.last = p
	return p
}

func TestProductionReadyTransitionDeliversCoalescesAndEscalates(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := t.TempDir()
	ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	t.Setenv("HERD_MAIL_FILE", filepath.Join(root, ".herd", "mail.jsonl"))
	t.Setenv(herdr.NoLiveEnv, "1")
	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"event":"record","sha":"` + sha + `","task":"FAC-599","branch":"herd/fac-599","reviewer":"reviewer","builder_family":"openai","reviewer_family":"anthropic","gate":"independent"}` + "\n" +
		`{"event":"verdict","sha":"` + sha + `","reviewer":"reviewer","builder_family":"openai","reviewer_family":"anthropic","verdict":"PASS"}` + "\n"
	if err := os.WriteFile(ledgerPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger.QueuePath, []byte(`{"event":"enqueue","sha":"`+sha+`","branch":"herd/fac-599"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Register(root, "coordinator", "wK"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.BindTab(root, "wK", "wK:t1", "wK:p1", "term1"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.TaskProvider.ProjectID = "project"
	tp := &readyWakeTaskReader{task: &provider.Task{Ref: "FAC-599", ProjectID: "project", Status: "in-review"}}
	ready := true
	roster, err := json.Marshal(map[string]any{"result": map[string]any{"agents": []herdr.AgentEntry{{
		Kind: "codex", Status: "idle", Workspace: "wK", TabID: "wK:t1", PaneID: "wK:p1", TerminalID: "term1", InteractiveReady: &ready,
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	prJSON := `[{"number":42,"headRefOid":"` + sha + `","state":"OPEN","url":"https://example.test/pr/42","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"Build, Preflight & Test Suite","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://example.test/check/1"}]}]`
	pendingJSON := `[{"number":42,"headRefOid":"` + sha + `","state":"OPEN","url":"https://example.test/pr/42","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"Build, Preflight & Test Suite","status":"PENDING","conclusion":"","detailsUrl":"https://example.test/check/1"}]}]`
	prBody := pendingJSON
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "git":
			return []byte(sha), nil
		case "gh":
			return []byte(prBody), nil
		case "herdr":
			if strings.Join(args, " ") != "agent list" {
				t.Fatalf("production path issued %s %v", name, args)
			}
			return roster, nil
		default:
			t.Fatalf("production path issued %s %v", name, args)
			return nil, nil
		}
	}
	var n int32
	restore := herdr.InstallOperatorStatusProbe(func(target string) (string, error) {
		if target != "wK:p1" {
			t.Fatalf("delivery target=%q", target)
		}
		if atomic.AddInt32(&n, 1)%2 == 1 {
			return "idle", nil
		}
		return "working", nil
	})
	t.Cleanup(restore)
	starter := &readyWakeStarter{}
	deliver := func(ctx context.Context, labRoot string, w beat.IntegrationWake) error {
		return deliverReadyIntegrationWake(ctx, labRoot, w, run, func(ctx context.Context, d herdr.OperatorDelivery) (herdr.DeliveryProof, error) {
			return herdr.DeliverOperatorWithExecutor(ctx, d, textdelivery.NewDirectExecutor(starter.Start))
		})
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	tick := func(at time.Time) {
		t.Helper()
		if err := reconcileReadyIntegrationWakes(context.Background(), root, cfg, tp, run, at, time.Minute, deliver); err != nil {
			t.Fatal(err)
		}
	}

	tick(now)
	if starter.calls.Load() != 0 {
		t.Fatalf("ci-pending candidate produced a wake: calls=%d", starter.calls.Load())
	}

	prBody = prJSON
	tick(now)
	if starter.calls.Load() != 1 || starter.last == nil {
		t.Fatalf("ready-but-open did not deliver: calls=%d", starter.calls.Load())
	}
	payload := starter.last.args[3]
	if !strings.Contains(payload, sha) || !strings.Contains(payload, "#42") || !strings.Contains(payload, `"owner":"coordinator"`) || !strings.Contains(payload, "integration-ready") {
		t.Fatalf("wake payload missing exact action/owner: %s", payload)
	}
	if strings.Contains(payload, "board-done") || strings.Contains(strings.ToLower(payload), "auto-merge") {
		t.Fatal("wake granted merge or board authority")
	}

	tick(now.Add(time.Second))
	if starter.calls.Load() != 1 {
		t.Fatalf("unchanged ready observation redelivered: calls=%d", starter.calls.Load())
	}

	tick(now.Add(time.Minute))
	if starter.calls.Load() != 2 {
		t.Fatalf("unconsumed wake did not escalate: calls=%d", starter.calls.Load())
	}
	if !strings.Contains(starter.last.args[3], `"escalated":true`) || !strings.Contains(starter.last.args[3], `"generation":2`) {
		t.Fatalf("escalation payload missing identity: %s", starter.last.args[3])
	}
}
