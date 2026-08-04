package lost

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
)

var commitSeq int64

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(dir, args...)
	// Unique timestamps per commit: two --allow-empty commits with the same
	// subject, parent, and second-resolution time produce the IDENTICAL sha
	// (no content to differ), silently collapsing test branches together.
	// Only commit-signing salt hid this on the dev machine.
	seq := atomic.AddInt64(&commitSeq, 1)
	stamp := time.Unix(1_700_000_000+seq*7, 0).UTC().Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture builds a repo with origin/main plus branches covering every
// classification arm of the zsh decision tree.
func fixture(t *testing.T) string {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "t@h.local")
	run(t, dir, "config", "user.name", "t")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: base work")
	run(t, dir, "update-ref", "refs/remotes/origin/main", run(t, dir, "rev-parse", "HEAD"))
	return dir
}

func find(t *testing.T, dir string) *LostReport {
	f := NewFinder(dir)
	f.Fetch = false
	rep, err := f.Find(context.Background())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	return rep
}

func TestNoMainErrors(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	f := NewFinder(dir)
	f.Fetch = false
	if _, err := f.Find(context.Background()); err != ErrNoMain {
		t.Fatalf("want ErrNoMain, got %v", err)
	}
}

func TestCleanRepo(t *testing.T) {
	rep := find(t, fixture(t))
	if len(rep.Lost) != 0 || rep.LostTotal != 0 {
		t.Fatalf("clean repo must lose nothing: %+v", rep)
	}
}

func TestLocalOrphanIsLost(t *testing.T) {
	dir := fixture(t)
	run(t, dir, "branch", "dead-lane")
	run(t, dir, "checkout", "-q", "dead-lane")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: orphaned work")
	run(t, dir, "checkout", "-q", "main")
	rep := find(t, dir)
	if len(rep.Lost) != 1 || rep.Lost[0].Branch != "dead-lane" || rep.Lost[0].Unmerged != 1 {
		t.Fatalf("want dead-lane lost with 1 subject: %+v", rep.Lost)
	}
}

func TestRemoteOnlyOrphanIsLost(t *testing.T) {
	dir := fixture(t)
	run(t, dir, "checkout", "-q", "-b", "tmp")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: remote-only work")
	sha := run(t, dir, "rev-parse", "HEAD")
	run(t, dir, "checkout", "-q", "main")
	run(t, dir, "branch", "-D", "tmp")
	run(t, dir, "update-ref", "refs/remotes/origin/ghost", sha)
	rep := find(t, dir)
	if len(rep.Lost) != 1 || rep.Lost[0].Label != "origin/ghost" {
		t.Fatalf("remote-only orphan must be lost: %+v", rep.Lost)
	}
}

func TestSubjectSurvivesRebase(t *testing.T) {
	dir := fixture(t)
	// Branch carries subject X; main later carries the SAME SUBJECT with a
	// different sha (rebase merge). Subject comparison → superseded.
	run(t, dir, "checkout", "-q", "-b", "old-lane")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: rebased work")
	run(t, dir, "checkout", "-q", "main")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: rebased work")
	run(t, dir, "update-ref", "refs/remotes/origin/main", run(t, dir, "rev-parse", "HEAD"))
	rep := find(t, dir)
	if len(rep.Lost) != 0 {
		t.Fatalf("rebased-in subject must not be lost: %+v", rep.Lost)
	}
	if len(rep.Superseded) != 1 || rep.Superseded[0] != "old-lane" {
		t.Fatalf("want old-lane superseded: %+v", rep.Superseded)
	}
}

func TestLiveWorktreeIsOwnedNotLost(t *testing.T) {
	dir := fixture(t)
	run(t, dir, "worktree", "add", "-q", "-b", "lane", dir+"/wt-lane")
	run(t, dir+"/wt-lane", "commit", "--allow-empty", "-q", "-m", "feat: lane work in flight")
	rep := find(t, dir)
	if len(rep.Lost) != 0 {
		t.Fatalf("owned branch must never be lost: %+v", rep.Lost)
	}
	if len(rep.Owned) != 1 || rep.Owned[0].Branch != "lane" {
		t.Fatalf("want lane owned: %+v", rep.Owned)
	}
}

func TestParkedIsNeverOwnerless(t *testing.T) {
	dir := fixture(t)
	run(t, dir, "checkout", "-q", "-b", "park/big-refactor")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: parked work")
	run(t, dir, "checkout", "-q", "main")
	rep := find(t, dir)
	if len(rep.Lost) != 0 || len(rep.Parked) != 1 {
		t.Fatalf("park/ must be parked, never lost: %+v", rep)
	}
}

func TestInfraBranchInvisible(t *testing.T) {
	dir := fixture(t)
	run(t, dir, "checkout", "-q", "-b", "herd/allocators")
	for i := 0; i < 3; i++ {
		run(t, dir, "commit", "--allow-empty", "-q", "-m", "herd: reserve "+strings.Repeat("i", i+1))
	}
	run(t, dir, "checkout", "-q", "main")
	rep := find(t, dir)
	if len(rep.Lost)+len(rep.Parked)+len(rep.Owned) != 0 {
		t.Fatalf("infra branch must appear in no row: %+v", rep)
	}
}

func TestLimitCapsSubjects(t *testing.T) {
	dir := fixture(t)
	run(t, dir, "checkout", "-q", "-b", "busy")
	for i := 0; i < 5; i++ {
		run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: work "+strings.Repeat("x", i+1))
	}
	run(t, dir, "checkout", "-q", "main")
	f := NewFinder(dir)
	f.Fetch = false
	f.Limit = 2
	rep, err := f.Find(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Lost) != 1 || rep.Lost[0].Total != 2 || rep.Lost[0].Unmerged != 2 {
		t.Fatalf("limit must cap examination: %+v", rep.Lost)
	}
}

func TestRemoteTwinDeduplicated(t *testing.T) {
	dir := fixture(t)
	run(t, dir, "checkout", "-q", "-b", "twin")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "feat: twin work")
	sha := run(t, dir, "rev-parse", "HEAD")
	run(t, dir, "checkout", "-q", "main")
	run(t, dir, "update-ref", "refs/remotes/origin/twin", sha)
	rep := find(t, dir)
	if len(rep.Lost) != 1 {
		t.Fatalf("twin must be reported exactly once: %+v", rep.Lost)
	}
}
