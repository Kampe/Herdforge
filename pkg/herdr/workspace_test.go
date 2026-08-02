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

func TestResolveWorkspaceWithConfig_WinsOverFocused(t *testing.T) {
	// When config sets herdr_workspace and no env is set, config wins
	// regardless of what WorkspaceList would return.
	t.Setenv("HERD_WORKSPACE", "")
	cfg := &config.Config{Fleet: config.FleetConfig{HerdrWorkspace: "wF"}}
	got := ResolveWorkspaceWithConfig("some/repo", cfg)
	if got != "wF" {
		t.Errorf("ResolveWorkspaceWithConfig with config workspace = %q, want wF", got)
	}
}

func TestResolveWorkspaceWithConfig_EmptyConfigFallsThrough(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "")
	gotNil := ResolveWorkspaceWithConfig("some/repo", nil)
	if gotNil == "" {
		t.Error("ResolveWorkspaceWithConfig with nil config returned empty")
	}

	cfg := &config.Config{}
	gotEmpty := ResolveWorkspaceWithConfig("some/repo", cfg)
	if gotEmpty == "" {
		t.Error("ResolveWorkspaceWithConfig with empty config returned empty")
	}
}
