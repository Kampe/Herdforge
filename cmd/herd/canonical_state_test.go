package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalStateRootFromLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main")
	runGitT(t, root, "config", "user.email", "test@example.com")
	runGitT(t, root, "config", "user.name", "canonical-state-test")
	runGitT(t, root, "commit", "--allow-empty", "-qm", "base")
	linked := filepath.Join(t.TempDir(), "lane")
	runGitT(t, root, "worktree", "add", "-q", "-b", "lane", linked)

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(linked); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	got, err := canonicalHerdRoot()
	if err != nil {
		t.Fatalf("canonicalHerdRoot: %v", err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("canonicalHerdRoot from linked worktree = %q, want %q", got, want)
	}
	if gotWinddown := winddownStatePath(); gotWinddown != filepath.Join(want, defaultWinddownStatePath) {
		t.Fatalf("winddownStatePath from linked worktree = %q, want root-scoped path", gotWinddown)
	}
}
