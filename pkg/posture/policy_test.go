package posture

import (
	"testing"
)

func TestAllowClaudeOnlyNeverProxy(t *testing.T) {
	cases := []struct {
		provider, model, family string
		want                    bool
	}{
		{"claude", "claude-sonnet-5", "anthropic", true},
		{"codex", "gpt-5.6-luna", "openai", false},
		{"agy", "claude-opus-4-6-thinking", "anthropic", false},
		{"lazer", "claude-sonnet", "anthropic", false},
		{"lazer", "gpt-x", "proxy", false},
		{"claude", "something", "proxy", false},
	}
	for _, tc := range cases {
		ok, reason := Allow(ModeClaudeOnly, tc.provider, tc.model, tc.family)
		if ok != tc.want {
			t.Errorf("Allow(claude-only, %s/%s/%s)=%v (%s), want %v",
				tc.provider, tc.model, tc.family, ok, reason, tc.want)
		}
	}
}

func TestAllowNoClaudeExcludesEveryAnthropicFamily(t *testing.T) {
	cases := []struct {
		provider, model, family string
		want                    bool
	}{
		{"claude", "claude-sonnet-5", "anthropic", false},
		{"agy", "claude-opus-4-6-thinking", "anthropic", false},
		{"lazer", "claude-x", "anthropic", false},
		{"grok", "grok-4", "xai", true},
		{"codex", "gpt-5.6-luna", "openai", true},
		{"agy", "gemini-3.1-pro-high", "google", true},
		{"lazer", "gpt-x", "proxy", true},
	}
	for _, tc := range cases {
		ok, reason := Allow(ModeNoClaude, tc.provider, tc.model, tc.family)
		if ok != tc.want {
			t.Errorf("Allow(no-claude, %s/%s/%s)=%v (%s), want %v",
				tc.provider, tc.model, tc.family, ok, reason, tc.want)
		}
	}
}

func TestFilterProvidersClaudeOnlyInjectsNative(t *testing.T) {
	kept, excl := FilterProviders(ModeClaudeOnly, []string{"codex", "grok"})
	if len(kept) != 1 || kept[0] != "claude" {
		t.Fatalf("kept=%v excl=%v", kept, excl)
	}
	if len(excl) != 2 {
		t.Fatalf("expected 2 exclusions, got %v", excl)
	}
}

func TestFilterStableOrder(t *testing.T) {
	in := []Candidate{
		{Provider: "claude", Family: "anthropic"},
		{Provider: "grok", Family: "xai"},
		{Provider: "agy", Model: "claude-x", Family: "anthropic"},
		{Provider: "codex", Family: "openai"},
	}
	kept, excl := Filter(ModeNoClaude, in)
	if len(kept) != 2 || kept[0].Provider != "grok" || kept[1].Provider != "codex" {
		t.Fatalf("kept=%v", kept)
	}
	if len(excl) != 2 {
		t.Fatalf("excl=%v", excl)
	}
	// Reasons must be non-empty so status is useful (and tests are non-vacuous).
	for _, e := range excl {
		if e.Reason == "" {
			t.Fatalf("empty exclusion reason: %+v", e)
		}
	}
}
