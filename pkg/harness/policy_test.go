package harness

import (
	"reflect"
	"testing"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
)

func TestBuildPolicyInvocationCompilesBoundHandoff(t *testing.T) {
	key := []byte("fixture-key")
	policy, err := agentpolicy.NewContract("github.com/Kampe/Herdforge", "FAC-173", "lane", "forge-smith", 1, "session", "tab", "pane", "codex", "herd dispatch", key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetHarnessConfig("codex").BuildPolicyInvocation("prompt", policy, key)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--disable", "multi_agent", "--disable", "multi_agent_v2", "-c", "mcp_servers.code-review-graph={command=\"false\",enabled=false}", "prompt"}
	if !reflect.DeepEqual(got.Argv, want) {
		t.Fatalf("argv = %v, want %v", got.Argv, want)
	}
}

func TestClaudePolicyInvocationUsesDisallowedTools(t *testing.T) {
	key := []byte("fixture-key")
	policy, err := agentpolicy.NewContract("repo", "task", "lane", "role", 1, "session", "tab", "pane", "claude", "herd dispatch", key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetHarnessConfig("claude").BuildPolicyInvocation("prompt", policy, key)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--mcp-config", "{}", "--strict-mcp-config", "--disable-slash-commands", "--disallowed-tools", "Agent", "Task", "ToolSearch", "-p", "prompt"}
	if !reflect.DeepEqual(got.Argv, want) {
		t.Fatalf("argv = %v, want %v", got.Argv, want)
	}
}

func TestBuildPolicyInvocationRejectsWrongFamilyForSupportedHarnesses(t *testing.T) {
	key := []byte("fixture-key")
	for _, tc := range []struct{ name, family string }{{"claude", "codex"}, {"codex", "claude"}} {
		policy, err := agentpolicy.NewContract("repo", "task", "lane", "role", 1, "session", "tab", "pane", tc.family, "herd dispatch", key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := GetHarnessConfig(tc.name).BuildPolicyInvocation("prompt", policy, key); err == nil {
			t.Fatalf("%s must reject %s family", tc.name, tc.family)
		}
	}
}

func TestUnsupportedHarnessFailsClosed(t *testing.T) {
	key := []byte("fixture-key")
	policy, _ := agentpolicy.NewContract("repo", "task", "lane", "role", 1, "session", "tab", "pane", "family", "herd dispatch", key)
	if _, err := GetHarnessConfig("opencode").BuildPolicyInvocation("prompt", policy, key); err == nil {
		t.Fatal("unsupported harness must fail closed")
	}
}

func TestBuildPolicyInvocationRejectsUnenforceablePolicy(t *testing.T) {
	key := []byte("fixture-key")
	policy, _ := agentpolicy.NewContract("repo", "task", "lane", "role", 1, "session", "tab", "pane", "family", "herd dispatch", key)
	policy.PolicyDigest = "stale"
	if _, err := GetHarnessConfig("claude").BuildPolicyInvocation("prompt", policy, key); err == nil {
		t.Fatal("stale policy must not produce launch configuration")
	}
}
