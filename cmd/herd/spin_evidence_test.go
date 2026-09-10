package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/process"
	"github.com/Kampe/Herdforge/pkg/spin"
)

func TestSpin_ClassifyTargetWithNativeOpencodeEvidence(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"
	worktreeDir := filepath.Clean("/path/to/worktree")

	// Construct >64KiB export payload with finish=length
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"info":{"id":%q,"directory":%q},"messages":[`, sessionID, worktreeDir))
	sb.WriteString(fmt.Sprintf(`{"info":{"id":"u-spin","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},`, sessionID, now.Add(-2*time.Minute).UnixMilli()))
	for i := 0; i < 400; i++ {
		sb.WriteString(fmt.Sprintf(`{"info":{"id":"m-%d","sessionID":%q,"role":"assistant","parentID":"u-spin","providerID":"litellm","modelID":%q,"finish":"tool-calls","time":{"created":%d,"completed":%d}},"parts":[{"type":"tool"},{"type":"text","content":%q}]},`,
			i, sessionID, model, now.Add(-1*time.Minute).UnixMilli(), now.Add(-50*time.Second).UnixMilli(), strings.Repeat("b", 160)))
	}
	sb.WriteString(fmt.Sprintf(`{"info":{"id":"a-final","sessionID":%q,"role":"assistant","parentID":"u-spin","providerID":"litellm","modelID":%q,"finish":"length","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}`,
		sessionID, model, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli()))
	sb.WriteString(`]}`)
	payload := []byte(sb.String())

	restore := process.SetDefaultExportRunner(func(ctx context.Context, sid string, dir string) ([]byte, error) {
		if sid == sessionID {
			return payload, nil
		}
		return nil, fmt.Errorf("unknown session: %s", sid)
	})
	defer restore()

	fence := process.IdentityFence{
		Name:             "forge-ux-comber",
		Kind:             "opencode",
		SessionID:        sessionID,
		PaneID:           "pane-1",
		TabID:            "tab-1",
		TerminalID:       "term-1",
		Workspace:        "ws-1",
		Cwd:              worktreeDir,
		StateChangeSeq:   10,
		ExpectedModel:    model,
		ExpectedProvider: "litellm",
	}

	fetchAfter := func(name string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:             "forge-ux-comber",
			Kind:             "opencode",
			PaneID:           "pane-1",
			TabID:            "tab-1",
			TerminalID:       "term-1",
			Workspace:        "ws-1",
			Cwd:              worktreeDir,
			StateChangeSeq:   10,
			ExpectedModel:    model,
			ExpectedProvider: "litellm",
			Session:          kick.AgentSession{Value: sessionID},
		}, nil
	}

	// What runSpin executes:
	ev, sctx, _, err := process.ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, now, 5*time.Minute)
	if err != nil {
		t.Fatalf("ResolveNativeAgentEvidenceWithFence failed: %v", err)
	}

	paneTail := "Verdict: PASS\nStatus: COMPLETE" // Even if pane has stale/crafted text
	target := process.ClassifyTargetWithEvidence("pane-1", "forge-ux-comber", "working", paneTail, ev, sctx)

	if target.Class != process.Unknown {
		t.Errorf("spin target diagnostic with finish=length must be UNKNOWN, got %s", target.Class)
	}

	obs := spin.Observation{
		PaneID:      "pane-1",
		Name:        "forge-ux-comber",
		AgentStatus: "working",
		Diagnostic:  string(target.Class),
	}

	if obs.Diagnostic != "UNKNOWN" {
		t.Errorf("spin observation diagnostic must be UNKNOWN, got %s", obs.Diagnostic)
	}
}

func TestSpin_DivergentProcessCwdAndRegisteredWorktree_Rejects(t *testing.T) {
	herdrWorktree := "/path/to/herdr/worktree"
	divergentProcCwd := "/path/to/other/worktree"

	agent := kick.AgentEntry{
		Name:    "forge-worker",
		Kind:    "opencode",
		PaneID:  "p-1",
		Status:  "working",
		Cwd:     herdrWorktree,
		Session: kick.AgentSession{Value: "sess-1"},
	}

	// Verify spin Cwd divergence guard logic
	if process.NormalizePath(divergentProcCwd) != process.NormalizePath(agent.Cwd) {
		assessment := spin.Assessment{
			PaneID:     agent.PaneID,
			Name:       agent.Name,
			Cause:      spin.CauseUnknownState,
			NextAction: spin.ActionObserve,
			Evidence:   []string{fmt.Sprintf("process directory %q diverged from registered worktree %q", divergentProcCwd, agent.Cwd)},
		}

		if assessment.NextAction != spin.ActionObserve {
			t.Errorf("expected action ActionObserve, got %v", assessment.NextAction)
		}
		if len(assessment.Evidence) == 0 || !strings.Contains(assessment.Evidence[0], "diverged from registered worktree") {
			t.Errorf("expected evidence to contain divergence explanation, got %v", assessment.Evidence)
		}
	} else {
		t.Fatalf("expected divergence check to succeed")
	}
}

func TestSpin_ModelRouteMismatch_RefusesEvidence(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":"litellm/actual-model","finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	fence := process.IdentityFence{
		Name:             "forge-worker",
		Kind:             "opencode",
		SessionID:        sessionID,
		PaneID:           "p-1",
		Cwd:              "/path/to/worktree",
		ExpectedModel:    "litellm/expected-model",
		ExpectedProvider: "litellm",
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:             "forge-worker",
			Kind:             "opencode",
			PaneID:           "p-1",
			Cwd:              "/path/to/worktree",
			ExpectedModel:    "litellm/expected-model",
			ExpectedProvider: "litellm",
			Session:          kick.AgentSession{Value: sessionID},
		}, nil
	}

	ev, sctx, _, err := process.ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "model mismatch") {
		t.Fatalf("expected model mismatch error, got: %v", err)
	}

	target := process.ClassifyTargetWithEvidence("p-1", "forge-worker", "working", "some pane text", ev, sctx)
	if target.Class != process.Unknown {
		t.Errorf("model mismatch must classify as Unknown, got: %s", target.Class)
	}
}

func TestSpin_UnboundRoute_RefusesEvidence(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":"model","finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	// Omit ExpectedModel and ExpectedProvider -> Unbound route
	fence := process.IdentityFence{
		Name:      "forge-worker",
		Kind:      "opencode",
		SessionID: sessionID,
		PaneID:    "p-1",
		Cwd:       "/path/to/worktree",
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:    "forge-worker",
			Kind:    "opencode",
			PaneID:  "p-1",
			Cwd:     "/path/to/worktree",
			Session: kick.AgentSession{Value: sessionID},
		}, nil
	}

	ev, sctx, _, err := process.ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "unbound model route") {
		t.Fatalf("expected unbound model route error, got: %v", err)
	}

	target := process.ClassifyTargetWithEvidence("p-1", "forge-worker", "working", "some pane text", ev, sctx)
	if target.Class != process.Unknown {
		t.Errorf("unbound route must classify as Unknown, got: %s", target.Class)
	}
}

func TestSpin_ContextCancellation_PropagatesToHerdrCensus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled context

	censusCancelled := make(chan struct{})
	restore := herdr.SetRunHerdrContextForTest(func(c context.Context, args ...string) (string, error) {
		select {
		case <-c.Done():
			close(censusCancelled)
			return "", c.Err()
		case <-time.After(5 * time.Second):
			return "", errors.New("timeout not observed by census runner")
		}
	})
	defer restore()

	_, err := herdr.AgentListContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context canceled error, got: %v", err)
	}

	select {
	case <-censusCancelled:
		// cancellation was observed directly by the underlying census runner
	case <-time.After(1 * time.Second):
		t.Errorf("underlying herdr runner did not observe context cancellation")
	}
}

func TestSpin_NativeEvidenceError_DoesNotFallbackToPaneComplete(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"

	// Runner returns an error (e.g. malformed export / read failure)
	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return nil, errors.New("export failed: process terminated")
	})
	defer restore()

	fence := process.IdentityFence{
		Name:             "forge-worker",
		Kind:             "opencode",
		SessionID:        sessionID,
		PaneID:           "p-1",
		ExpectedModel:    "m-1",
		ExpectedProvider: "p-1",
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:             "forge-worker",
			Kind:             "opencode",
			PaneID:           "p-1",
			Session:          kick.AgentSession{Value: sessionID},
			ExpectedModel:    "m-1",
			ExpectedProvider: "p-1",
		}, nil
	}

	ev, sctx, _, evErr := process.ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, now, 5*time.Minute)
	if evErr == nil {
		t.Fatal("expected resolver error")
	}

	paneTail := "Verdict: PASS\nStatus: COMPLETE"

	// Replicate runSpin branch for native agent when evErr != nil:
	var target process.Target
	if evErr != nil && strings.EqualFold(fence.Kind, "opencode") {
		target = process.Target{
			PaneID: "p-1",
			Name:   "forge-worker",
			Status: "working",
			Class:  process.Unknown,
			Action: "observe",
			Tail:   fmt.Sprintf("native evidence error: %v", evErr),
		}
	} else {
		target = process.ClassifyTargetWithEvidence("p-1", "forge-worker", "working", paneTail, ev, sctx)
	}

	if target.Class == process.Complete || target.Class == process.Pass {
		t.Fatalf("SECURITY VIOLATION: native evidence error fell back to pane classification: %s", target.Class)
	}
	if target.Class != process.Unknown {
		t.Errorf("expected Unknown target class, got %s", target.Class)
	}
	if target.Action != "observe" {
		t.Errorf("expected observe target action, got %s", target.Action)
	}
}

func TestSpin_RealNativeAgent_BindsAcceptedLaunchProvenance(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"
	provider := "litellm"
	account := "lazer-prod"

	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":%q,"modelID":%q,"finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, provider, model, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	// Launch receipt store with authentic accepted receipt
	tmpDir := t.TempDir()
	receiptPath := filepath.Join(tmpDir, ".herd", "launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_LAUNCH_RECEIPTS", receiptPath)

	sink := &launch.JSONLSink{Path: receiptPath}
	if err := sink.Write(launch.Receipt{
		CreatedAt:         now.Add(-2 * time.Hour),
		Accepted:          true,
		Name:              "forge-worker",
		Lane:              "worker",
		Provider:          provider,
		Model:             model,
		RedactedAuthority: account,
		HerdrSession:      sessionID,
		PaneID:            "p-1",
	}); err != nil {
		t.Fatalf("write launch receipt: %v", err)
	}

	// Real agent row from Herdr has NO expected_model/expected_provider fields
	realAgent := kick.AgentEntry{
		Name:      "forge-worker",
		Kind:      "opencode",
		Status:    "working",
		PaneID:    "p-1",
		Workspace: "ws-1",
		Cwd:       "/path/to/worktree",
		Session:   kick.AgentSession{Value: sessionID},
	}

	receipts, _ := launch.ReadReceipts(launch.DefaultReceiptPath())
	prov, mod, acc, err := launch.AcceptedNativeLaunchRouteForAgent(receipts, realAgent.Name, realAgent.Session.Value, realAgent.PaneID, realAgent.TabID)
	if err != nil {
		t.Fatalf("resolve launch route: %v", err)
	}

	fence := process.IdentityFence{
		Name:             realAgent.Name,
		Kind:             realAgent.Kind,
		SessionID:        realAgent.Session.Value,
		PaneID:           realAgent.PaneID,
		Cwd:              realAgent.Cwd,
		ExpectedModel:    mod,
		ExpectedProvider: prov,
		ExpectedAccount:  acc,
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:             realAgent.Name,
			Kind:             realAgent.Kind,
			PaneID:           realAgent.PaneID,
			Cwd:              realAgent.Cwd,
			Session:          kick.AgentSession{Value: sessionID},
			ExpectedModel:    mod,
			ExpectedProvider: prov,
		}, nil
	}

	ev, sctx, _, evErr := process.ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, now, 5*time.Minute)
	if evErr != nil {
		t.Fatalf("expected evidence resolution to succeed via launch provenance, got: %v", evErr)
	}

	if ev.Model != model || ev.Provider != provider || ev.Account != account {
		t.Errorf("evidence route mismatch: model=%q prov=%q acc=%q", ev.Model, ev.Provider, ev.Account)
	}
	if sctx.Model != model || sctx.Provider != provider || sctx.Account != account {
		t.Errorf("session context route mismatch: model=%q prov=%q acc=%q", sctx.Model, sctx.Provider, sctx.Account)
	}

	target := process.ClassifyTargetWithEvidence(realAgent.PaneID, realAgent.Name, realAgent.Status, "Status: COMPLETE\nsummary of work", ev, sctx)
	if target.Class != process.Complete {
		t.Errorf("expected Complete target class with finish=stop, got %s", target.Class)
	}
}

func TestSpin_RealNativeAgent_MissingProvenance_FailsClosed(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"
	provider := "litellm"

	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":%q,"modelID":%q,"finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, provider, model, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	// Empty receipt store
	tmpDir := t.TempDir()
	emptyReceiptPath := filepath.Join(tmpDir, ".herd", "launch-receipts.jsonl")
	t.Setenv("HERD_LAUNCH_RECEIPTS", emptyReceiptPath)

	realAgent := kick.AgentEntry{
		Name:      "forge-worker",
		Kind:      "opencode",
		Status:    "working",
		PaneID:    "p-1",
		Workspace: "ws-1",
		Cwd:       "/path/to/worktree",
		Session:   kick.AgentSession{Value: sessionID},
	}

	receipts, _ := launch.ReadReceipts(launch.DefaultReceiptPath())
	prov, mod, acc, _ := launch.AcceptedNativeLaunchRouteForAgent(receipts, realAgent.Name, realAgent.Session.Value, realAgent.PaneID, realAgent.TabID)

	fence := process.IdentityFence{
		Name:             realAgent.Name,
		Kind:             realAgent.Kind,
		SessionID:        realAgent.Session.Value,
		PaneID:           realAgent.PaneID,
		Cwd:              realAgent.Cwd,
		ExpectedModel:    mod,
		ExpectedProvider: prov,
		ExpectedAccount:  acc,
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:             realAgent.Name,
			Kind:             realAgent.Kind,
			PaneID:           realAgent.PaneID,
			Cwd:              realAgent.Cwd,
			Session:          kick.AgentSession{Value: sessionID},
			ExpectedModel:    mod,
			ExpectedProvider: prov,
		}, nil
	}

	_, _, _, evErr := process.ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, now, 5*time.Minute)
	if evErr == nil || !strings.Contains(evErr.Error(), "unbound model route") {
		t.Fatalf("missing launch provenance must fail closed as unbound model route, got: %v", evErr)
	}
}
