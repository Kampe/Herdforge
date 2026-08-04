package toolpolicy_test

import (
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolpolicy"
)

func TestCodexPolicyDisablesInheritedMCPKeepsCLI(t *testing.T) {
	argv, cfg, err := toolpolicy.Require(toolpolicy.RoleWorker, "codex", []string{"codex", "--model", "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Valid() || cfg.MCPServers[toolpolicy.CodeReviewGraph] || !cfg.CLI[toolpolicy.CodeReviewGraph] {
		t.Fatalf("bad effective config: %+v", cfg)
	}
	if len(argv) != 5 || argv[3] != "-c" || argv[4] != "mcp_servers.code-review-graph.enabled=false" {
		t.Fatalf("missing compiled override: %v", argv)
	}
}

func TestEffectiveConfigRejectsMissingCRGMapKey(t *testing.T) {
	cfg := toolpolicy.EffectiveConfig{
		MCPServers: map[string]bool{},
		CLI:        map[string]bool{toolpolicy.CodeReviewGraph: true},
	}
	if cfg.Valid() {
		t.Fatal("missing CRG MCP key must not default to disabled")
	}
}

func fakeInheritedCRGChildren(argv []string) int {
	for i := range argv {
		if i+1 < len(argv) && argv[i] == "-c" && argv[i+1] == "mcp_servers.code-review-graph.enabled=false" {
			return 0
		}
	}
	return 1
}

func TestProductionLaunchRejectsInheritedCRGChild(t *testing.T) {
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{CLIPresent: func(cli string) bool { return cli == launch.WorkerProvider }, Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	d, err := r.Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := launch.Validate(launch.Request{Decision: d}, nil); err != nil {
		t.Fatalf("real launch validation rejected compiled route: %v", err)
	}
	mutated := *d
	mutated.Argv = append([]string(nil), d.Argv[:len(d.Argv)-2]...)
	if got := fakeInheritedCRGChildren(mutated.Argv); got != 1 {
		t.Fatalf("mutated launch did not model inherited CRG child: %d", got)
	}
	if err := launch.Validate(launch.Request{Decision: &mutated}, nil); err == nil {
		t.Fatal("production Validate accepted a launch that would inherit a CRG child")
	}
}

func TestExceptionalAuthorizationBindsAllIdentity(t *testing.T) {
	a := toolpolicy.Authorization{Repository: "repo", Role: "reviewer", Server: "x", Transport: "stdio", OwnerSession: "s7", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if !a.Matches(a, time.Now().UTC()) {
		t.Fatal("matching authorization rejected")
	}
	for name, mutate := range map[string]func(*toolpolicy.Authorization){"repo": func(x *toolpolicy.Authorization) { x.Repository = "other" }, "role": func(x *toolpolicy.Authorization) { x.Role = "worker" }, "server": func(x *toolpolicy.Authorization) { x.Server = "other" }, "transport": func(x *toolpolicy.Authorization) { x.Transport = "http" }, "session": func(x *toolpolicy.Authorization) { x.OwnerSession = "s8" }} {
		t.Run(name, func(t *testing.T) {
			b := a
			mutate(&b)
			if a.Matches(b, time.Now().UTC()) {
				t.Fatal("identity mutation accepted")
			}
		})
	}
}
