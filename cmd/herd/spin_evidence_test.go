package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/kick"
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
			Name:    "forge-worker",
			Kind:    "opencode",
			PaneID:  "p-1",
			Cwd:     "/path/to/worktree",
			Session: kick.AgentSession{Value: sessionID},
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
