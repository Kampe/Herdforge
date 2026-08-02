package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectSharedRoot(t *testing.T) {
	tmp := t.TempDir()
	if err := RejectSharedRoot(tmp, tmp); err == nil {
		t.Fatal("expected shared-root denial for identical paths")
	}
	wt := filepath.Join(tmp, ".herd", "worktrees", "fac-1")
	if err := RejectSharedRoot(tmp, wt); err != nil {
		t.Fatalf("nested worktree must be allowed: %v", err)
	}
	if err := RejectSharedRoot("", wt); err == nil {
		t.Fatal("empty repo root must fail")
	}
}

func TestCreateTaskWorktree_ImmutableOriginBase_IgnoresDirtyLocalMain(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	originSHA := gitOut(t, tmpDir, "rev-parse", "origin/main")
	if originSHA == "" {
		t.Fatal("origin/main missing after initRepo")
	}

	// Advance and dirty local main so HEAD != origin/main.
	if err := os.WriteFile(filepath.Join(tmpDir, "dirty.txt"), []byte("local-only"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(tmpDir, "git", "add", "dirty.txt"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(tmpDir, "git", "commit", "-m", "local ahead of origin"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "untracked.txt"), []byte("dirt"), 0644); err != nil {
		t.Fatal(err)
	}
	localMain := gitOut(t, tmpDir, "rev-parse", "main")
	if localMain == originSHA {
		t.Fatal("test setup failed: local main should diverge from origin/main")
	}

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-121")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	if wi.BaseSHA != originSHA {
		t.Fatalf("BaseSHA = %s, want origin/main %s", wi.BaseSHA, originSHA)
	}
	// Parent of the FAC-106 empty anchor must be the immutable base.
	parent := gitOut(t, wi.Path, "rev-parse", "HEAD^")
	if parent != originSHA {
		t.Fatalf("worktree parent = %s, want immutable origin/main %s (not local %s)", parent, originSHA, localMain)
	}
	// Must not contain local-only dirty.txt from the advanced main.
	if _, err := os.Stat(filepath.Join(wi.Path, "dirty.txt")); err == nil {
		t.Fatal("worktree based on local main; dirty.txt from advanced local HEAD must not appear")
	}
	if wi.Branch != "herd/fac-121" {
		t.Fatalf("Branch = %q, want herd/fac-121 (actual Git name)", wi.Branch)
	}
	if wi.AnchorRef != AnchorRefFor("FAC-121") {
		t.Fatalf("AnchorRef = %q", wi.AnchorRef)
	}
	anchorSHA := gitOut(t, tmpDir, "rev-parse", wi.AnchorRef)
	if anchorSHA == "" {
		t.Fatal("durable anchor ref missing")
	}
	// Anchor must be restorable after worktree removal.
	_ = runCmd(tmpDir, "git", "worktree", "remove", "--force", wi.Path)
	if gitOut(t, tmpDir, "rev-parse", "--verify", wi.AnchorRef) == "" {
		t.Fatal("anchor ref must survive worktree removal")
	}
}

func TestCreateTaskWorktree_RecordsActualGitBranch(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-42")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	actual := gitOut(t, wi.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if wi.Branch != actual {
		t.Fatalf("recorded branch %q != git branch %q", wi.Branch, actual)
	}
	if wi.Branch != TaskBranch("FAC-42") {
		t.Fatalf("branch = %q, want %q", wi.Branch, TaskBranch("FAC-42"))
	}
}

func TestCreateTaskWorktree_ReattachBranchWithoutWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-RE")
	if err != nil {
		t.Fatalf("initial create: %v", err)
	}
	branch := wi.Branch
	base := wi.BaseSHA
	// Remove only the working tree; keep the branch (simulates reap of path).
	if err := runCmd(tmpDir, "git", "worktree", "remove", "--force", wi.Path); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if gitOut(t, tmpDir, "rev-parse", "--verify", branch) == "" {
		t.Fatal("branch should survive worktree remove")
	}

	wi2, err := wm.CreateTaskWorktree(context.Background(), "FAC-RE")
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi2.Path) })
	if wi2.Branch != branch {
		t.Fatalf("reattached branch = %q, want %q", wi2.Branch, branch)
	}
	if wi2.BaseSHA != base {
		t.Fatalf("reattached BaseSHA = %q, want %q", wi2.BaseSHA, base)
	}
	if _, err := os.Stat(filepath.Join(wi2.Path, ".git")); err != nil {
		t.Fatalf("reattached path missing .git: %v", err)
	}
}

func TestCreateTaskWorktree_RejectsWhenTargetIsRepoRoot(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	// Force WorktreeDir such that task path would equal repo root — impossible
	// with normal Join, so call RejectSharedRoot via a manager aimed at root.
	wm := &WorktreeManager{RepoRoot: tmpDir, WorktreeDir: tmpDir}
	// CreateTaskWorktree joins WorktreeDir with lowercased ref, so path != root
	// unless ref is empty. Empty ref is rejected.
	if _, err := wm.CreateTaskWorktree(context.Background(), ""); err == nil {
		t.Fatal("empty task ref must fail")
	}
	// Direct API check already covered; also ensure attachExisting denies root.
	if _, err := wm.attachExisting(context.Background(), tmpDir, "herd/x", "abc", AnchorRefFor("x")); err == nil {
		t.Fatal("attachExisting on repo root must fail closed")
	}
}

func TestResolveImmutableBase_RequiresOrigin(t *testing.T) {
	tmpDir := t.TempDir()
	// Minimal repo without origin
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "t@t.com"},
		{"git", "config", "user.name", "T"},
		{"git", "config", "commit.gpgsign", "false"},
	} {
		if err := runCmd(tmpDir, args[0], args[1:]...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = runCmd(tmpDir, "git", "add", "f")
	_ = runCmd(tmpDir, "git", "commit", "-m", "i")
	_ = runCmd(tmpDir, "git", "branch", "-M", "main")

	wm := NewWorktreeManager(tmpDir)
	if _, err := wm.ResolveImmutableBase(context.Background(), "main"); err == nil {
		t.Fatal("expected error when origin/main is missing")
	}
}

func TestAnchorRefFor(t *testing.T) {
	if got := AnchorRefFor("FAC-121"); got != "refs/herd/anchors/fac-121" {
		t.Fatalf("got %q", got)
	}
}

// Ensure gitOut helper is available when only this file is run.
func TestGitOutHelper(t *testing.T) {
	tmp := t.TempDir()
	initRepo(t, tmp)
	if gitOut(t, tmp, "rev-parse", "--is-inside-work-tree") != "true" {
		t.Fatal("expected inside work tree")
	}
}

// crash-point: update-ref failure must not leave a half-created branch claim.
func TestCreateTaskWorktree_CrashPoint_AnchorRefFailure(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	// Make update-ref fail by replacing git with a wrapper via PATH is heavy;
	// instead mock execCommandContext to fail only on update-ref.
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	calls := 0
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		// Let fetch/rev-parse/origin work via real git; fail update-ref.
		if name == "git" && len(args) > 0 && args[0] == "update-ref" {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, name, args...)
	}

	wm := NewWorktreeManager(tmpDir)
	_, err := wm.CreateTaskWorktree(context.Background(), "FAC-CRASH")
	if err == nil {
		t.Fatal("expected anchor update-ref failure")
	}
	if !strings.Contains(err.Error(), "durable anchor") && !strings.Contains(err.Error(), "update-ref") {
		t.Fatalf("unexpected error: %v", err)
	}
	// No worktree path should have been created for the task.
	path := filepath.Join(tmpDir, ".herd", "worktrees", "fac-crash")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("partial worktree must not exist after anchor failure")
	}
}
