package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestWorktreeMigrateIsDryRunByDefaultThenMovesLosslessly(t *testing.T) {
	repo := t.TempDir()
	migrationGit(t, repo, "init", "-q")
	migrationGit(t, repo, "config", "user.email", "test@example.invalid")
	migrationGit(t, repo, "config", "user.name", "Lifecycle Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	migrationGit(t, repo, "add", "README.md")
	migrationGit(t, repo, "commit", "-qm", "base")
	legacy := filepath.Join(repo, ".worktrees", "legacy-lane")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	migrationGit(t, repo, "worktree", "add", "-q", "-b", "legacy/lane", legacy)
	if err := os.WriteFile(filepath.Join(legacy, "uncommitted.txt"), []byte("preserve me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_ROOT", repo)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var dryOut, errOut bytes.Buffer
	if err := runWorktreeMigrate(nil, &dryOut, &errOut); err != nil {
		t.Fatalf("dry-run migration: %v (%s)", err, errOut.String())
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("dry run moved legacy worktree: %v", err)
	}
	if !strings.Contains(dryOut.String(), "DRY RUN") {
		t.Fatalf("dry run did not identify itself: %s", dryOut.String())
	}

	var applyOut bytes.Buffer
	if err := runWorktreeMigrate([]string{"--apply"}, &applyOut, &errOut); err != nil {
		t.Fatalf("apply migration: %v (%s)", err, errOut.String())
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy path remains after migration: %v", err)
	}
	stateRoot := worktree.StateWorktreeRoot(repo)
	entries, err := worktree.NewWorktreeManager(repo).ListWorktrees(t.Context())
	if err != nil {
		t.Fatalf("list migrated worktrees: %v", err)
	}
	var moved string
	for _, entry := range entries {
		if entry != nil && withinMigrationRoot(filepath.Join(stateRoot, "legacy"), entry.Path) {
			moved = entry.Path
			break
		}
	}
	if moved == "" {
		t.Fatalf("no registration moved below %q", stateRoot)
	}
	if content, err := os.ReadFile(filepath.Join(moved, "uncommitted.txt")); err != nil || string(content) != "preserve me\n" {
		t.Fatalf("uncommitted work was not preserved: content=%q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "records", "legacy", filepath.Base(moved)+".json")); err != nil {
		t.Fatalf("migration did not write lifecycle record: %v", err)
	}
}

func migrationGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
