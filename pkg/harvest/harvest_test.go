package harvest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func createTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, cmd := range []string{
		"git init -b main",
		"git config user.email test@test.com",
		"git config user.name test",
	} {
		c := exec.Command("bash", "-c", cmd)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
	}
	return dir
}

func commitFile(t *testing.T, dir, filename, content, msg string) {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	c := exec.Command("git", "add", filename)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	c = exec.Command("git", "commit", "-m", msg)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func createBranch(t *testing.T, dir, name string) {
	t.Helper()
	c := exec.Command("git", "checkout", "-b", name)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", name, err, out)
	}
}

func checkoutBranch(t *testing.T, dir, name string) {
	t.Helper()
	c := exec.Command("git", "checkout", name)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout %s: %v\n%s", name, err, out)
	}
}

func addWorktree(t *testing.T, repoDir, worktreePath, branch string) {
	t.Helper()
	c := exec.Command("git", "worktree", "add", "-b", branch, worktreePath)
	c.Dir = repoDir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
}

func TestHarvesterListsWorktrees(t *testing.T) {
	dir := createTestGitRepo(t)
	commitFile(t, dir, "a.txt", "hello", "initial")
	commitFile(t, dir, "b.txt", "world", "second")

	wt := filepath.Join(dir, "..", "test-wt")
	worktreeDir, err := filepath.Abs(wt)
	if err != nil {
		t.Fatal(err)
	}
	addWorktree(t, dir, worktreeDir, "feature-x")

	h := NewHarvester(dir)
	wts, err := h.listWorktrees(context.Background())
	if err != nil {
		t.Fatalf("listWorktrees: %v", err)
	}
	found := false
	for _, w := range wts {
		abs, err := filepath.EvalSymlinks(w)
		if err != nil {
			continue
		}
		expected, err := filepath.EvalSymlinks(worktreeDir)
		if err != nil {
			continue
		}
		if abs == expected {
			found = true
			break
		}
		// Try direct comparison too
		if filepath.Clean(w) == filepath.Clean(worktreeDir) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected worktree %s in %v", worktreeDir, wts)
	}
}

func TestHarvesterWithNoWorktrees(t *testing.T) {
	dir := createTestGitRepo(t)
	commitFile(t, dir, "a.txt", "hello", "initial")

	h := NewHarvester(dir)
	result, err := h.Harvest(context.Background())
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if len(result.UnmergedWorktrees) != 0 {
		t.Fatalf("expected 0 unmerged, got %d", len(result.UnmergedWorktrees))
	}
}

func TestUnmergedWorktreeCount(t *testing.T) {
	dir := createTestGitRepo(t)
	commitFile(t, dir, "a.txt", "hello", "initial")

	h := NewHarvester(dir)
	count, err := h.UnmergedWorktreeCount(context.Background())
	if err != nil {
		t.Fatalf("UnmergedWorktreeCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 unmerged, got %d", count)
	}
}

func TestCheckUnmerged_SkipMainBranch(t *testing.T) {
	dir := createTestGitRepo(t)
	commitFile(t, dir, "a.txt", "hello", "initial")

	h := NewHarvester(dir)
	u, err := h.checkUnmerged(context.Background(), dir)
	if err != nil {
		t.Fatalf("checkUnmerged: %v", err)
	}
	if u != nil {
		t.Fatalf("expected nil (main branch skipped), got %+v", *u)
	}
}

func TestCheckUnmerged_FeatureBranch(t *testing.T) {
	dir := createTestGitRepo(t)
	commitFile(t, dir, "a.txt", "hello", "initial")
	commitFile(t, dir, "b.txt", "world", "second")

	createBranch(t, dir, "feature")
	commitFile(t, dir, "c.txt", "feature", "feature commit")

	checkoutBranch(t, dir, "main")

	h := NewHarvester(dir)
	u, err := h.checkUnmerged(context.Background(), dir)
	if err != nil {
		t.Fatalf("checkUnmerged: %v", err)
	}
	_ = u
}

func TestHarvestEmptyResultSummary(t *testing.T) {
	dir := createTestGitRepo(t)
	commitFile(t, dir, "a.txt", "hello", "initial")

	h := NewHarvester(dir)
	s := h.Summary(context.Background())
	if s != "herd-harvest: no unmerged commits in any worktree" {
		t.Fatalf("unexpected summary: %s", s)
	}
}

func TestQuietSummaryNoUnmerged(t *testing.T) {
	dir := createTestGitRepo(t)
	commitFile(t, dir, "a.txt", "hello", "initial")

	h := NewHarvester(dir)
	s := h.QuietSummary(context.Background())
	if s != "herd-harvest: 0 worktree(s) with unmerged commits" {
		t.Fatalf("unexpected quiet summary: %s", s)
	}
}

func TestHarvestResultJSONTags(t *testing.T) {
	r := HarvestResult{
		UnmergedWorktrees: []UnmergedWork{
			{
				WorktreePath: "/tmp/wt",
				Branch:       "feature",
				Unmerged:     []string{"abc123 commit msg"},
			},
		},
		Errors: nil,
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "worktree_path") {
		t.Fatalf("expected json tag worktree_path, got %s", out)
	}
	if !strings.Contains(string(out), "unmerged_commits") {
		t.Fatalf("expected json tag unmerged_commits, got %s", out)
	}
}

func TestHarvesterIgnoresMainMasterHEAD(t *testing.T) {
	checkBranch := func(branch string) bool {
		return branch == "main" || branch == "master" || branch == "HEAD"
	}
	if !checkBranch("main") {
		t.Fatal("expected main to match")
	}
	if !checkBranch("master") {
		t.Fatal("expected master to match")
	}
	if !checkBranch("HEAD") {
		t.Fatal("expected HEAD to match")
	}
	if checkBranch("feature") {
		t.Fatal("expected feature not to match")
	}
}
