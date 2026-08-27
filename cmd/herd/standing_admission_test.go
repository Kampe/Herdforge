package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

type standingAdmissionFixture struct {
	root string
	wt   string
}

func newStandingAdmissionFixture(t *testing.T) standingAdmissionFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, remote, "init", "--bare", "-q")
	runGitT(t, root, "init", "-q", "-b", "main")
	runGitT(t, root, "config", "user.email", "standing-admission@test.invalid")
	runGitT(t, root, "config", "user.name", "Standing Admission Test")
	runGitT(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	runGitT(t, root, "remote", "add", "origin", remote)
	runGitT(t, root, "push", "-q", "-u", "origin", "main")
	wt := filepath.Join(t.TempDir(), "standing")
	runGitT(t, root, "worktree", "add", "-q", "-b", "wt/standing", wt, "origin/main")
	return standingAdmissionFixture{root: root, wt: wt}
}

func (f standingAdmissionFixture) lane(t *testing.T) *config.LaneDef {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(f.root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	rel, err := filepath.Rel(f.root, f.wt)
	if err != nil {
		t.Fatal(err)
	}
	return &config.LaneDef{Name: "standing", Worktree: rel}
}

// This drives the production prepareStandingWorktree function used by the
// raise options, rather than the injected creation helper.
func TestPrepareStandingWorktreeReadmitsCleanStaleLane(t *testing.T) {
	f := newStandingAdmissionFixture(t)
	runGitT(t, f.root, "commit", "--allow-empty", "-q", "-m", "fresh main")
	runGitT(t, f.root, "push", "-q", "origin", "main")

	if err := prepareStandingWorktree(f.lane(t)); err != nil {
		t.Fatalf("raise-time worktree preparation refused a clean stale lane: %v", err)
	}
	if got, want := strings.TrimSpace(runGitT(t, f.wt, "rev-parse", "HEAD")), strings.TrimSpace(runGitT(t, f.wt, "rev-parse", "origin/main")); got != want {
		t.Fatalf("existing lane HEAD=%s want freshly admitted origin/main=%s", got, want)
	}
}

func TestPrepareStandingWorktreeRefusesDirtyStaleLaneWithoutDiscardingWork(t *testing.T) {
	f := newStandingAdmissionFixture(t)
	runGitT(t, f.root, "commit", "--allow-empty", "-q", "-m", "fresh main")
	runGitT(t, f.root, "push", "-q", "origin", "main")
	if err := os.WriteFile(filepath.Join(f.wt, "lane-work.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := prepareStandingWorktree(f.lane(t))
	if err == nil || !strings.Contains(err.Error(), "dirty worktree") || !strings.Contains(err.Error(), "ahead=0 behind=1") {
		t.Fatalf("dirty stale lane refusal=%v; want diagnostic with measured staleness", err)
	}
	if _, err := os.Stat(filepath.Join(f.wt, "lane-work.txt")); err != nil {
		t.Fatalf("admission discarded dirty lane work: %v", err)
	}
}

func TestPrepareStandingWorktreeRefusesAheadLaneWithoutRewrite(t *testing.T) {
	f := newStandingAdmissionFixture(t)
	if err := os.WriteFile(filepath.Join(f.wt, "lane-commit.txt"), []byte("keep committed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, f.wt, "add", "lane-commit.txt")
	runGitT(t, f.wt, "commit", "-q", "-m", "lane work")
	head := strings.TrimSpace(runGitT(t, f.wt, "rev-parse", "HEAD"))

	err := prepareStandingWorktree(f.lane(t))
	if err == nil || !strings.Contains(err.Error(), "unmerged lane commits") || !strings.Contains(err.Error(), "ahead=1 behind=0") {
		t.Fatalf("ahead lane refusal=%v; want diagnostic with measured staleness", err)
	}
	if got := strings.TrimSpace(runGitT(t, f.wt, "rev-parse", "HEAD")); got != head {
		t.Fatalf("admission rewrote lane HEAD=%s want preserved %s", got, head)
	}
}
