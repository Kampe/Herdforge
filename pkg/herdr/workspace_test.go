package herdr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestAuditWorkspaceDriftFlagsMismatchedWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "version: \"1\"\nproject:\n  name: Herdforge\ntask_provider:\n  type: kaneo\n  project_id: project\nfleet:\n  herdr_workspace: w-herdforge\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, ".herd", "worktrees", "fac-420")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	drift := AuditWorkspaceDrift([]AgentEntry{{Name: "healthy", Workspace: "w-herdforge", ForegroundCwd: worktree}, {Name: "stranded", Workspace: "w-marina", ForegroundCwd: worktree}}, []WorkspaceEntry{{WorkspaceID: "w-herdforge", Label: "Herdforge"}, {WorkspaceID: "w-marina", Label: "marina-infra"}})
	if len(drift) != 1 {
		t.Fatalf("drift = %#v, want one finding", drift)
	}
	if drift[0].Agent != "stranded" || drift[0].ExpectedWorkspace != "w-herdforge" {
		t.Fatalf("drift = %#v, want stranded agent bound to w-herdforge", drift[0])
	}
}

func TestAuditWorkspaceDriftDoesNotFlagHealthyUnregisteredWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	drift := AuditWorkspaceDrift([]AgentEntry{{Name: "chainseer", Workspace: "w-chainseer", ForegroundCwd: root}}, []WorkspaceEntry{{WorkspaceID: "w-chainseer", Label: filepath.Base(root)}})
	if len(drift) != 0 {
		t.Fatalf("healthy unregistered fleet produced drift: %#v", drift)
	}
}

func workspaceBindingRepo(t *testing.T, workspace string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := "version: \"1\"\nproject:\n  name: Herdforge\ntask_provider:\n  type: kaneo\n  project_id: project\nfleet:\n  herdr_workspace: " + workspace + "\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func stubWorkspaceList(t *testing.T, payload string) {
	t.Helper()
	old := runHerdr
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "workspace" && args[1] == "list" {
			return payload, nil
		}
		return `{}`, nil
	}
	t.Cleanup(func() { runHerdr = old })
}

func TestPickWorkspace(t *testing.T) {
	entries := []WorkspaceEntry{
		{WorkspaceID: "w1", Label: "kluster"},
		{WorkspaceID: "wF", Label: "Herdforge"},
		{WorkspaceID: "w3", Label: "dotfiles", Focused: true},
	}
	if got := PickWorkspace(entries, "herdforge"); got != "wF" {
		t.Errorf("label match (case-insensitive) = %q, want wF", got)
	}
	if got := PickWorkspace(entries, "unknown-repo"); got != "w3" {
		t.Errorf("focused fallback = %q, want w3", got)
	}
	if got := PickWorkspace([]WorkspaceEntry{{WorkspaceID: "w9", Label: "x"}}, "y"); got != "w9" {
		t.Errorf("first fallback = %q, want w9", got)
	}
	if got := PickWorkspace(nil, "y"); got != "wF" {
		t.Errorf("empty fallback = %q, want wF", got)
	}
}

func TestPickWorkspaceStrict_NoHardcodedFallback(t *testing.T) {
	if id, ok := PickWorkspaceStrict(nil, "herdforge"); ok || id != "" {
		t.Fatalf("empty list must fail closed, got id=%q ok=%v", id, ok)
	}
	entries := []WorkspaceEntry{
		{WorkspaceID: "wF", Label: "Herdforge"},
	}
	id, ok := PickWorkspaceStrict(entries, "herdforge")
	if !ok || id != "wF" {
		t.Fatalf("label match: id=%q ok=%v", id, ok)
	}
}

func TestRequireWorkspace_EnvWins(t *testing.T) {
	stubWorkspaceList(t, `{"result":{"workspaces":[{"workspace_id":"wExplicit","label":"other"}]}}`)
	t.Setenv("HERD_WORKSPACE", "wExplicit")
	id, err := RequireWorkspace(".")
	if err != nil {
		t.Fatalf("RequireWorkspace: %v", err)
	}
	if id != "wExplicit" {
		t.Fatalf("got %q", id)
	}
}

func TestRequireWorkspace_UnknownFailsClosed(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "")
	// Force empty PATH so WorkspaceList cannot succeed via real herdr.
	t.Setenv("PATH", "/dev/null")
	_, err := RequireWorkspace("/tmp/no-such-herdforge-repo-xyz")
	if err == nil {
		t.Fatal("expected fail-closed error when workspace cannot be resolved")
	}
}

func TestRequireWorkspace_RejectsInheritedCrossWorkspaceOverride(t *testing.T) {
	root := workspaceBindingRepo(t, "wK")
	stubWorkspaceList(t, `{"result":{"workspaces":[{"workspace_id":"wK","label":"Herdforge"},{"workspace_id":"wB","label":"Chainseer"}]}}`)
	t.Setenv("HERD_WORKSPACE", "wB")
	if _, err := RequireWorkspace(root); err == nil {
		t.Fatal("expected inherited workspace mismatch to fail closed")
	} else if !strings.Contains(err.Error(), `HERD_WORKSPACE="wB"`) || !strings.Contains(err.Error(), `registered workspace="wK"`) {
		t.Fatalf("mismatch error omitted both workspace ids: %v", err)
	}
}

func TestRequireWorkspace_AllowsExplicitOverrideOnlyWhenItMatchesBinding(t *testing.T) {
	root := workspaceBindingRepo(t, "wK")
	stubWorkspaceList(t, `{"result":{"workspaces":[{"workspace_id":"wK","label":"Herdforge"}]}}`)
	t.Setenv("HERD_WORKSPACE", "wK")
	got, err := RequireWorkspace(root)
	if err != nil {
		t.Fatalf("matching workspace rejected: %v", err)
	}
	if got != "wK" {
		t.Fatalf("workspace=%q, want wK", got)
	}
}

func TestRequireCleanupWorkspace_RejectsRuntimeRepoAndHerdrDisagreement(t *testing.T) {
	root := workspaceBindingRepo(t, "wK")
	stubWorkspaceList(t, `{"result":{"workspaces":[{"workspace_id":"wK","label":"Herdforge"},{"workspace_id":"wB","label":"Chainseer"}]}}`)
	t.Setenv("HERD_WORKSPACE", "wK")
	t.Setenv("HERDR_WORKSPACE_ID", "wB")
	if _, err := RequireCleanupWorkspace(root); err == nil {
		t.Fatal("cleanup must reject a Herdr workspace that disagrees with runtime and repo binding")
	} else if !strings.Contains(err.Error(), `HERD_WORKSPACE="wK"`) || !strings.Contains(err.Error(), `registered workspace="wK"`) || !strings.Contains(err.Error(), `HERDR_WORKSPACE_ID="wB"`) {
		t.Fatalf("mismatch error omitted workspace identities: %v", err)
	}
}

func TestResolveWorkspace_EnvWins(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wOverride")
	got := ResolveWorkspace("/tmp")
	if got != "wOverride" {
		t.Errorf("env override = %q, want wOverride", got)
	}
}

func TestResolveWorkspace_Pure_Kernel(t *testing.T) {
	entries := []WorkspaceEntry{
		{WorkspaceID: "wF", Label: "Herdforge", Focused: true},
	}
	t.Run("env_wins", func(t *testing.T) {
		got := resolveWorkspace("wEnv", "", entries, "any")
		if got != "wEnv" {
			t.Errorf("want wEnv, got %q", got)
		}
	})
	t.Run("config_wins", func(t *testing.T) {
		got := resolveWorkspace("", "wConfig", entries, "any")
		if got != "wConfig" {
			t.Errorf("want wConfig, got %q", got)
		}
	})
	t.Run("env_overrides_config", func(t *testing.T) {
		got := resolveWorkspace("wEnv", "wConfig", entries, "any")
		if got != "wEnv" {
			t.Errorf("env should beat config, got %q", got)
		}
	})
	t.Run("config_wins_over_focused", func(t *testing.T) {
		// sentinel "wX" is NOT the focused workspace ID ("wF")
		got := resolveWorkspace("", "wX", entries, "any")
		if got != "wX" {
			t.Errorf("config wX should beat focused wF, got %q", got)
		}
	})
	t.Run("label_wins", func(t *testing.T) {
		got := resolveWorkspace("", "", entries, "herdforge")
		if got != "wF" {
			t.Errorf("label match should win, got %q", got)
		}
	})
	t.Run("fallback_focused", func(t *testing.T) {
		got := resolveWorkspace("", "", entries, "unknown")
		if got != "wF" {
			t.Errorf("focused fallback should be wF, got %q", got)
		}
	})
	t.Run("fallback_empty", func(t *testing.T) {
		got := resolveWorkspace("", "", nil, "any")
		if got != "wF" {
			t.Errorf("empty fallback should be wF, got %q", got)
		}
	})
}

func TestResolveWorkspaceWithConfig_ConfigWinsOverFocusedAndIsolation(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "") // ensure env is clear
	// Sentinel that cannot coincide with PickWorkspace fallback ("wF").
	cfg := &config.Config{Fleet: config.FleetConfig{HerdrWorkspace: "wConfigSentinel"}}
	got := ResolveWorkspaceWithConfig("/tmp", cfg)
	if got != "wConfigSentinel" {
		t.Errorf("config workspace = %q, want wConfigSentinel (live focused: %q)", got, PickWorkspace(nil, ""))
	}
}

func TestResolveWorkspaceWithConfig_NilConfigFallsThrough(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "")
	got := ResolveWorkspaceWithConfig("/tmp", nil)
	if got == "" {
		t.Error("nil config should fall through to live resolve, got empty")
	}
}

func TestResolveWorkspaceWithConfig_EmptyConfigFallsThrough(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "")
	cfg := &config.Config{}
	got := ResolveWorkspaceWithConfig("/tmp", cfg)
	if got == "" {
		t.Error("empty config should fall through to live resolve, got empty")
	}
}

func TestResolveWorkspaceWithConfig_EnvOverridesConfig(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wEnvOverride")
	cfg := &config.Config{Fleet: config.FleetConfig{HerdrWorkspace: "wConfig"}}
	got := ResolveWorkspaceWithConfig("/tmp", cfg)
	if got != "wEnvOverride" {
		t.Errorf("env should override config, got %q", got)
	}
}
