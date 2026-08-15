package gc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestScanOverlap_EmptyRepo(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)
	wm := worktree.NewWorktreeManager(tmpDir)
	gcm := NewGCManager(tmpDir, wm)

	report, err := gcm.ScanOverlap(context.Background(), 1)
	if err != nil {
		t.Fatalf("expected clean scan, got err: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if len(report.OverlappingFiles) != 0 {
		t.Fatalf("expected no overlapping files in empty repo, got %v", report.OverlappingFiles)
	}
	if report.ScannedTips != 0 {
		t.Fatalf("expected 0 scanned tips in empty repo, got %d", report.ScannedTips)
	}
	if report.BaseRef == "" {
		t.Fatal("expected non-empty BaseRef — scan must resolve a base ref")
	}
}

func TestScanOverlap_NonGitDir(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	tmpDir := t.TempDir()
	wm := worktree.NewWorktreeManager(tmpDir)
	gcm := NewGCManager(tmpDir, wm)

	_, err := gcm.ScanOverlap(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
}

// TestScanOverlap_RealOverlap verifies that two unmerged branches editing the
// same file are surfaced in the report with both branch names.
func TestScanOverlap_RealOverlap(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	dir := initOverlapRepo(t)
	branchCommit(t, dir, "alpha", "pkg/shared.go", "package pkg\n// alpha\n")
	branchCommit(t, dir, "beta", "pkg/shared.go", "package pkg\n// beta\n")

	wm := worktree.NewWorktreeManager(dir)
	gcm := NewGCManager(dir, wm)
	report, err := gcm.ScanOverlap(context.Background(), 2)
	if err != nil {
		t.Fatalf("ScanOverlap: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if len(report.OverlappingFiles) != 1 {
		t.Fatalf("expected 1 overlapping file, got %d: %v", len(report.OverlappingFiles), report.OverlappingFiles)
	}
	branches := report.OverlappingFiles["pkg/shared.go"]
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches on shared.go, got %d: %v", len(branches), branches)
	}
	if report.ScannedTips != 2 {
		t.Fatalf("expected ScannedTips=2, got %d", report.ScannedTips)
	}
	if report.BaseRef != "origin/main" {
		t.Fatalf("expected BaseRef=origin/main, got %s", report.BaseRef)
	}
}

// TestScanOverlap_Disjoint verifies that two unmerged branches editing
// different files produce no overlap.
func TestScanOverlap_Disjoint(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	dir := initOverlapRepo(t)
	branchCommit(t, dir, "alpha", "pkg/alpha.go", "package pkg\n// alpha\n")
	branchCommit(t, dir, "beta", "pkg/beta.go", "package pkg\n// beta\n")

	wm := worktree.NewWorktreeManager(dir)
	gcm := NewGCManager(dir, wm)
	report, err := gcm.ScanOverlap(context.Background(), 2)
	if err != nil {
		t.Fatalf("ScanOverlap: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if len(report.OverlappingFiles) != 0 {
		t.Fatalf("expected 0 overlapping files for disjoint branches, got %d: %v", len(report.OverlappingFiles), report.OverlappingFiles)
	}
	if report.ScannedTips != 2 {
		t.Fatalf("expected ScannedTips=2, got %d", report.ScannedTips)
	}
}

// TestScanOverlap_DetachedNoPhantom verifies that a detached-HEAD worktree
// (no branch ref) does not manufacture a phantom overlap. The census reads
// refs/heads, not the worktree registry, so a detached HEAD is invisible.
func TestScanOverlap_DetachedNoPhantom(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	dir := initOverlapRepo(t)
	branchCommit(t, dir, "alpha", "pkg/shared.go", "package pkg\n// alpha\n")
	runGit(t, dir, "worktree", "add", "--detach", filepath.Join(dir, "detached"), "HEAD")

	wm := worktree.NewWorktreeManager(dir)
	gcm := NewGCManager(dir, wm)
	report, err := gcm.ScanOverlap(context.Background(), 2)
	if err != nil {
		t.Fatalf("ScanOverlap: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if len(report.OverlappingFiles) != 0 {
		t.Fatalf("detached worktree must not contribute phantom overlap, got %v", report.OverlappingFiles)
	}
	if report.ScannedTips != 1 {
		t.Fatalf("expected ScannedTips=1 (alpha only, detached excluded), got %d", report.ScannedTips)
	}
}

// TestScanOverlap_MissingWorktreeDir verifies that a deleted worktree directory
// (not yet pruned) does not break the scan. The branch ref still exists in
// refs/heads, so the overlap engine still counts it.
func TestScanOverlap_MissingWorktreeDir(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	dir := initOverlapRepo(t)

	runGit(t, dir, "worktree", "add", "-b", "alpha", filepath.Join(dir, "wt-alpha"), "HEAD")
	writeFile(t, filepath.Join(dir, "wt-alpha"), "pkg/shared.go", "package pkg\n// alpha\n")
	runGit(t, filepath.Join(dir, "wt-alpha"), "add", "pkg/shared.go")
	runGit(t, filepath.Join(dir, "wt-alpha"), "commit", "-m", "alpha edits shared")

	runGit(t, dir, "checkout", "main")
	runGit(t, dir, "worktree", "add", "-b", "beta", filepath.Join(dir, "wt-beta"), "HEAD")
	writeFile(t, filepath.Join(dir, "wt-beta"), "pkg/shared.go", "package pkg\n// beta\n")
	runGit(t, filepath.Join(dir, "wt-beta"), "add", "pkg/shared.go")
	runGit(t, filepath.Join(dir, "wt-beta"), "commit", "-m", "beta edits shared")
	runGit(t, dir, "checkout", "main")

	if err := os.RemoveAll(filepath.Join(dir, "wt-alpha")); err != nil {
		t.Fatal(err)
	}

	wm := worktree.NewWorktreeManager(dir)
	gcm := NewGCManager(dir, wm)
	report, err := gcm.ScanOverlap(context.Background(), 2)
	if err != nil {
		t.Fatalf("ScanOverlap: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	branches := report.OverlappingFiles["pkg/shared.go"]
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches on shared.go despite missing worktree dir, got %d: %v", len(branches), branches)
	}
}

// TestScanOverlap_MinTipsSemantics verifies that minTips acts as a strict
// threshold: two branches overlapping on one file with minTips=3 must not
// be reported as hot.
func TestScanOverlap_MinTipsSemantics(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	dir := initOverlapRepo(t)
	branchCommit(t, dir, "alpha", "pkg/shared.go", "package pkg\n// alpha\n")
	branchCommit(t, dir, "beta", "pkg/shared.go", "package pkg\n// beta\n")

	wm := worktree.NewWorktreeManager(dir)
	gcm := NewGCManager(dir, wm)
	report, err := gcm.ScanOverlap(context.Background(), 3)
	if err != nil {
		t.Fatalf("ScanOverlap: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if len(report.OverlappingFiles) != 0 {
		t.Fatalf("minTips=3 with only 2 tips must be empty, got %v", report.OverlappingFiles)
	}
	if report.ScannedTips != 2 {
		t.Fatalf("expected ScannedTips=2, got %d", report.ScannedTips)
	}
}

// TestScanOverlap_MissingBaseRef verifies that a repo with no recognizable
// base ref (no origin/main, no main, no master) returns a hard error instead
// of an empty success.
func TestScanOverlap_MissingBaseRef(t *testing.T) {
	t.Setenv("HERD_OVERLAP_MAIN_REF", "")
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "feature"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := testgit.Command(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0644)
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")

	wm := worktree.NewWorktreeManager(dir)
	gcm := NewGCManager(dir, wm)
	_, err := gcm.ScanOverlap(context.Background(), 2)
	if err == nil {
		t.Fatal("expected error when no base ref exists, got nil")
	}
}

func TestScanOverlap_NilManager(t *testing.T) {
	var gcm *GCManager
	_, err := gcm.ScanOverlap(context.Background(), 2)
	if err == nil {
		t.Fatal("expected error for nil manager")
	}
}

func TestScanOverlap_MinTipsZero(t *testing.T) {
	dir := initOverlapRepo(t)
	wm := worktree.NewWorktreeManager(dir)
	gcm := NewGCManager(dir, wm)
	_, err := gcm.ScanOverlap(context.Background(), 0)
	if err == nil {
		t.Fatal("expected error for minTips=0")
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := testgit.Command(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0644)
	cmd := testgit.Command(dir, "add", "README.md")
	cmd.Run()
	cmd = testgit.Command(dir, "commit", "-m", "initial")
	cmd.Run()
}

// initOverlapRepo creates a hermetic git repo with a main branch and an
// origin/main remote-tracking ref, ready for unmerged-branch overlap tests.
func initOverlapRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := testgit.Command(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
	}
	writeFile(t, dir, "README.md", "# test\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")
	return dir
}

// branchCommit creates branch name from main, writes file with content,
// commits, and checks out main again so the next branch starts from the
// same base.
func branchCommit(t *testing.T, dir, name, file, content string) {
	t.Helper()
	runGit(t, dir, "checkout", "-b", name)
	writeFile(t, dir, file, content)
	runGit(t, dir, "add", file)
	runGit(t, dir, "commit", "-m", name+" edits "+file)
	runGit(t, dir, "checkout", "main")
}

func writeFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
