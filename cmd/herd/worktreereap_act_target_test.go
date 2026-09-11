package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

func TestRetireLandedInspectsOnlySelectedTargetsAtActTime(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	runGitT(t, root, "commit", "--allow-empty", "-qm", "base")
	a := reapBatchWorktree(t, root, "selected-a")
	b := reapBatchWorktree(t, root, "selected-b")
	other := reapBatchWorktree(t, root, "unrelated")
	if err := os.WriteFile(filepath.Join(other.Path, "keep.txt"), []byte("uncommitted evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	original := reapStatusRunner
	t.Cleanup(func() { reapStatusRunner = original })
	calls := make(map[string]int)
	reapStatusRunner = func(path string, args ...string) (string, error) {
		calls[canonicalPathT(t, path)]++
		return original(path, args...)
	}
	inspector := &reapBatchInspectorFunc{fn: func(_ context.Context, paths []string) (map[string]resources.ProcessUsage, error) {
		usage := make(map[string]resources.ProcessUsage, len(paths))
		for _, path := range paths {
			usage[path] = resources.ProcessUsage{}
		}
		return usage, nil
	}}
	retired, failed := retireLandedWithInspector(root, []reapRow{a, b}, inspector)
	if len(retired) != 2 || len(failed) != 0 {
		t.Fatalf("selected clean worktrees must retire: retired=%v failed=%v", retired, failed)
	}
	if len(calls) != 2 || calls[canonicalPathT(t, a.Path)] != 1 || calls[canonicalPathT(t, b.Path)] != 1 {
		t.Fatalf("act must inspect each selected target exactly once and no unrelated worktrees: %v", calls)
	}
	if worktreeExists(a.Path) || worktreeExists(b.Path) {
		t.Fatal("successful retirement left a selected worktree behind")
	}
	data, err := os.ReadFile(filepath.Join(other.Path, "keep.txt"))
	if err != nil || string(data) != "uncommitted evidence" {
		t.Fatalf("unrelated work must remain intact: data=%q err=%v", data, err)
	}
}

func TestRetireLandedSelectedStatusErrorStillProtectsWorktree(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	runGitT(t, root, "commit", "--allow-empty", "-qm", "base")
	row := reapBatchWorktree(t, root, "selected")
	original := reapStatusRunner
	t.Cleanup(func() { reapStatusRunner = original })
	calls := 0
	reapStatusRunner = func(path string, args ...string) (string, error) {
		if canonicalPathT(t, path) != canonicalPathT(t, row.Path) {
			t.Fatalf("unexpected status target: %s", path)
		}
		calls++
		return "", errors.New("selected status unavailable")
	}
	err := retireLandedOne(root, row, runReapGit)
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "selected status unavailable") {
		t.Fatalf("unreadable selected status must refuse with its cause: calls=%d err=%v", calls, err)
	}
	if !worktreeExists(row.Path) {
		t.Fatal("status failure removed the worktree")
	}
	if got := strings.TrimSpace(runGitT(t, root, "rev-parse", "refs/heads/"+row.Branch)); got != row.Head {
		t.Fatalf("status failure changed branch: got=%s want=%s", got, row.Head)
	}
}
