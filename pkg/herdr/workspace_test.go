package herdr

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

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
