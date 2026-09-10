package attention

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/process"
)

func TestAttention_RunWithFleet_OpencodeExportFinishLength(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"
	worktreeDir := filepath.Clean("/path/to/worktree")

	// Construct realistic >64KiB export with finish=length
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"info":{"id":%q,"directory":%q},"messages":[`, sessionID, worktreeDir))
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

	kick.SetStandingOverride([]string{"forge-ux-comber"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "ux-comber", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(context.Context, string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: "ux-comber", Task: "CHA-2000", Scope: "task"},
		}, nil
	}

	fleet := []kick.AgentEntry{
		{
			Name:           "forge-ux-comber",
			Label:          "ux-comber",
			Kind:           "opencode",
			Status:         "done", // Agent status claimed "done", but native export has finish=length
			PaneID:         "p-ux",
			TabID:          "t-ux",
			TerminalID:     "term-ux",
			Workspace:      "ws-1",
			Cwd:            worktreeDir,
			StateChangeSeq: 5,
			Session:        kick.AgentSession{Value: sessionID},
		},
	}

	result, err := runWithFleet(func() ([]kick.AgentEntry, error) {
		return fleet, nil
	}, callPathReader{}, "repo", resolver, registry)
	if err != nil {
		t.Fatalf("runWithFleet: %v", err)
	}

	if len(result.Items) != 1 {
		t.Fatalf("expected 1 triage item needing eyes, got %d (%+v)", len(result.Items), result.Items)
	}

	item := result.Items[0]
	if item.Level != LevelMedium {
		t.Errorf("agent with finish=length must be LevelMedium (incomplete), got level %s", item.Level)
	}
	if !strings.Contains(item.Reason, "finish=length") {
		t.Errorf("reason should mention finish=length, got %q", item.Reason)
	}
}

func TestAttention_RunWithFleet_IdentityFenceRejection(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"
	worktreeDir := filepath.Clean("/path/to/worktree")

	// Export payload is normal stop/complete
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"info":{"id":%q,"directory":%q},"messages":[`, sessionID, worktreeDir))
	sb.WriteString(fmt.Sprintf(`{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},`, sessionID, now.Add(-2*time.Minute).UnixMilli()))
	sb.WriteString(fmt.Sprintf(`{"info":{"id":"a-final","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":%q,"finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}`,
		sessionID, model, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli()))
	sb.WriteString(`]}`)
	payload := []byte(sb.String())

	restore := process.SetDefaultExportRunner(func(ctx context.Context, sid string, dir string) ([]byte, error) {
		return payload, nil
	})
	defer restore()

	kick.SetStandingOverride([]string{"forge-ux-comber"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "ux-comber", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(context.Context, string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: "ux-comber", Task: "CHA-2000", Scope: "task"},
		}, nil
	}

	callCount := 0
	fleetFunc := func() ([]kick.AgentEntry, error) {
		callCount++
		entry := kick.AgentEntry{
			Name:           "forge-ux-comber",
			Label:          "ux-comber",
			Kind:           "opencode",
			Status:         "done",
			PaneID:         "p-ux",
			TabID:          "t-ux",
			TerminalID:     "term-ux",
			Workspace:      "ws-1",
			Cwd:            worktreeDir,
			StateChangeSeq: 5,
			Session:        kick.AgentSession{Value: sessionID},
		}
		if callCount > 1 {
			// Second call (after export) simulates mutated/reused agent state
			entry.StateChangeSeq = 6
		}
		return []kick.AgentEntry{entry}, nil
	}

	result, err := runWithFleet(fleetFunc, callPathReader{}, "repo", resolver, registry)
	if err != nil {
		t.Fatalf("runWithFleet: %v", err)
	}

	if len(result.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(result.Items))
	}

	// Since identity fence failed, native evidence returned an error and lane is stamped LevelMedium (UNKNOWN/error)
	item := result.Items[0]
	if item.Level != LevelMedium {
		t.Errorf("expected LevelMedium on rejected fence error, got %s", item.Level)
	}
	if !strings.Contains(item.Reason, "native evidence error") {
		t.Errorf("expected reason to contain 'native evidence error', got %q", item.Reason)
	}
}

func TestAttention_RunWithFleet_MultipleHangingLanes_BoundedAggregateContext(t *testing.T) {
	restore := process.SetDefaultExportRunner(func(ctx context.Context, sid string, dir string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer restore()

	kick.SetStandingOverride([]string{"forge-lane-1", "forge-lane-2", "forge-lane-3"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "lane-1", Role: "worker", Standing: true},
		{Name: "lane-2", Role: "worker", Standing: true},
		{Name: "lane-3", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(_ context.Context, laneName string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: laneName, Task: "CHA-1", Scope: "task"},
		}, nil
	}

	fleet := []kick.AgentEntry{
		{
			Name:       "forge-lane-1",
			Label:      "lane-1",
			Kind:       "opencode",
			Status:     "working",
			PaneID:     "p-1",
			Session:    kick.AgentSession{Value: "s-1"},
			TerminalID: "term-1",
		},
		{
			Name:       "forge-lane-2",
			Label:      "lane-2",
			Kind:       "opencode",
			Status:     "working",
			PaneID:     "p-2",
			Session:    kick.AgentSession{Value: "s-2"},
			TerminalID: "term-2",
		},
		{
			Name:       "forge-lane-3",
			Label:      "lane-3",
			Kind:       "opencode",
			Status:     "working",
			PaneID:     "p-3",
			Session:    kick.AgentSession{Value: "s-3"},
			TerminalID: "term-3",
		},
	}

	start := time.Now()
	result, err := runWithFleet(func() ([]kick.AgentEntry, error) {
		return fleet, nil
	}, callPathReader{}, "repo", resolver, registry)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("runWithFleet: %v", err)
	}

	// Should finish within fleet timeout (~15s ceiling), not 3 * 10s = 30s
	if elapsed > 20*time.Second {
		t.Errorf("fleet scan with multiple hanging lanes took too long: %v (expected <= 20s aggregate)", elapsed)
	}

	for _, item := range result.Items {
		if item.Level != LevelMedium {
			t.Errorf("hanging lane must be stamped LevelMedium on export error, got %s for %s", item.Level, item.Name)
		}
		if !strings.Contains(item.Reason, "native evidence error") {
			t.Errorf("expected reason to contain 'native evidence error', got %q", item.Reason)
		}
	}
}
