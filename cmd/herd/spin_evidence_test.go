package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		Name:           "forge-ux-comber",
		Kind:           "opencode",
		SessionID:      sessionID,
		PaneID:         "pane-1",
		TabID:          "tab-1",
		TerminalID:     "term-1",
		Workspace:      "ws-1",
		Cwd:            worktreeDir,
		StateChangeSeq: 10,
	}

	fetchAfter := func(name string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:           "forge-ux-comber",
			Kind:           "opencode",
			PaneID:         "pane-1",
			TabID:          "tab-1",
			TerminalID:     "term-1",
			Workspace:      "ws-1",
			Cwd:            worktreeDir,
			StateChangeSeq: 10,
			Session:        kick.AgentSession{Value: sessionID},
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
