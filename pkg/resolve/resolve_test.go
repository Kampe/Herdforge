package resolve

import (
	"os"
	"strings"
	"testing"
)

// mockScorer returns a deterministic RouteScore for testing.
type mockScorer struct {
	providers []string
}

func (m *mockScorer) Score(shape string, preferProvider string) *RouteScore {
	// If a specific provider is requested, return it (if in our list).
	if preferProvider != "" {
		return &RouteScore{
			Provider: preferProvider,
			Model:    modelForProvider(preferProvider),
			Effort:   "medium",
		}
	}
	// Otherwise return the first available provider.
	if len(m.providers) > 0 {
		p := m.providers[0]
		return &RouteScore{
			Provider: p,
			Model:    modelForProvider(p),
			Effort:   "medium",
		}
	}
	return nil
}

func modelForProvider(p string) string {
	switch p {
	case "claude":
		return "claude-sonnet-5"
	case "codex":
		return "gpt-5.6-luna"
	case "agy":
		return "gemini-3.6-flash-high"
	case "ollama":
		return "litellm/ollama/glm-5.2:cloud"
	case "opencode":
		return "opencode/deepseek-v4-flash"
	case "grok":
		return "grok-3-beta"
	case "lazer":
		return "lazer/deepseek-v4-flash"
	default:
		return "gpt-5.6-luna"
	}
}

var testRegistryJSON = `{
  "version": 1,
  "provider_constraints": {
    "claude-haiku-4-5": {
      "forbid_standing": true,
      "floor_to": "claude-sonnet-5"
    },
    "lazer": {
      "last_resort": true
    }
  },
  "risk_classes": {
    "standard": {
      "cost_tier": "market"
    },
    "deep-reasoning": {
      "provider_pin": "claude",
      "model_pin": "claude-opus-5",
      "effort_floor": "high",
      "cost_tier": "premium"
    },
    "fixture-critical": {
      "model_floor_class": "sonnet",
      "byte_replay_review_off_claude": true,
      "cost_tier": "market"
    }
  },
  "lanes": [
    {
      "id": "scout-planner",
      "route_shape": "research",
      "risk_class": "deep-reasoning",
      "prefer": null
    },
    {
      "id": "platform-ops",
      "route_shape": "implementation",
      "risk_class": "standard",
      "prefer": null
    },
    {
      "id": "qa-sentinel",
      "route_shape": "qa",
      "risk_class": "standard",
      "prefer": "agy"
    },
    {
      "id": "ux-comber",
      "route_shape": "implementation",
      "risk_class": "standard",
      "prefer": "ollama",
      "prefer_model": "litellm/ollama/glm-5.2:cloud"
    },
    {
      "id": "defi-crusader",
      "route_shape": "implementation",
      "risk_class": "fixture-critical",
      "prefer": null
    },
    {
      "id": "chain-indexer",
      "route_shape": "implementation",
      "risk_class": "fixture-critical",
      "prefer": null
    }
  ]
}`

func mustParseRegistry(t *testing.T, data string) *LaneRegistry {
	t.Helper()
	reg, err := ParseRegistry([]byte(data))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	return reg
}

func TestParseRegistry(t *testing.T) {
	reg, err := ParseRegistry([]byte(testRegistryJSON))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	if reg.Version != 1 {
		t.Errorf("Version = %d, want 1", reg.Version)
	}
	if len(reg.Lanes) != 6 {
		t.Errorf("len(Lanes) = %d, want 6", len(reg.Lanes))
	}
}

func TestParseRegistry_EmptyLanes(t *testing.T) {
	_, err := ParseRegistry([]byte(`{"version":1,"lanes":[],"risk_classes":{}}`))
	if err == nil || !strings.Contains(err.Error(), "no lanes") {
		t.Errorf("expected 'no lanes' error, got %v", err)
	}
}

func TestLaneDef_Standing(t *testing.T) {
	reg := mustParseRegistry(t, `{"version":1,"lanes":[
		{"id":"harvest","route_shape":"bounded","risk_class":"standard","standing":true},
		{"id":"worker","route_shape":"code","risk_class":"standard"}
	]}`)
	if len(reg.Lanes) != 2 {
		t.Fatalf("len(Lanes) = %d, want 2", len(reg.Lanes))
	}
	if !reg.Lanes[0].Standing {
		t.Errorf("lane %q: Standing = false, want true", reg.Lanes[0].ID)
	}
	if reg.Lanes[1].Standing {
		t.Errorf("lane %q: Standing = true, want false (field omitted defaults to ephemeral)", reg.Lanes[1].ID)
	}
}

func TestLaneIDs(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})
	ids := resolver.LaneIDs()
	expected := []string{"scout-planner", "platform-ops", "qa-sentinel", "ux-comber", "defi-crusader", "chain-indexer"}
	if len(ids) != len(expected) {
		t.Fatalf("got %d ids, want %d", len(ids), len(expected))
	}
	for i, id := range ids {
		if id != expected[i] {
			t.Errorf("ids[%d] = %q, want %q", i, id, expected[i])
		}
	}
}

func TestLaneField(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})

	shape, err := resolver.LaneField("scout-planner", "route_shape")
	if err != nil {
		t.Fatal(err)
	}
	if shape != "research" {
		t.Errorf("route_shape = %q, want %q", shape, "research")
	}

	_, err = resolver.LaneField("nonexistent", "id")
	if err == nil {
		t.Error("expected error for nonexistent lane")
	}

	_, err = resolver.LaneField("platform-ops", "bogus")
	if err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestResolve_StandardLane(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})

	result := resolver.Resolve("platform-ops", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	if result.Provider != "claude" {
		t.Errorf("provider = %q, want %q", result.Provider, "claude")
	}
	if result.Effort != "medium" {
		t.Errorf("effort = %q, want %q", result.Effort, "medium")
	}
	if result.RouteShape != "implementation" {
		t.Errorf("route_shape = %q, want %q", result.RouteShape, "implementation")
	}
	if result.RiskClass != "standard" {
		t.Errorf("risk_class = %q, want %q", result.RiskClass, "standard")
	}
}

func TestResolve_DeepReasoningLane(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})

	result := resolver.Resolve("scout-planner", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	if result.Provider != "claude" {
		t.Errorf("provider = %q, want %q", result.Provider, "claude")
	}
	if result.Model != "claude-opus-5" {
		t.Errorf("model = %q, want %q", result.Model, "claude-opus-5")
	}
	if result.Effort != "high" {
		t.Errorf("effort = %q, want %q", result.Effort, "high")
	}
	if result.CostTier != "premium" {
		t.Errorf("cost_tier = %q, want %q", result.CostTier, "premium")
	}
}

func TestResolve_SoftPreferHonored(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"agy"}})

	result := resolver.Resolve("qa-sentinel", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	if result.Provider != "agy" {
		t.Errorf("provider = %q, want %q", result.Provider, "agy")
	}
	// Soft preference should be honored
	found := false
	for _, c := range result.Constraints {
		if strings.Contains(c, "prefer=agy(honored)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected constraint 'prefer=agy(honored)', got %v", result.Constraints)
	}
}

// fallbackScorer respects preferProvider but returns a different provider
// so the resolution logic detects the fallback.
type fallbackScorer struct {
	defaultProvider string
}

func (m *fallbackScorer) Score(shape string, preferProvider string) *RouteScore {
	if preferProvider != "" {
		// Act like the preferred provider isn't available
		// and return nil so the fallback path runs
		return nil
	}
	return &RouteScore{
		Provider: m.defaultProvider,
		Model:    modelForProvider(m.defaultProvider),
		Effort:   "medium",
	}
}

func TestResolve_SoftPreferFallsBack(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	// fallbackScorer returns nil for the preferred provider, then claude as fallback
	resolver := New(reg, &fallbackScorer{defaultProvider: "claude"})

	result := resolver.Resolve("qa-sentinel", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	// Soft preference should have fallen back
	found := false
	for _, c := range result.Constraints {
		if strings.Contains(c, "prefer=agy(fell-back)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected constraint 'prefer=agy(fell-back)', got %v", result.Constraints)
	}
	if result.Provider != "claude" {
		t.Errorf("expected provider 'claude' after fallback, got %q", result.Provider)
	}
}

func TestResolve_PreferModelApplied(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	// ux-comber prefers ollama with prefer_model=litellm/ollama/glm-5.2:cloud
	resolver := New(reg, &mockScorer{providers: []string{"ollama"}})

	result := resolver.Resolve("ux-comber", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	if result.Provider != "ollama" {
		t.Errorf("provider = %q, want %q", result.Provider, "ollama")
	}
	if result.Model != "litellm/ollama/glm-5.2:cloud" {
		t.Errorf("model = %q, want %q", result.Model, "litellm/ollama/glm-5.2:cloud")
	}
}

func TestResolve_DropPrefer(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude", "agy"}})

	result := resolver.Resolve("qa-sentinel", true)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	// With dropPrefer=true, the prefer=agy should be skipped and we should get the
	// first provider from the scorer (claude)
	if result.Provider != "claude" {
		t.Errorf("expected provider 'claude' with dropPrefer, got %q", result.Provider)
	}
}

func TestResolve_FixtureCriticalFloor(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	// Use a scorer that returns a cheap model (haiku-class)
	resolver := New(reg, &mockScorer{providers: []string{"ollama"}})

	result := resolver.Resolve("defi-crusader", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	// Since ollama has no providerSonnetModel mapping, the floor is unmet and
	// byte_replay_review should be true
	if !result.ByteReplayReview {
		t.Errorf("expected byte_replay_review=true for fixture-critical on non-Claude without model mapping")
	}
}

func TestResolve_UnrouteableLane(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{}}) // no providers -> nil scores

	result := resolver.Resolve("platform-ops", false)
	if result.Resolvable {
		t.Error("expected unresolvable for lane with no healthy providers")
	}
	if result.Reason == "" {
		t.Error("expected non-empty reason for unresolved lane")
	}
}

func TestResolve_UnknownLane(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})

	result := resolver.Resolve("bogus-lane", false)
	if result.Resolvable {
		t.Error("expected unresolvable for unknown lane")
	}
	if !strings.Contains(result.Reason, "not in registry") {
		t.Errorf("reason = %q, want 'not in registry'", result.Reason)
	}
}

func TestResolveAll(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})

	results := resolver.ResolveAll()
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}
	for _, res := range results {
		if res.Resolvable == false {
			t.Errorf("lane %s should be resolvable", res.Lane)
		}
	}
}

func TestEffortRank(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"low", 1},
		{"medium", 2},
		{"high", 3},
		{"xhigh", 4},
		{"max", 5},
		{"", 2},
		{"unknown", 2},
	}
	for _, tc := range tests {
		if got := effortRank(tc.input); got != tc.want {
			t.Errorf("effortRank(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestModelClassRank(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", 1},
		{"gpt-5.3-codex-spark", 1},
		{"qwen3.5-turbo", 1},
		{"claude-sonnet-5", 2},
		{"gpt-5.6-luna", 2},
		{"gemini-3.6-flash-high", 2},
		{"qwen-2.5-72b", 2},
		{"claude-opus-4", 3},
		{"grok-3-beta", 3},
		{"gemini-3-pro-001", 3},
		{"deepseek-v3", 3},
		{"kimi-k3", 3},
		{"claude-opus-5", 4},
		{"claude-fable", 4},
		{"gpt-5-sol", 4},
		{"opencode/deepseek-v4-flash", 3},
	}
	for _, tc := range tests {
		if got := modelClassRank(tc.model); got != tc.want {
			t.Errorf("modelClassRank(%q) = %d, want %d", tc.model, got, tc.want)
		}
	}
}

func TestProviderSonnetModel(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{"claude", "claude-sonnet-5"},
		{"codex", "gpt-5.6-luna"},
		{"agy", "gemini-3.6-flash-high"},
		{"ollama", ""},
		{"unknown", ""},
	}
	for _, tc := range tests {
		if got := providerSonnetModel(tc.provider); got != tc.want {
			t.Errorf("providerSonnetModel(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

func TestFloorRankForClass(t *testing.T) {
	tests := []struct {
		class string
		want  int
	}{
		{"", 0},
		{"sonnet", 2},
		{"opus", 3},
		{"unknown", 2},
	}
	for _, tc := range tests {
		if got := floorRankForClass(tc.class); got != tc.want {
			t.Errorf("floorRankForClass(%q) = %d, want %d", tc.class, got, tc.want)
		}
	}
}

func TestCostTierForModel(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-haiku-4-5", "cheap"},
		{"claude-sonnet-5", "market"},
		{"claude-opus-5", "premium"},
	}
	for _, tc := range tests {
		if got := costTierForModel(tc.model); got != tc.want {
			t.Errorf("costTierForModel(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestCostTierCeilingRank(t *testing.T) {
	tests := []struct {
		tier string
		want int
	}{
		{"cheap", 1},
		{"market", 3},
		{"premium", 4},
		{"unknown", 3},
	}
	for _, tc := range tests {
		if got := costTierCeilingRank(tc.tier); got != tc.want {
			t.Errorf("costTierCeilingRank(%q) = %d, want %d", tc.tier, got, tc.want)
		}
	}
}

func TestProviderModelWithinRank(t *testing.T) {
	tests := []struct {
		provider string
		ceil     int
		want     string
	}{
		{"claude", 3, "claude-sonnet-5"},
		{"claude", 1, "claude-haiku-4-5"},
		{"codex", 3, "gpt-5.6-luna"},
		{"codex", 1, "gpt-5.3-codex-spark"},
		{"ollama", 3, ""},
		{"claude", 4, "claude-sonnet-5"}, // ceil 4 > 3 -> sonnet
	}
	for _, tc := range tests {
		if got := providerModelWithinRank(tc.provider, tc.ceil); got != tc.want {
			t.Errorf("providerModelWithinRank(%q, %d) = %q, want %q", tc.provider, tc.ceil, got, tc.want)
		}
	}
}

func TestRationaleLine_Resolvable(t *testing.T) {
	res := &ResolvedLane{
		Lane:        "platform-ops",
		RouteShape:  "implementation",
		RiskClass:   "standard",
		Provider:    "claude",
		Model:       "claude-sonnet-5",
		Effort:      "medium",
		CostTier:    "market",
		Constraints: []string{"none"},
		Resolvable:  true,
	}
	line := res.RationaleLine()
	if !strings.Contains(line, "resolve platform-ops") {
		t.Errorf("rationale line = %q, missing 'resolve platform-ops'", line)
	}
	if !strings.Contains(line, "claude/claude-sonnet-5") {
		t.Errorf("rationale line = %q, missing provider/model", line)
	}
}

func TestRationaleLine_Unrouteable(t *testing.T) {
	res := &ResolvedLane{
		Lane:        "bogus",
		RouteShape:  "implementation",
		RiskClass:   "standard",
		Effort:      "medium",
		Constraints: []string{"none"},
		Resolvable:  false,
		Reason:      "no healthy provider",
	}
	line := res.RationaleLine()
	if !strings.Contains(line, "UNROUTABLE") {
		t.Errorf("rationale line = %q, missing 'UNROUTABLE'", line)
	}
	if !strings.Contains(line, "no healthy provider") {
		t.Errorf("rationale line = %q, missing reason", line)
	}
}

func TestEnvOverride_NoClaude(t *testing.T) {
	os.Setenv("HERD_NO_CLAUDE", "1")
	defer os.Unsetenv("HERD_NO_CLAUDE")

	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"codex"}})

	result := resolver.Resolve("scout-planner", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable with no-claude fallback, got: %s", result.Reason)
	}
	// Should fall through to codex since claude pin was dropped
	found := false
	for _, c := range result.Constraints {
		if strings.Contains(c, "provider_pin=claude(dropped:no-claude)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected no-claude constraint, got %v", result.Constraints)
	}
}

func TestEnvOverride_ClaudeOnlyDropsPrefer(t *testing.T) {
	os.Setenv("HERD_CLAUDE_ONLY", "1")
	defer os.Unsetenv("HERD_CLAUDE_ONLY")

	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})

	result := resolver.Resolve("qa-sentinel", false)
	if !result.Resolvable {
		t.Fatalf("expected resolvable, got reason: %s", result.Reason)
	}
	// Soft prefer to agy should have been dropped
	for _, c := range result.Constraints {
		if strings.Contains(c, "prefer=agy(dropped:claude-only)") {
			return
		}
	}
	t.Errorf("expected claude-only dropping constraint, got %v", result.Constraints)
}

func TestResolveAllJSON(t *testing.T) {
	reg := mustParseRegistry(t, testRegistryJSON)
	resolver := New(reg, &mockScorer{providers: []string{"claude"}})

	jsonOutput, err := resolver.ResolveAllJSON()
	if err != nil {
		t.Fatalf("ResolveAllJSON: %v", err)
	}
	if !strings.Contains(jsonOutput, "scout-planner") {
		t.Errorf("JSON output missing scout-planner: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, "platform-ops") {
		t.Errorf("JSON output missing platform-ops: %s", jsonOutput)
	}
}
