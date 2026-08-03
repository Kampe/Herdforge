package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// repoRoot resolves the repository root from this test file's own path so
// the real-roster tests below work regardless of `go test`'s CWD.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this file's path")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// canonicalRoles are the fleet roles FAC-127 requires the roster to cover.
var canonicalRoles = []string{
	"orchestrator", "scout-planner", "worker", "forge-smith",
	"verification-gate", "reviewer", "review-supervisor",
	"harvest", "recovery-sentinel",
}

// TestRealRoster_CompleteAndPromptFilesExist loads the actual .herd/herd.yaml
// and proves: every canonical role has a lane, and every lane's prompt path
// resolves to a real file on disk (not just a non-empty string).
func TestRealRoster_CompleteAndPromptFilesExist(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		t.Fatalf("failed to load real .herd/herd.yaml: %v", err)
	}

	declared := make(map[string]bool, len(cfg.Lanes))
	for _, lane := range cfg.Lanes {
		declared[lane.Role] = true
		promptPath := filepath.Join(root, lane.Prompt)
		if _, err := os.Stat(promptPath); err != nil {
			t.Errorf("lane %q: prompt file does not exist: %s", lane.Name, lane.Prompt)
		}
	}
	for _, role := range canonicalRoles {
		if !declared[role] {
			t.Errorf("canonical role %q has no lane registered in .herd/herd.yaml", role)
		}
	}
}

// TestRealRoster_AllPromptsRegisteredOrTemplated proves every contract file
// under .herd/prompts is either wired to a lane or explicitly named here as
// a one-off template, so a new prompt can never silently go unregistered.
func TestRealRoster_AllPromptsRegisteredOrTemplated(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		t.Fatalf("failed to load real .herd/herd.yaml: %v", err)
	}

	registered := make(map[string]bool, len(cfg.Lanes))
	for _, lane := range cfg.Lanes {
		registered[filepath.Base(lane.Prompt)] = true
	}
	// Explicitly documented one-off templates — not standing role contracts.
	templates := map[string]bool{
		"critical-wave-coordinator.md": true,
	}

	entries, err := os.ReadDir(filepath.Join(root, ".herd", "prompts"))
	if err != nil {
		t.Fatalf("failed to read .herd/prompts: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !registered[e.Name()] && !templates[e.Name()] {
			t.Errorf(".herd/prompts/%s has no lane schema entry and is not documented as a template in this test", e.Name())
		}
	}
}
