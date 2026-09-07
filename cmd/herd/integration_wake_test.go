package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/beat"
	"github.com/Kampe/Herdforge/pkg/coordinator"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/outbox"
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
}

func TestIntegrationWakeAckCommandUsesExactGeneration(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(root)
	sha := strings.Repeat("c", 40)
	action := beat.IntegrationAction{CandidateSHA: sha, PullRequest: 12, Task: "FAC-599", Owner: "coordinator", Target: "wK:p1", Session: "term1", Action: "Evaluate exact PR 12 admission"}
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
	wake := beat.IntegrationWake{IntegrationAction: beat.IntegrationAction{CandidateSHA: strings.Repeat("d", 40), PullRequest: 12, Task: "FAC-599", Owner: "coordinator", Target: "wK:p1", Session: "term1", Action: "Evaluate exact PR 12"}, Generation: 1, CreatedAt: time.Now().UTC()}
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
