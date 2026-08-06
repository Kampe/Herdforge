package stash

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Hermetic: no inherited signing, no user identity, no UI helpers.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo returns a repo with one commit and a tracked file.
func newRepo(t *testing.T) (Repo, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "work")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "f.txt")
	run(t, dir, "commit", "-q", "-m", "base")
	return Repo{Dir: dir}, dir
}

func TestPushStoresInPrivateNamespaceAndRevertsWorktree(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := r.Push("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, NamespaceRoot+"/") {
		t.Fatalf("entry must live under the private namespace, got %q", ref)
	}
	// The shared stack must be untouched — that is the entire safety property.
	if shared := run(t, dir, "stash", "list"); shared != "" {
		t.Fatalf("shared refs/stash must stay empty, got %q", shared)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(body) != "base\n" {
		t.Fatalf("worktree must be reverted to HEAD, got %q", body)
	}
}

func TestPushNothingToSaveIsNotAnError(t *testing.T) {
	r, _ := newRepo(t)
	ref, err := r.Push("")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "" {
		t.Fatalf("clean worktree must store nothing, got %q", ref)
	}
}

func TestPopRestoresAndDropsApplyKeeps(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Push("scratch"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(false); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(body) != "dirty\n" {
		t.Fatalf("apply must restore the work, got %q", body)
	}
	if refs, _ := r.Entries(); len(refs) != 1 {
		t.Fatalf("apply must KEEP the entry, have %d", len(refs))
	}
	run(t, dir, "checkout", "HEAD", "--", "f.txt")
	if _, err := r.Apply(true); err != nil {
		t.Fatal(err)
	}
	if refs, _ := r.Entries(); len(refs) != 0 {
		t.Fatalf("pop must drop the entry, have %d", len(refs))
	}
}

// Lexical sorting would put /10 before /9 and pop the wrong entry.
func TestEntriesSortNumericallyNotLexically(t *testing.T) {
	r, dir := newRepo(t)
	for i := 0; i < 11; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Push("e"); err != nil {
			t.Fatal(err)
		}
	}
	newest, err := r.Newest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(newest, "/10") {
		t.Fatalf("newest must be entry 10, got %q", newest)
	}
}

// Two worktrees of the same repo must not see each other's entries.
func TestWorktreesDoNotShareEntries(t *testing.T) {
	r, dir := newRepo(t)
	sibling := filepath.Join(t.TempDir(), "lane-b")
	run(t, dir, "worktree", "add", "-q", "-b", "lane-b", sibling)

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("lane-a-wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Push("lane-a"); err != nil {
		t.Fatal(err)
	}

	other := Repo{Dir: sibling}
	refs, err := other.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("sibling worktree must not see lane-a's stash, got %v", refs)
	}
	if _, err := other.Newest(); err == nil {
		t.Fatal("sibling must report no entries rather than grabbing another lane's WIP")
	}
}

func TestSharedStackConflictIsDetected(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("racy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "stash", "push", "-m", "racy")
	hits := r.SharedStackConflict()
	if len(hits) == 0 {
		t.Fatal("entries on the shared stack for this branch must be reported")
	}
}
