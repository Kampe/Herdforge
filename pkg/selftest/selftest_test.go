package selftest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSelfTestRunner_RunSuite(t *testing.T) {
	st := NewSelfTestRunner("../..")
	results, err := st.RunSuite(context.Background())
	if err != nil {
		t.Fatalf("expected clean selftest suite run, got err: %v", err)
	}

	if len(results) < 3 {
		t.Errorf("expected at least 3 assertion results, got %d", len(results))
	}
}

func TestRunSuite_AssertionFailure(t *testing.T) {
	// Using a nonexistent repo root triggers preflight_boundary_check failure
	st := NewSelfTestRunner("/nonexistent")
	results, err := st.RunSuite(context.Background())
	if err == nil {
		t.Fatal("expected error from assertion failure")
	}
	if len(results) == 0 {
		t.Errorf("expected at least 1 result, got 0")
	}
}

func TestRunSuite_UsesConfiguredAbsolutePathAllowlist(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := []byte(`
version: "1"
project:
  name: "selftest-allowlist"
task_provider:
  type: "memory"
worktree_boundary:
  allowed_absolute_paths:
    - "docs/operational-notes.md"
`)
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(string(filepath.Separator), "Users", "operator", "shared-worktree")
	if err := os.WriteFile(filepath.Join(root, "docs", "operational-notes.md"), []byte("checkout: "+path+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := NewSelfTestRunner(root).RunSuite(context.Background())
	if err != nil {
		t.Fatalf("configured allowlist should be honored by selftest: %v", err)
	}
	if len(results) == 0 || !results[0].Passed {
		t.Fatalf("preflight boundary assertion did not pass: %+v", results)
	}
}
