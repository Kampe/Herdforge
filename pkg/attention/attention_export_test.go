package attention

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/process"
)

func TestAttention_RunWithFleet_OpencodeExportFinishLength(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"

	// Construct realistic >64KiB export with finish=length
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[`, sessionID))
	sb.WriteString(fmt.Sprintf(`{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},`, sessionID, now.Add(-2*time.Minute).UnixMilli()))
	for i := 0; i < 400; i++ {
		sb.WriteString(fmt.Sprintf(`{"info":{"id":"m-%d","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":%q,"finish":"tool-calls","time":{"created":%d,"completed":%d}},"parts":[{"type":"tool"},{"type":"text","content":%q}]},`,
			i, sessionID, model, now.Add(-1*time.Minute).UnixMilli(), now.Add(-50*time.Second).UnixMilli(), strings.Repeat("a", 160)))
	}
	sb.WriteString(fmt.Sprintf(`{"info":{"id":"a-final","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":%q,"finish":"length","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}`,
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

	agents := []kick.AgentEntry{
		{
			Name:    "forge-ux-comber",
			Label:   "ux-comber",
			Kind:    "opencode",
			Status:  "done", // Agent status claimed "done", but export has finish=length
			PaneID:  "p-ux",
			Session: kick.AgentSession{Value: sessionID},
		},
	}

	check := func(name string) (string, bool) { return "", false }
	resolver := func(name string) (*process.TerminalEvidence, process.SessionContext, string) {
		a, found := findAttentionAgent(agents, name)
		if !found {
			return nil, process.SessionContext{}, ""
		}
		ev, sctx, paneText, _ := process.ResolveNativeAgentEvidence(context.Background(), a.Session.Value, a.Kind, a.Cwd, now, 5*time.Minute)
		return ev, sctx, paneText
	}

	result := TriageWithEvidence(agents, []string{"ux-comber"}, check, resolver, nil)

	if len(result.Items) != 1 {
		t.Fatalf("expected 1 triage item needing eyes, got %d", len(result.Items))
	}

	item := result.Items[0]
	if item.Level != LevelMedium {
		t.Errorf("agent with finish=length must be LevelMedium (incomplete), got level %s", item.Level)
	}
	if !strings.Contains(item.Reason, "finish=length") {
		t.Errorf("reason should mention finish=length, got %q", item.Reason)
	}
}
