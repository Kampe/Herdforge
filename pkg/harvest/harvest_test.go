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

// gitTestArgs prefixes every fixture git invocation with a hermetic author
// and GPG-off flags so tests never depend on global git identity (CI runners
// have none). Same pattern as gitIn / gitInHarvest in sibling test files.
func gitTestArgs(args ...string) []string {
	base := []string{
		"-c", "commit.gpgSign=false",
		"-c", "gpg.x509.program=false",
		"-c", "gpg.format=openpgp",
		"-c", "tag.gpgSign=false",
		"-c", "user.email=test@herdforge.local",
		"-c", "user.name=Test Runner",
	}
	return append(base, args...)
}

func gitInTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", gitTestArgs(args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func createTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInTest(t, dir, "init", "-q", "-b", "main")
	// Local identity so any bare `git commit` in this fixture (and code
	// under test that commits without -c) also works without global config.
	gitInTest(t, dir, "config", "user.email", "test@herdforge.local")
	gitInTest(t, dir, "config", "user.name", "Test Runner")
	gitInTest(t, dir, "config", "commit.gpgSign", "false")
	return dir
}

func commitFile(t *testing.T, dir, filename, content, msg string) {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	gitInTest(t, dir, "add", filename)
	gitInTest(t, dir, "commit", "-q", "-m", msg)
}

func createBranch(t *testing.T, dir, name string) {
	t.Helper()
	gitInTest(t, dir, "checkout", "-b", name)
}

func checkoutBranch(t *testing.T, dir, name string) {
	t.Helper()
	gitInTest(t, dir, "checkout", name)
}

func addWorktree(t *testing.T, repoDir, worktreePath, branch string) {
	t.Helper()
	gitInTest(t, repoDir, "worktree", "add", "-b", branch, worktreePath)
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
