package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-832: pool preparation allocated a detached carrier under .herd/worktrees
// on EVERY resolution, including the case where the caller had already pinned
// the exact candidate sha and the exact review base. Nothing opened that
// directory — the reviewer works in the leased pool slot, and the review
// surface is a symlink to that slot — so it outlived the reviewer with no
// retirement owner: --no-launch records no manifest, and worktree-reap
// deliberately keeps detached review-pool surfaces as transient. Four merged,
// clean carriers were left behind after PR #844.
//
// These drive the production allocation decision itself. Discovery of an
// existing worktree is unchanged and still preferred; only speculative
// creation is withheld.

// carrierRepo is a real isolated git repository with one commit and a
// candidate branch, and NO worktree holding the candidate.
func carrierRepo(t *testing.T) (root, sha string) {
	t.Helper()
	root = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	run("branch", "feat/carrier/candidate")
	out, err := exec.Command("git", "-C", root, "rev-parse", "refs/heads/feat/carrier/candidate").Output()
	if err != nil {
		t.Fatal(err)
	}
	return root, strings.TrimSpace(string(out))
}

// managedWorktrees lists the carriers under .herd/worktrees, which is where the
// orphans accumulated.
func managedWorktrees(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".herd", "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// registeredWorktreeCount is git's own view, so a carrier that was registered
// but whose directory was later removed still counts.
func registeredWorktreeCount(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	return strings.Count(string(out), "worktree ")
}

// A caller that pinned BOTH identities allocates nothing.
func TestPinnedCandidateAndBaseAllocateNoCarrier(t *testing.T) {
	root, sha := carrierRepo(t)
	before := registeredWorktreeCount(t, root)

	dir, err := resolvePoolReviewCandidateAtFor(root, "feat/carrier/candidate", sha, false)
	if err != nil {
		t.Fatalf("a fully pinned resolution must not fail: %v", err)
	}
	if dir != "" {
		t.Fatalf("a fully pinned resolution allocated a carrier at %q", dir)
	}
	if got := managedWorktrees(t, root); len(got) != 0 {
		t.Fatalf("a fully pinned resolution left an unowned carrier: %v", got)
	}
	if after := registeredWorktreeCount(t, root); after != before {
		t.Fatalf("registered worktrees went from %d to %d; a carrier was registered", before, after)
	}
}

// The pool entry asks for exactly that when both identities are pinned, and
// asks for a directory whenever one of them is missing.
func TestNeedsCandidateDirectoryFollowsWhatIsActuallyRead(t *testing.T) {
	sha := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, sha, base string
		want            bool
	}{
		{"both pinned reads nothing", sha, base, false},
		{"no sha needs the HEAD read", "", base, true},
		{"no base needs the task context", sha, "", true},
		{"neither", "", "", true},
		{"whitespace is not a pin", "   ", "   ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsCandidateDirectory(tc.sha, tc.base); got != tc.want {
				t.Fatalf("needsCandidateDirectory(%q,%q) = %v, want %v", tc.sha, tc.base, got, tc.want)
			}
		})
	}
}

// PRESERVED: when the base is not pinned the task context must be readable, so
// the carrier is still prepared exactly as FAC-678 intended.
func TestUnpinnedBaseStillPreparesTheCandidateCarrier(t *testing.T) {
	root, sha := carrierRepo(t)

	dir, err := resolvePoolReviewCandidateAtFor(root, "feat/carrier/candidate", sha, true)
	if err != nil {
		t.Fatalf("preparation must still work when a directory is needed: %v", err)
	}
	if dir == "" {
		t.Fatal("no carrier was prepared when one was required")
	}
	if got := managedWorktrees(t, root); len(got) != 1 {
		t.Fatalf("managed worktrees = %v, want exactly the prepared carrier", got)
	}
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("prepared carrier is not a worktree: %v", err)
	}
	if strings.TrimSpace(string(head)) != sha {
		t.Fatalf("prepared carrier HEAD = %s, want the exact candidate %s", strings.TrimSpace(string(head)), sha)
	}
}

// DISCOVERY IS UNCHANGED: an existing worktree at the exact candidate is still
// found and returned, without preparing anything, so pinning both identities
// never hides a surface that is genuinely there.
func TestExistingCandidateWorktreeIsStillUsedWhenNothingMayBePrepared(t *testing.T) {
	root, sha := carrierRepo(t)
	existing := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart("feat/carrier/candidate"))
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "--detach", existing, sha).CombinedOutput(); err != nil {
		t.Fatalf("fixture worktree: %v\n%s", err, out)
	}
	before := registeredWorktreeCount(t, root)

	dir, err := resolvePoolReviewCandidateAtFor(root, "feat/carrier/candidate", sha, false)
	if err != nil {
		t.Fatalf("discovery must still work: %v", err)
	}
	if dir != existing {
		t.Fatalf("resolved %q, want the existing surface %q", dir, existing)
	}
	if after := registeredWorktreeCount(t, root); after != before {
		t.Fatalf("discovery registered an extra worktree: %d -> %d", before, after)
	}
}

// RETRY IS IDEMPOTENT and leaves nothing behind: repeating the fully pinned
// resolution must keep allocating nothing.
func TestRepeatedPinnedResolutionStaysAllocationFree(t *testing.T) {
	root, sha := carrierRepo(t)
	before := registeredWorktreeCount(t, root)
	for i := 0; i < 3; i++ {
		dir, err := resolvePoolReviewCandidateAtFor(root, "feat/carrier/candidate", sha, false)
		if err != nil || dir != "" {
			t.Fatalf("attempt %d: dir=%q err=%v, want no carrier and no error", i, dir, err)
		}
	}
	if got := managedWorktrees(t, root); len(got) != 0 {
		t.Fatalf("repeated resolution accumulated carriers: %v", got)
	}
	if after := registeredWorktreeCount(t, root); after != before {
		t.Fatalf("repeated resolution registered worktrees: %d -> %d", before, after)
	}
}

// A candidate that does not resolve still refuses, and still allocates nothing.
func TestUnresolvableCandidateAllocatesNothingAndRefuses(t *testing.T) {
	root, _ := carrierRepo(t)
	missing := strings.Repeat("d", 40)

	if _, err := resolvePoolReviewCandidateAtFor(root, "feat/carrier/candidate", missing, true); err == nil {
		t.Fatal("an unresolvable candidate must refuse")
	}
	if got := managedWorktrees(t, root); len(got) != 0 {
		t.Fatalf("a refused resolution left a carrier: %v", got)
	}
}
