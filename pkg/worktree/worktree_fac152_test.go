package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- ResolveCanonicalRoot ----------------------------------------------

func TestResolveCanonicalRoot_FromNestedPackageDir(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	nested := filepath.Join(tmpDir, "pkg", "dispatch")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveCanonicalRoot(context.Background(), nested, "")
	if err != nil {
		t.Fatalf("ResolveCanonicalRoot: %v", err)
	}
	want := normalizePath(tmpDir)
	if got != want {
		t.Fatalf("resolved root = %q, want %q (a naive cwd-string implementation would instead return %q)",
			got, want, normalizePath(nested))
	}
}

// TestResolveCanonicalRoot_FromInsideLinkedWorktree directly exercises the
// FAC-152 acceptance shape: "a compiled dispatch launched from
// <task-worktree>/pkg/dispatch creates the next task worktree only in the
// canonical pool." From deep inside a linked task worktree, the resolved
// root must be the SHARED repo root, never the task worktree's own path —
// otherwise a dispatch running from there computes its pool relative to
// itself, reproducing the fac-1 nesting.
func TestResolveCanonicalRoot_FromInsideLinkedWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-64")
	if err != nil {
		t.Fatalf("create source worktree: %v", err)
	}
	t.Cleanup(func() { _ = wm.RemoveWorktree(context.Background(), wi.Path) })

	nested := filepath.Join(wi.Path, "pkg", "dispatch")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveCanonicalRoot(context.Background(), nested, "")
	if err != nil {
		t.Fatalf("ResolveCanonicalRoot: %v", err)
	}
	want := normalizePath(tmpDir)
	if got != want {
		t.Fatalf("resolved root = %q, want shared repo root %q — dispatch would otherwise root its pool at the task worktree %q, reproducing the fac-1 shape",
			got, want, wi.Path)
	}
}

func TestResolveCanonicalRoot_OverrideShortCircuits(t *testing.T) {
	tmpDir := t.TempDir()
	got, err := ResolveCanonicalRoot(context.Background(), "/does/not/matter", tmpDir)
	if err != nil {
		t.Fatalf("ResolveCanonicalRoot: %v", err)
	}
	if got != normalizePath(tmpDir) {
		t.Fatalf("override root = %q, want %q", got, normalizePath(tmpDir))
	}
}

func TestResolveCanonicalRoot_NonRepoFails(t *testing.T) {
	tmpDir := t.TempDir() // no git init
	if _, err := ResolveCanonicalRoot(context.Background(), tmpDir, ""); err == nil {
		t.Fatal("expected error resolving canonical root outside a git checkout")
	}
}

// --- RejectContainedDestination -----------------------------------------

func TestRejectContainedDestination_AllowsCanonicalPool(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, ".herd", "worktrees", "fac-9")
	registered := []*WorktreeInfo{{Path: root, Branch: "main"}}
	if err := RejectContainedDestination(root, dest, registered); err != nil {
		t.Fatalf("expected canonical pool destination to be allowed: %v", err)
	}
}

func TestRejectContainedDestination_AllowsSiblingTaskWorktrees(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(root, ".herd", "worktrees", "fac-64")
	dest := filepath.Join(root, ".herd", "worktrees", "fac-65")
	registered := []*WorktreeInfo{{Path: root}, {Path: sibling, Branch: "herd/fac-64"}}
	if err := RejectContainedDestination(root, dest, registered); err != nil {
		t.Fatalf("sibling task worktrees under the pool must not collide: %v", err)
	}
}

func TestRejectContainedDestination_RejectsNestedInOtherWorktree(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(root, ".herd", "worktrees", "fac-64")
	dest := filepath.Join(other, "pkg", "dispatch", ".herd", "worktrees", "fac-1")
	registered := []*WorktreeInfo{{Path: root, Branch: "main"}, {Path: other, Branch: "herd/fac-64"}}

	err := RejectContainedDestination(root, dest, registered)
	if err == nil {
		t.Fatal("expected rejection: destination nested inside another registered worktree")
	}
	var cerr *ContainmentError
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *ContainmentError, got %T: %v", err, err)
	}
	if cerr.ContainedBy != other {
		t.Fatalf("ContainedBy = %q, want %q", cerr.ContainedBy, other)
	}
}

func TestRejectContainedDestination_RejectsOutsidePoolRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dest := filepath.Join(outside, "fac-9")
	if err := RejectContainedDestination(root, dest, []*WorktreeInfo{{Path: root}}); err == nil {
		t.Fatal("expected rejection: destination outside pool root")
	}
}

// TestRejectContainedDestination_SymlinkAlias proves the check follows a
// symlinked alias back to its real, already-registered target rather than
// comparing raw strings.
func TestRejectContainedDestination_SymlinkAlias(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(root, "other-wt")
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(other, alias); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	dest := filepath.Join(alias, "pkg", "dispatch", ".herd", "worktrees", "fac-1")
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}
	registered := []*WorktreeInfo{{Path: root, Branch: "main"}, {Path: other, Branch: "herd/fac-64"}}

	if err := RejectContainedDestination(root, dest, registered); err == nil {
		t.Fatal("expected containment rejection through a symlinked alias of a registered worktree")
	}
}

// TestRejectContainedDestination_CaseAlias proves the check catches a
// case-only alias of a registered worktree path on case-insensitive
// filesystems (macOS/Windows), where two differently-cased strings name the
// same on-disk directory.
func TestRejectContainedDestination_CaseAlias(t *testing.T) {
	if !caseInsensitiveFS() {
		t.Skip("case-insensitive filesystem only")
	}
	root := t.TempDir()
	other := filepath.Join(root, "Other-WT")
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "other-wt", "pkg", "dispatch", ".herd", "worktrees", "fac-1")
	if err := os.MkdirAll(dest, 0755); err != nil {
		t.Fatal(err)
	}
	registered := []*WorktreeInfo{{Path: root}, {Path: other, Branch: "herd/fac-64"}}

	if err := RejectContainedDestination(root, dest, registered); err == nil {
		t.Fatal("expected containment rejection through a case-only alias of a registered worktree")
	}
}

// --- CreateTaskWorktreeFrom containment gate (mutation-probe) -----------

// TestCreateTaskWorktreeFrom_RejectsPackageRelativeRepoRoot reproduces the
// exact FAC-64/fac-1 shape end to end: a WorktreeManager constructed with a
// RepoRoot resolved to a package-relative path inside an existing task
// worktree (the historical bug) must be refused before any git worktree
// add — not merely produce a nested worktree that gets caught later.
//
// This is the mutation-probe case named in the ticket: delete the
// containment gate call in CreateTaskWorktreeFrom (worktree.go) and this
// test starts succeeding at creating the nested worktree instead of
// failing.
func TestCreateTaskWorktreeFrom_RejectsPackageRelativeRepoRoot(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	rootWM := NewWorktreeManager(tmpDir)
	source, err := rootWM.CreateTaskWorktree(context.Background(), "FAC-64")
	if err != nil {
		t.Fatalf("create source worktree: %v", err)
	}
	t.Cleanup(func() { _ = rootWM.RemoveWorktree(context.Background(), source.Path) })

	beforeList, err := rootWM.ListWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a dispatch invocation whose RepoRoot resolved to a
	// package-relative cwd instead of the shared checkout.
	misrootedRoot := filepath.Join(source.Path, "pkg", "dispatch")
	if err := os.MkdirAll(misrootedRoot, 0755); err != nil {
		t.Fatal(err)
	}
	misrooted := &WorktreeManager{
		RepoRoot:    misrootedRoot,
		WorktreeDir: filepath.Join(misrootedRoot, ".herd", "worktrees"),
	}

	_, err = misrooted.CreateTaskWorktreeFrom(context.Background(), "FAC-1", "main")
	if err == nil {
		t.Fatal("expected containment rejection for a package-relative repo root")
	}
	var cerr *ContainmentError
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *ContainmentError, got %T: %v", err, err)
	}

	nestedPath := filepath.Join(misrootedRoot, ".herd", "worktrees", "fac-1")
	if _, statErr := os.Stat(nestedPath); statErr == nil {
		t.Fatal("nested worktree directory must not exist after rejection")
	}
	afterList, err := rootWM.ListWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterList) != len(beforeList) {
		t.Fatalf("git worktree list changed after a rejected create: before=%d after=%d", len(beforeList), len(afterList))
	}
}

// --- DetectNestedLanes ----------------------------------------------------

// TestDetectNestedLanes_FindsRegisteredNestedWorktree reproduces the live
// pkg/dispatch/.herd/worktrees/fac-1 shape: a linked worktree registered
// nested inside another task worktree, with uncommitted work. Detection
// must report it with owner/dirty evidence and leave both worktrees and
// the dirty file completely untouched.
func TestDetectNestedLanes_FindsRegisteredNestedWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	rootWM := NewWorktreeManager(tmpDir)
	source, err := rootWM.CreateTaskWorktree(context.Background(), "FAC-64")
	if err != nil {
		t.Fatalf("create source worktree: %v", err)
	}
	t.Cleanup(func() { _ = rootWM.RemoveWorktree(context.Background(), source.Path) })

	nestedDir := filepath.Join(source.Path, "pkg", "dispatch")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(nestedDir, "git", "worktree", "add", "-b", "herd/fac-1",
		filepath.Join(".herd", "worktrees", "fac-1"), "HEAD"); err != nil {
		t.Fatalf("create nested worktree (reproducing fac-1 shape): %v", err)
	}
	nestedPath := filepath.Join(nestedDir, ".herd", "worktrees", "fac-1")
	t.Cleanup(func() { _ = rootWM.RemoveWorktree(context.Background(), nestedPath) })
	if err := os.WriteFile(filepath.Join(nestedPath, "TASK-PACKET.md"), []byte("dirty work in progress"), 0644); err != nil {
		t.Fatal(err)
	}

	beforeList, err := rootWM.ListWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	lanes, err := rootWM.DetectNestedLanes(context.Background(), "main")
	if err != nil {
		t.Fatalf("DetectNestedLanes: %v", err)
	}
	if len(lanes) != 1 {
		t.Fatalf("expected exactly 1 nested lane, got %d: %+v", len(lanes), lanes)
	}
	nl := lanes[0]
	if normalizePath(nl.Path) != normalizePath(nestedPath) {
		t.Fatalf("nested lane Path = %q, want %q", nl.Path, nestedPath)
	}
	if normalizePath(nl.Owner) != normalizePath(source.Path) {
		t.Fatalf("nested lane Owner = %q, want %q", nl.Owner, source.Path)
	}
	if !nl.Dirty {
		t.Fatalf("expected nested lane to be reported dirty (untracked TASK-PACKET.md): %+v", nl)
	}
	if nl.Evidence == "" {
		t.Fatal("expected non-empty evidence string")
	}

	// No mutation: registration count and dirty file content are unchanged.
	afterList, err := rootWM.ListWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterList) != len(beforeList) {
		t.Fatalf("DetectNestedLanes mutated worktree registration: before=%d after=%d", len(beforeList), len(afterList))
	}
	content, err := os.ReadFile(filepath.Join(nestedPath, "TASK-PACKET.md"))
	if err != nil || string(content) != "dirty work in progress" {
		t.Fatalf("dirty TASK-PACKET.md must survive detection untouched: content=%q err=%v", content, err)
	}
}

func TestDetectNestedLanes_NoFalsePositiveOnNormalPool(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	rootWM := NewWorktreeManager(tmpDir)
	a, err := rootWM.CreateTaskWorktree(context.Background(), "FAC-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rootWM.RemoveWorktree(context.Background(), a.Path) })
	b, err := rootWM.CreateTaskWorktree(context.Background(), "FAC-2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rootWM.RemoveWorktree(context.Background(), b.Path) })

	lanes, err := rootWM.DetectNestedLanes(context.Background(), "main")
	if err != nil {
		t.Fatalf("DetectNestedLanes: %v", err)
	}
	if len(lanes) != 0 {
		t.Fatalf("expected no nested lanes for sibling task worktrees, got %+v", lanes)
	}
}
