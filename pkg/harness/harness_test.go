package harness

import (
	"reflect"
	"testing"
)

func TestGetHarnessConfig(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected HarnessType
		binary   string
	}{
		{"Claude", "claude", HarnessClaude, "claude"},
		{"Codex", "codex", HarnessCodex, "codex"},
		{"OpenCode", "opencode", HarnessOpenCode, "opencode"},
		{"Grok", "grok", HarnessGrok, "grok"},
		{"Kimi", "kimi", HarnessKimi, "kimi"},
		{"AGY", "agy", HarnessAGY, "antigravity-cli"},
		{"Antigravity", "antigravity", HarnessAGY, "antigravity-cli"},
		{"PI", "pi", HarnessPI, "pi"},
	}

	for _, tt := range tests {
		cfg := GetHarnessConfig(tt.input)
		if cfg.Type != tt.expected || cfg.BinaryName != tt.binary {
			t.Errorf("GetHarnessConfig(%s) = %+v, expected type %s, binary %s", tt.input, cfg, tt.expected, tt.binary)
		}
	}
}

func TestBuildInvocation(t *testing.T) {
	cfg := GetHarnessConfig("pi")
	inv := cfg.BuildInvocation("do work")

	expected := []string{"pi", "-p", "do work"}
	if !reflect.DeepEqual(inv, expected) {
		t.Errorf("BuildInvocation() = %v, expected %v", inv, expected)
	}
}
