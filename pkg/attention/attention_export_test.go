package attention

import (
	"context"
	"encoding/json"
	"errors"
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
			Name:             "forge-ux-comber",
			Label:            "ux-comber",
			Kind:             "opencode",
			Status:           "done", // Agent status claimed "done", but native export has finish=length
			PaneID:           "p-ux",
			TabID:            "t-ux",
			TerminalID:       "term-ux",
			Workspace:        "ws-1",
			Cwd:              worktreeDir,
			StateChangeSeq:   5,
			ExpectedModel:    model,
			ExpectedProvider: "litellm",
			Session:          kick.AgentSession{Value: sessionID},
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

func TestAttention_RunWithFleet_BlockingInitialCensus_ReturnsAtDeadline(t *testing.T) {
	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "lane-1", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(_ context.Context, laneName string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: laneName, Task: "CHA-1", Scope: "task"},
		}, nil
	}

	// Inject blocking initial census
	blockingCensus := func() ([]kick.AgentEntry, error) {
		time.Sleep(30 * time.Second)
		return nil, nil
	}

	start := time.Now()
	_, err = runWithFleet(blockingCensus, callPathReader{}, "repo", resolver, registry)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected fleet census timed out error, got: %v", err)
	}

	// Must unblock at aggregate deadline (~15s), not wait 30s
	if elapsed > 18*time.Second {
		t.Errorf("runWithFleet with blocking initial census took too long: %v (expected <= 18s)", elapsed)
	}
}

func TestAttention_RunWithFleet_BlockingAfterCensus_ReturnsAtDeadline(t *testing.T) {
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	now := time.Now().UTC()
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":"model","finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	kick.SetStandingOverride([]string{"forge-lane-1"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "lane-1", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(_ context.Context, laneName string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: laneName, Task: "CHA-1", Scope: "task"},
		}, nil
	}

	callCount := 0
	census := func() ([]kick.AgentEntry, error) {
		callCount++
		if callCount == 1 {
			return []kick.AgentEntry{
				{
					Name:             "forge-lane-1",
					Label:            "lane-1",
					Kind:             "opencode",
					Status:           "working",
					PaneID:           "p-1",
					Session:          kick.AgentSession{Value: sessionID},
					TerminalID:       "term-1",
					ExpectedModel:    "model",
					ExpectedProvider: "litellm",
				},
			}, nil
		}
		// Subsequent after-census call hangs
		time.Sleep(30 * time.Second)
		return nil, nil
	}

	start := time.Now()
	result, err := runWithFleet(census, callPathReader{}, "repo", resolver, registry)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("runWithFleet: %v", err)
	}

	if elapsed > 18*time.Second {
		t.Errorf("runWithFleet with blocking after-census took too long: %v (expected <= 18s)", elapsed)
	}

	for _, item := range result.Items {
		if item.Level != LevelMedium {
			t.Errorf("lane with hanging after-census must be LevelMedium, got %s", item.Level)
		}
		if !strings.Contains(item.Reason, "native evidence error") {
			t.Errorf("expected reason to contain 'native evidence error', got %q", item.Reason)
		}
	}
}

func TestAttention_RunWithFleet_ModelRouteMismatch_RefusesEvidence(t *testing.T) {
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	now := time.Now().UTC()
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":"litellm/actual-model","finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	kick.SetStandingOverride([]string{"forge-lane-1"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "lane-1", Role: "worker", Standing: true},
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
			Name:             "forge-lane-1",
			Label:            "lane-1",
			Kind:             "opencode",
			Status:           "working",
			PaneID:           "p-1",
			Session:          kick.AgentSession{Value: sessionID},
			TerminalID:       "term-1",
			ExpectedModel:    "litellm/expected-model",
			ExpectedProvider: "litellm",
		},
	}

	result, err := runWithFleet(func() ([]kick.AgentEntry, error) {
		return fleet, nil
	}, callPathReader{}, "repo", resolver, registry)
	if err != nil {
		t.Fatalf("runWithFleet: %v", err)
	}

	for _, item := range result.Items {
		if item.Level != LevelMedium {
			t.Errorf("model mismatch must classify as LevelMedium, got: %s", item.Level)
		}
		if !strings.Contains(item.Reason, "native evidence error") || !strings.Contains(item.Reason, "model mismatch") {
			t.Errorf("expected reason to contain 'model mismatch', got: %q", item.Reason)
		}
	}
}

func TestAttention_RunWithFleet_ContextCensusCancellation_PropagatesToUnderlyingOperation(t *testing.T) {
	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "lane-1", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(_ context.Context, laneName string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: laneName, Task: "CHA-1", Scope: "task"},
		}, nil
	}

	censusCancelled := make(chan struct{})
	censusContextAware := func(ctx context.Context) ([]kick.AgentEntry, error) {
		select {
		case <-ctx.Done():
			close(censusCancelled)
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, errors.New("timeout not observed by census")
		}
	}

	start := time.Now()
	_, err = runWithFleet(censusContextAware, callPathReader{}, "repo", resolver, registry)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "cancelled") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected timeout/cancellation error, got: %v", err)
	}

	// Must unblock within aggregate deadline (~15s)
	if elapsed > 18*time.Second {
		t.Errorf("runWithFleet with context-aware census took too long: %v (expected <= 18s)", elapsed)
	}

	// Ensure census observed cancellation
	select {
	case <-censusCancelled:
		// cancellation was observed directly by the underlying census operation
	case <-time.After(1 * time.Second):
		t.Errorf("underlying census did not observe context cancellation")
	}
}

func TestAttention_RunWithFleet_UnboundRoute_RefusesEvidence(t *testing.T) {
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	now := time.Now().UTC()
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":"model","finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	kick.SetStandingOverride([]string{"forge-lane-1"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "lane-1", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(_ context.Context, laneName string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: laneName, Task: "CHA-1", Scope: "task"},
		}, nil
	}

	// Herdr row omits model and provider -> Unbound route
	fleet := []kick.AgentEntry{
		{
			Name:       "forge-lane-1",
			Label:      "lane-1",
			Kind:       "opencode",
			Status:     "working",
			PaneID:     "p-1",
			Session:    kick.AgentSession{Value: sessionID},
			TerminalID: "term-1",
		},
	}

	result, err := runWithFleet(func() ([]kick.AgentEntry, error) {
		return fleet, nil
	}, callPathReader{}, "repo", resolver, registry)
	if err != nil {
		t.Fatalf("runWithFleet: %v", err)
	}

	for _, item := range result.Items {
		if item.Level != LevelMedium {
			t.Errorf("unbound route must classify as LevelMedium, got: %s", item.Level)
		}
		if !strings.Contains(item.Reason, "native evidence error") || !strings.Contains(item.Reason, "unbound model route") {
			t.Errorf("expected reason to contain 'unbound model route', got: %q", item.Reason)
		}
	}
}

func TestAttention_RunWithFleet_UnmarshaledNativeHerdrTransport_AcceptsEvidence(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"
	provider := "litellm"

	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/repo"},"messages":[
		{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":%q,"modelID":%q,"finish":"length","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, provider, model, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	restore := process.SetDefaultExportRunner(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	})
	defer restore()

	kick.SetStandingOverride([]string{"forge-lane-1"})
	t.Cleanup(func() { kick.SetStandingOverride(nil) })

	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "lane-1", Role: "worker", Standing: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	resolver := func(_ context.Context, laneName string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{
			{Repository: "repo", Owner: "worker", Lane: laneName, Task: "CHA-1", Scope: "task"},
		}, nil
	}

	// Unmarshal native Herdr JSON into kick.AgentEntry
	nativeJSON := fmt.Sprintf(`{"result":{"agents":[
		{
			"name":"forge-lane-1",
			"label":"lane-1",
			"agent":"opencode",
			"agent_status":"working",
			"pane_id":"p-1",
			"tab_id":"t-1",
			"terminal_id":"term-1",
			"workspace_id":"ws-1",
			"cwd":"/repo",
			"revision":10,
			"state_change_seq":3,
			"tab_generation":1,
			"agent_session":{"source":"native","agent":"opencode","kind":"opencode","value":%q},
			"model":%q,
			"provider":%q
		}
	]}}`, sessionID, model, provider)

	var listRes kick.AgentListResult
	if err := json.Unmarshal([]byte(nativeJSON), &listRes); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	fleet := listRes.Result.Agents
	if len(fleet) != 1 || fleet[0].ExpectedModel != model || fleet[0].ExpectedProvider != provider {
		t.Fatalf("unmarshaled fleet invalid: %#v", fleet)
	}

	result, err := runWithFleet(func() ([]kick.AgentEntry, error) {
		return fleet, nil
	}, callPathReader{}, "repo", resolver, registry)
	if err != nil {
		t.Fatalf("runWithFleet: %v", err)
	}

	// Native evidence with finish=length is successfully accepted and classified as LevelMedium (finish=length needs attention)
	found := false
	for _, item := range result.Items {
		if item.Name == "forge-lane-1" || item.Name == "lane-1" {
			found = true
			if item.Level != LevelMedium {
				t.Errorf("expected finish=length to classify as LevelMedium, got %s (reason: %s)", item.Level, item.Reason)
			}
			if strings.Contains(item.Reason, "unbound model route") {
				t.Errorf("evidence was incorrectly rejected as unbound model route: %s", item.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("expected lane in attention result items: %#v", result.Items)
	}
}
