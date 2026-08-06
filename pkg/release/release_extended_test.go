package release

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

func TestGenerateChangelog_WithFromTag(t *testing.T) {
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)

	// Create a tag so git log v0.0.1..HEAD works
	cmd := testgit.Command(tmpDir, "tag", "v0.0.1", "-m", "tag v0.0.1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag: %v, %s", err, out)
	}

	r := NewReleaseEngine(tmpDir)
	notes, md, err := r.GenerateChangelog(context.Background(), "v0.0.1", "v0.2.0")
	if err != nil {
		t.Fatalf("expected clean changelog, got err: %v", err)
	}
	if notes.Version != "v0.2.0" {
		t.Errorf("expected version v0.2.0, got %s", notes.Version)
	}
	if !strings.Contains(md, "v0.2.0") {
		t.Errorf("expected markdown to contain version")
	}
}

func TestGenerateChangelog_SortsByTypes(t *testing.T) {
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)
	// Add commits of each type
	makeCommit(t, tmpDir, "feat: new feature")
	makeCommit(t, tmpDir, "fix: bug fix")
	makeCommit(t, tmpDir, "refactor: clean up")

	r := NewReleaseEngine(tmpDir)
	notes, md, err := r.GenerateChangelog(context.Background(), "", "v0.3.0")
	if err != nil {
		t.Fatalf("expected clean changelog, got err: %v", err)
	}

	if len(notes.Features) != 1 {
		t.Errorf("expected 1 feature, got %d", len(notes.Features))
	}
	if len(notes.Fixes) != 1 {
		t.Errorf("expected 1 fix, got %d", len(notes.Fixes))
	}
	if len(notes.Refactors) != 1 {
		t.Errorf("expected 1 refactor, got %d", len(notes.Refactors))
	}
	if !strings.Contains(md, "🚀 Features") || !strings.Contains(md, "🐛 Bug Fixes") || !strings.Contains(md, "🧹 Refactoring") {
		t.Errorf("expected all sections in markdown")
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := testgit.Command(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
	}
	makeCommit(t, dir, "initial commit")
}

func makeCommit(t *testing.T, dir, msg string) {
	t.Helper()
	f, err := os.CreateTemp(dir, "commit-*.txt")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	f.Close()
	cmd := testgit.Command(dir, "add", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v, %s", err, out)
	}
	cmd = testgit.Command(dir, "commit", "-m", msg, "--allow-empty")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v, %s", err, out)
	}
}
