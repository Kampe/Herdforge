package harvest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestRebaseMergedWorkIsNotUnlanded is the FAC-576 regression.
//
// Pulse used ancestry (`git log origin/main..HEAD`) to decide a lane had work.
// A rebase-merge rewrites the SHA, so landed work is never an ancestor: the lane
// looked unlanded forever and pulse re-emitted an already-reviewed,
// already-merged candidate, producing a starvation loop on the supervisor.
func TestRebaseMergedWorkIsNotUnlanded(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base", "base\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	// Feature commit on a branch.
	gitRun(t, dir, "checkout", "-q", "-b", "feature")
	write("feat", "feature\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: real work")

	// Simulate a rebase-merge upstream: same patch, DIFFERENT SHA on main.
	//
	// An unrelated commit lands on main first so the cherry-pick has a different
	// parent. Without it the cherry-pick can produce a byte-identical commit
	// (same tree, parent, message and same-second timestamp) and therefore the
	// SAME SHA, which makes it a true ancestor and silently fails to reproduce
	// the bug. That nondeterminism made this test pass for the wrong reason.
	gitRun(t, dir, "checkout", "-q", "main")
	write("unrelated", "unrelated\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "chore: unrelated main commit")
	gitRun(t, dir, "cherry-pick", "feature")
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitRun(t, dir, "checkout", "-q", "feature")

	// Ancestry says unlanded; patch-equivalence says landed.
	ancestry, err := gitOutput(context.Background(), dir, "log", "--format=%s", "origin/main..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ancestry, "feat: real work") {
		t.Fatal("fixture must reproduce the ancestry false positive")
	}

	subjects, err := UnlandedSubjects(context.Background(), dir, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(SubstantiveSubjects(subjects)) != 0 {
		t.Fatalf("rebase-merged work must not count as unlanded, got %v", subjects)
	}
}

// Genuinely unmerged work must still be reported, or pulse would stop opening
// real reviews.
func TestGenuinelyUnmergedWorkIsReported(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: not yet merged")

	subjects, err := UnlandedSubjects(context.Background(), dir, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	got := SubstantiveSubjects(subjects)
	if len(got) != 1 || got[0] != "feat: not yet merged" {
		t.Fatalf("genuinely unmerged work must be reported, got %v", got)
	}
}

// Anchors and wip markers are bookkeeping, not reviewable work.
func TestBookkeepingCommitsAreNotWork(t *testing.T) {
	got := SubstantiveSubjects([]string{
		"chore: anchor FAC-1 worktree (FAC-106 reap-safe)",
		"wip: half done",
		"",
		"feat: real",
	})
	if len(got) != 1 || got[0] != "feat: real" {
		t.Fatalf("only substantive work counts, got %v", got)
	}
}
