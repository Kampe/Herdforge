package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// squashFixture builds a repository whose shape is the live FAC-805 repro: a
// worktree branch carries a multi-commit reviewed range, main squash-lands it,
// and main then continues to advance. Every git failure is fatal to the test,
// because a silently wrong fixture would prove nothing.
type squashFixture struct {
	t       *testing.T
	root    string
	branch  string
	dir     string
	baseRef string
}

func newSquashFixture(t *testing.T, branch string) *squashFixture {
	t.Helper()
	root := t.TempDir()
	f := &squashFixture{t: t, root: root, branch: branch, baseRef: "main"}
	f.git(root, "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o644)
	f.git(root, "add", ".")
	f.git(root, "commit", "-qm", "base")
	f.dir = filepath.Join(root, "wt-"+branch)
	f.git(root, "worktree", "add", "-q", "-b", branch, f.dir)
	return f
}

func (f *squashFixture) git(dir string, args ...string) {
	f.t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		f.t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
	}
}

// commit writes one commit on the branch with distinct content per step.
func (f *squashFixture) commit(step int) {
	f.t.Helper()
	os.WriteFile(filepath.Join(f.dir, "step"+string(rune('0'+step))+".txt"), []byte(strings.Repeat("c", step+1)+"\n"), 0o644)
	f.git(f.dir, "add", ".")
	f.git(f.dir, "commit", "-qm", "reviewed step")
}

// squashLands squash-merges the branch's whole range onto main as one commit,
// the shape GitHub produces for a squash merge.
func (f *squashFixture) squashLands() {
	f.t.Helper()
	f.git(f.root, "merge", "--squash", f.branch)
	f.git(f.root, "commit", "-qm", "squash the reviewed range (#PR)")
}

// mainAdvances adds an unrelated commit on main after the squash, so the
// base..landed range no longer holds exactly one commit.
func (f *squashFixture) mainAdvances() {
	f.t.Helper()
	os.WriteFile(filepath.Join(f.root, "unrelated.txt"), []byte("unrelated\n"), 0o644)
	f.git(f.root, "add", ".")
	f.git(f.root, "commit", "-qm", "unrelated advance")
}

func (f *squashFixture) entry(t *testing.T) worktreeEntry {
	t.Helper()
	entries, err := listWorktreeRegistrations(f.root)
	if err != nil {
		t.Fatalf("list registrations: %v", err)
	}
	for _, e := range entries {
		if e.Branch == f.branch {
			inspected := inspectWorktreeEntries([]worktreeEntry{e})
			return inspected[0]
		}
	}
	t.Fatalf("worktree for %s not registered", f.branch)
	return worktreeEntry{}
}

func (f *squashFixture) class(t *testing.T) reapRow {
	t.Helper()
	landed, kept := classifyReapEntries(f.root, f.baseRef, false, []worktreeEntry{f.entry(t)})
	for _, r := range landed {
		if r.Branch == f.branch {
			return r
		}
	}
	for _, r := range kept {
		if r.Branch == f.branch {
			return r
		}
	}
	t.Fatalf("branch %s was classified into neither landed nor kept", f.branch)
	return reapRow{}
}

func (f *squashFixture) assertClass(t *testing.T, want string) reapRow {
	t.Helper()
	row := f.class(t)
	if row.Class != want {
		t.Fatalf("class = %s (%s), want %s", row.Class, row.Reason, want)
	}
	return row
}

// A clean three-commit range squash-merged onto the base must be classified
// landed even though git cherry calls every one of its commits unique. This
// is the PR804 shape (harvest/fac804-task-binding, three commits, squash
// 22036f3a).
func TestWorktreeReapClassifiesCompleteSquashRangeAsLanded(t *testing.T) {
	f := newSquashFixture(t, "harvest-fac804")
	for i := 0; i < 3; i++ {
		f.commit(i)
	}
	f.squashLands()
	f.assertClass(t, "landed")
}

// Base drift is the live PR803 shape: the squash landed, then main advanced
// with an unrelated commit (PR804). The proof must pin the branch's own
// merge-base and still prove the range against it.
func TestWorktreeReapClassifiesSquashRangeAgainstDriftedBase(t *testing.T) {
	f := newSquashFixture(t, "harvest-fac803")
	for i := 0; i < 3; i++ {
		f.commit(i)
	}
	f.squashLands()
	f.mainAdvances()
	f.assertClass(t, "landed")
}

// One of three commits landed; two did not. The range is incomplete and the
// worktree holds the only copy of the missing patches.
func TestWorktreeReapKeepsPartiallyLandedRange(t *testing.T) {
	f := newSquashFixture(t, "partial")
	for i := 0; i < 3; i++ {
		f.commit(i)
	}
	os.WriteFile(filepath.Join(f.root, "step0.txt"), []byte("c\n"), 0o644)
	f.git(f.root, "add", ".")
	f.git(f.root, "commit", "-qm", "reviewed step")
	f.assertClass(t, "unmerged")
}

// The squash landed and the branch then gained another unique commit. The
// extra patch exists only on the branch, so the surface must be kept.
func TestWorktreeReapKeepsPostMergeChanges(t *testing.T) {
	f := newSquashFixture(t, "post-merge")
	for i := 0; i < 3; i++ {
		f.commit(i)
	}
	f.squashLands()
	os.WriteFile(filepath.Join(f.dir, "late.txt"), []byte("late\n"), 0o644)
	f.git(f.dir, "add", ".")
	f.git(f.dir, "commit", "-qm", "post-merge work")
	f.assertClass(t, "unmerged")
}

// A later main commit with the SAME SUBJECT as a reviewed commit but different
// content must not be mistaken for the landing. Subjects never enter the
// proof; patch identity and the replayed tree do.
func TestWorktreeReapKeepsSameSubjectDifferentContent(t *testing.T) {
	f := newSquashFixture(t, "same-subject")
	for i := 0; i < 2; i++ {
		f.commit(i)
	}
	f.squashLands()
	// Same subject as the branch's commits, different bytes.
	os.WriteFile(filepath.Join(f.root, "step0.txt"), []byte("DIFFERENT\n"), 0o644)
	f.git(f.root, "add", ".")
	f.git(f.root, "commit", "-qm", "reviewed step")
	f.assertClass(t, "unmerged")
}

// A failed git lookup must never read as landed or as unmerged-garbage: the
// worktree is kept with an unknown class and the exact failure.
func TestWorktreeReapKeepsWorktreeWhenGitLookupFails(t *testing.T) {
	f := newSquashFixture(t, "lookup-fail")
	for i := 0; i < 3; i++ {
		f.commit(i)
	}
	f.squashLands()
	landed, kept := classifyReapEntries(f.root, "refs/heads/no-such-ref", false, []worktreeEntry{f.entry(t)})
	if len(landed) != 0 {
		t.Fatalf("classified landed against a base that does not resolve: %v", landed)
	}
	for _, r := range kept {
		if r.Branch == f.branch && r.Class != "unknown" {
			t.Fatalf("class = %s (%s), want unknown on failed lookup", r.Class, r.Reason)
		}
	}
}

// The squash was reverted on main after classification. The historical proof
// still names the reverted merge, but the net content is gone from the tip;
// the act-time fence must re-prove landing against the current tip and refuse
// the removal. The worktree survives.
func TestRetireLandedRechecksLandingAtActFence(t *testing.T) {
	f := newSquashFixture(t, "reverted")
	for i := 0; i < 3; i++ {
		f.commit(i)
	}
	f.squashLands()
	f.assertClass(t, "landed")

	// Revert the squash on main after classification. rev-list main is
	// [base, squash] here, so the tip is the squash being reverted.
	out, err := exec.Command("git", "-C", f.root, "rev-list", "--reverse", "main").Output()
	if err != nil {
		t.Fatalf("rev-list main: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 2 {
		t.Fatalf("expected base plus squash on main, got %v", lines)
	}
	f.git(f.root, "revert", "--no-edit", lines[1])

	head, _ := exec.Command("git", "-C", f.dir, "rev-parse", "HEAD").Output()
	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(f.root, []reapRow{{Path: f.dir, Branch: f.branch, Head: strings.TrimSpace(string(head)), Class: "landed", Base: f.baseRef}}, clean)
	if len(retired) != 0 || len(failed) != 1 {
		t.Fatalf("reverted squash must be refused at the act fence: retired=%v failed=%v", retired, failed)
	}
	if !worktreeExists(f.dir) {
		t.Fatal("act-time landing recheck did not keep the worktree")
	}
	if !strings.Contains(failed[0]["error"], "act-time landing recheck") {
		t.Fatalf("failure must name the landing recheck, got: %s", failed[0]["error"])
	}
}

// A squash that landed and was later reverted must be classified unmerged at
// classification time, not only at the act fence: the historical proof still
// matches at the reverted merge point, but the net content is gone and the
// branch may hold the only copy. Only current-tip containment proves
// losslessness.
func TestWorktreeReapKeepsRevertedSquash(t *testing.T) {
	f := newSquashFixture(t, "revert-class")
	for i := 0; i < 3; i++ {
		f.commit(i)
	}
	f.squashLands()
	out, err := exec.Command("git", "-C", f.root, "rev-list", "--reverse", "main").Output()
	if err != nil {
		t.Fatalf("rev-list main: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 2 {
		t.Fatalf("expected base plus squash on main, got %v", lines)
	}
	f.git(f.root, "revert", "--no-edit", lines[1])
	f.assertClass(t, "unmerged")
}

// A hand-built row with no recorded base keeps the legacy fence semantics
// (head identity, cleanliness, owner census) without a landing recheck.
func TestRetireLandedWithoutBaseSkipsLandingRecheck(t *testing.T) {
	root := t.TempDir()
	run := func(a ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", root}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	run("add", ".")
	run("commit", "-qm", "base")
	dir := filepath.Join(root, "wt-legacy")
	run("worktree", "add", "-q", "-b", "legacy", dir)
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(root, []reapRow{{Path: dir, Branch: "legacy", Head: strings.TrimSpace(string(head)), Class: "landed"}}, clean)
	if len(retired) != 1 || len(failed) != 0 {
		t.Fatalf("legacy row must retire unchanged: retired=%v failed=%v", retired, failed)
	}
}
