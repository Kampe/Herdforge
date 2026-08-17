package agentpolicy

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompileCodexArgsInjectsNestedDenials(t *testing.T) {
	got, err := CompileCodexArgs([]string{"codex", "--model", "gpt-5.6-luna", "-c", "model_reasoning_effort=high", "-a", "never"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--disable", "multi_agent", "--disable", "multi_agent_v2", "--model", "gpt-5.6-luna", "-c", "model_reasoning_effort=high", "-a", "never"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Idempotent.
	again, err := CompileCodexArgs(got)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("recompile drifted: %v %v", again, err)
	}
}

func TestCompileCodexArgsRejectsExplicitEnable(t *testing.T) {
	_, err := CompileCodexArgs([]string{"codex", "-c", "features.multi_agent=true"})
	if err == nil {
		t.Fatal("explicit multi_agent enable must fail closed")
	}
}

func TestCompileClaudeArgsInjectsDisallowedTools(t *testing.T) {
	got, err := CompileClaudeArgs([]string{"claude", "--model", "claude-sonnet-5", "--effort", "high"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--mcp-config", `{"mcpServers":{}}`, "--strict-mcp-config", "--disable-slash-commands", "--disallowed-tools", "Agent", "Task", "ToolSearch", "--model", "claude-sonnet-5", "--effort", "high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	again, err := CompileClaudeArgs(got)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("recompile drifted: %v %v", again, err)
	}
}

func TestRequireNestedDenyAcceptsCompiledAndRejectsBare(t *testing.T) {
	bare := []string{"codex", "--model", "m"}
	if err := RequireNestedDeny("codex", bare); err == nil {
		t.Fatal("bare codex argv must fail")
	}
	compiled, err := CompileCodexArgs(bare)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireNestedDeny("codex", compiled); err != nil {
		t.Fatal(err)
	}
	if err := RequireNestedDeny("grok", []string{"grok", "--model", "x"}); err != nil {
		t.Fatalf("non-codex/claude must be out of scope: %v", err)
	}
	bareClaude := []string{"claude", "--model", "m", "--effort", "high"}
	if err := RequireNestedDeny("claude", bareClaude); err == nil {
		t.Fatal("bare claude argv must fail")
	}
	cClaude, err := CompileClaudeArgs(bareClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireNestedDeny("claude", cClaude); err != nil {
		t.Fatal(err)
	}
}

func TestCompileRejectsWrongBinary(t *testing.T) {
	if _, err := CompileCodexArgs([]string{"pi", "--model", "x"}); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("expected codex binary error, got %v", err)
	}
	if _, err := CompileClaudeArgs([]string{"pi", "--model", "x"}); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Fatalf("expected claude binary error, got %v", err)
	}
}
