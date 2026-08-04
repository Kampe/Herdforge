package toolpolicy

import (
	"testing"
	"time"
)

func TestCodexPolicyDisablesInheritedMCPKeepsCLI(t *testing.T) {
	argv, cfg, err := Require(RoleWorker, "codex", []string{"codex", "--model", "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Valid() || cfg.MCPServers[CodeReviewGraph] || !cfg.CLI[CodeReviewGraph] {
		t.Fatalf("bad effective config: %+v", cfg)
	}
	if len(argv) != 5 || argv[3] != "-c" || argv[4] != "mcp_servers.code-review-graph.enabled=false" {
		t.Fatalf("missing compiled override: %v", argv)
	}
}

func fakeChildren(argv []string, inherited bool) int {
	for i := range argv {
		if i+1 < len(argv) && argv[i] == "-c" && argv[i+1] == "mcp_servers.code-review-graph.enabled=false" {
			return 0
		}
	}
	if inherited {
		return 1
	}
	return 0
}

func TestFakeGlobalCRGMCPMutationIsRedGreen(t *testing.T) {
	compiled, _, err := Require(RoleForgeSmith, "codex", []string{"codex", "--model", "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fakeChildren(compiled, true); got != 0 {
		t.Fatalf("compiled launch produced %d fake CRG children", got)
	}
	mutated := []string{"codex", "--model", "gpt-5.6-luna"}
	if got := fakeChildren(mutated, true); got != 1 {
		t.Fatalf("isolation mutation produced %d children, want 1", got)
	}
}

func TestExceptionalAuthorizationBindsAllIdentity(t *testing.T) {
	a := Authorization{Repository: "repo", Role: "reviewer", Server: "x", Transport: "stdio", OwnerSession: "s7", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if !a.Matches(a, time.Now().UTC()) {
		t.Fatal("matching authorization rejected")
	}
	for name, mutate := range map[string]func(*Authorization){"repo": func(x *Authorization) { x.Repository = "other" }, "role": func(x *Authorization) { x.Role = "worker" }, "server": func(x *Authorization) { x.Server = "other" }, "transport": func(x *Authorization) { x.Transport = "http" }, "session": func(x *Authorization) { x.OwnerSession = "s8" }} {
		t.Run(name, func(t *testing.T) {
			b := a
			mutate(&b)
			if a.Matches(b, time.Now().UTC()) {
				t.Fatal("identity mutation accepted")
			}
		})
	}
}
