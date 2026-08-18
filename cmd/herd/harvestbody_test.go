package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

// FAC-212: a harvest whose cherry-picked commits produce no diff against the
// base must be refused — a merge that changes no bytes is not a completed
// ticket. PR #151 merged with 0 additions, 0 deletions, 0 files because the
// branch held only its anchor commit, and the adversarial reviewer returned
// PASS because an empty diff has nothing wrong with it.
//
// This test creates a worktree off main with no commits to cherry-pick,
// producing an empty diff — the exact shape PR #151 had after its redundant
// anchor was skipped.
func TestHarvestBodyRefusesEmptyDiff(t *testing.T) {
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "-q", "--initial-branch=main")
	run("config", "user.name", "Herdforge Test")
	run("config", "user.email", "herdforge-test@example.invalid")
	run("config", "commit.gpgSign", "false")
	run("config", "gpg.program", "")
	run("config", "core.hooksPath", "./.herd-test-no-hooks-do-not-create")
	run("commit", "-q", "--allow-empty", "-m", "main: initial commit")

	// harvestBody runs `git worktree add` in the CWD (no -C flag on that
	// command), so the test must chdir into the repo.
	t.Chdir(dir)

	workdir := filepath.Join(dir, ".herd", "worktrees", "harvest-test")
	t.Cleanup(func() {
		testgit.Command(dir, "worktree", "remove", "--force", workdir).Run()
		testgit.Command(dir, "branch", "-D", "harvest/test").Run()
	})

	// No commits to cherry-pick: the worktree is off main with nothing
	// applied, so `git diff main...HEAD` is empty.
	err := harvestBody(workdir, "main", "harvest/test", nil, false)

	if err == nil {
		t.Fatal("an empty-diff harvest must be refused, but harvestBody returned nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("refusal must name the empty diff, got: %v", err)
	}
}

// The positive path: a harvest with real content must pass the empty-diff
// gate. Without this the test above could pass simply because harvestBody
// always fails.
func TestHarvestBodyAcceptsNonEmptyDiff(t *testing.T) {
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "-q", "--initial-branch=main")
	run("config", "user.name", "Herdforge Test")
	run("config", "user.email", "herdforge-test@example.invalid")
	run("config", "commit.gpgSign", "false")
	run("config", "gpg.program", "")
	run("config", "core.hooksPath", "./.herd-test-no-hooks-do-not-create")
	run("commit", "-q", "--allow-empty", "-m", "main: initial commit")

	// A branch with a real content change — its diff against main is non-empty.
	run("branch", "feature")
	run("checkout", "-q", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "feature.go")
	run("commit", "-q", "-m", "feat: add feature file")
	run("checkout", "-q", "main")

	// harvestBody runs `git worktree add` in the CWD (no -C flag on that
	// command), so the test must chdir into the repo.
	t.Chdir(dir)

	head, err := testgit.Command(dir, "rev-parse", "feature").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	commitSHA := strings.TrimSpace(string(head))

	workdir := filepath.Join(dir, ".herd", "worktrees", "harvest-test")
	t.Cleanup(func() {
		testgit.Command(dir, "worktree", "remove", "--force", workdir).Run()
		testgit.Command(dir, "branch", "-D", "harvest/test").Run()
	})

	err = harvestBody(workdir, "main", "harvest/test", []string{commitSHA}, false)
	if err != nil {
		t.Fatalf("a non-empty-diff harvest must pass the empty-diff gate: %v", err)
	}
}

func TestHarvestBodyUnionMergesConfiguredAppendOnlyFile(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "--initial-branch=main")
	run("config", "user.name", "Herdforge Test")
	run("config", "user.email", "herdforge-test@example.invalid")
	run("config", "commit.gpgSign", "false")
	run("config", "core.hooksPath", "./.herd-test-no-hooks-do-not-create")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "plan.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "docs/plan.md")
	run("commit", "-q", "-m", "main: plan")
	run("branch", "lane-a")
	run("checkout", "-q", "lane-a")
	if err := os.WriteFile(filepath.Join(dir, "docs", "plan.md"), []byte("base\nA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "lane: A")
	a, err := testgit.Command(dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	run("checkout", "-q", "main")
	run("branch", "lane-b")
	run("checkout", "-q", "lane-b")
	if err := os.WriteFile(filepath.Join(dir, "docs", "plan.md"), []byte("base\nB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "lane: B")
	b, err := testgit.Command(dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	run("checkout", "-q", "main")
	t.Chdir(dir)
	workdir := filepath.Join(dir, ".herd", "worktrees", "harvest-union")
	t.Cleanup(func() {
		testgit.Command(dir, "worktree", "remove", "--force", workdir).Run()
		testgit.Command(dir, "branch", "-D", "harvest/union").Run()
	})
	if err := harvestBody(workdir, "main", "harvest/union", []string{strings.TrimSpace(string(a)), strings.TrimSpace(string(b))}, false, []string{"docs/plan.md"}); err != nil {
		t.Fatalf("configured append-only conflict must resolve: %v", err)
	}
	merged, err := os.ReadFile(filepath.Join(workdir, "docs", "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(merged) != "base\nA\nB\n" {
		t.Fatalf("union merge = %q", merged)
	}
}
