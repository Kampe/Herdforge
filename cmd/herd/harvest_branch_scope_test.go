package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func scopeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestReachableFromBranchScopesCandidates is the FAC-561 regression.
//
// Last-PASS selection was latest-wins across the WHOLE queue with no branch
// scoping, so with two candidates admitted together it reported the other ref's
// candidate: harvest-merge for reconstruct/cha-2185-fresh named CHA-2205's
// 1d5ce367acd4 while the branch's own candidate was 991ce0757eeb. Newest is not
// the same question as "on this branch".
func TestReachableFromBranchScopesCandidates(t *testing.T) {
	dir := t.TempDir()
	scopeGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "base"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scopeGit(t, dir, "add", "-A")
	scopeGit(t, dir, "commit", "-qm", "base")

	// Branch A: the branch being harvested.
	scopeGit(t, dir, "checkout", "-q", "-b", "reconstruct/cha-2185-fresh")
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scopeGit(t, dir, "add", "-A")
	scopeGit(t, dir, "commit", "-qm", "cha-2185 work")
	ownSHA := chomp(scopeGit(t, dir, "rev-parse", "HEAD"))

	// Branch B: a DIFFERENT ref whose candidate was admitted at the same time
	// and is newer.
	scopeGit(t, dir, "checkout", "-q", "main")
	scopeGit(t, dir, "checkout", "-q", "-b", "reconstruct/cha-2205")
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("b2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scopeGit(t, dir, "add", "-A")
	scopeGit(t, dir, "commit", "-qm", "cha-2205 work")
	otherSHA := chomp(scopeGit(t, dir, "rev-parse", "HEAD"))

	if ownSHA == otherSHA {
		t.Fatal("fixture must produce two distinct candidates")
	}
	// The branch's own candidate is reachable; the other ref's is not.
	if !reachableFromBranch(dir, ownSHA, "reconstruct/cha-2185-fresh") {
		t.Fatal("the branch's own candidate must be reachable from it")
	}
	if reachableFromBranch(dir, otherSHA, "reconstruct/cha-2185-fresh") {
		t.Fatal("another ref's candidate must NOT be reachable — this is the bug")
	}
	// And symmetrically, so the filter is not simply rejecting everything.
	if !reachableFromBranch(dir, otherSHA, "reconstruct/cha-2205") {
		t.Fatal("the other branch's candidate must be reachable from its own branch")
	}
}

func TestReachableFromBranchFailsClosedOnEmptyInput(t *testing.T) {
	dir := t.TempDir()
	if reachableFromBranch(dir, "", "b") || reachableFromBranch(dir, "sha", "") {
		t.Fatal("empty identity must not be reported reachable")
	}
}

func chomp(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// TestLastPassSelectionIsScopedToTheBranch drives the real selection path, not
// just the helper underneath it.
//
// This is the FAC-561 defect as reported: with two candidates admitted
// together, harvest-merge for one ref named the OTHER ref's candidate, because
// last-PASS selection was latest-wins across the whole queue. A test that only
// exercises reachableFromBranch would pass even if the selector never called
// it, so this asserts on the resolver's own report.
func TestLastPassSelectionIsScopedToTheBranch(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	gitCandidateTest(t, "init", "-q", "-b", "main")
	gitCandidateTest(t, "config", "user.email", "test@example.com")
	gitCandidateTest(t, "config", "user.name", "test")
	gitCandidateTest(t, "commit", "--allow-empty", "-q", "-m", "base")

	// The branch under harvest, reviewed FIRST.
	gitCandidateTest(t, "checkout", "-q", "-b", "reconstruct/cha-2185-fresh")
	writeCandidateFile(t, "own.go", "package own\n")
	gitCandidateTest(t, "add", "own.go")
	gitCandidateTest(t, "commit", "-q", "-m", "own work")
	ownSHA := gitCandidateOutput(t, "rev-parse", "HEAD")

	// A different ref, reviewed LATER, so latest-wins would pick it.
	gitCandidateTest(t, "checkout", "-q", "main")
	gitCandidateTest(t, "checkout", "-q", "-b", "reconstruct/cha-2205")
	writeCandidateFile(t, "other.go", "package other\n")
	gitCandidateTest(t, "add", "other.go")
	gitCandidateTest(t, "commit", "-q", "-m", "other work")
	otherSHA := gitCandidateOutput(t, "rev-parse", "HEAD")
	if ownSHA == otherSHA {
		t.Fatal("fixture must produce two distinct candidates")
	}

	l, err := reviewledger.NewReviewLedger(root, filepath.Join(root, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	addCandidatePass(t, l, ownSHA, "reconstruct/cha-2185-fresh")
	addCandidatePass(t, l, otherSHA, "reconstruct/cha-2205")

	got, err := resolveHarvestCandidateWithReconstructionAt(root, "reconstruct/cha-2185-fresh", "", "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.LastPassSHA == otherSHA {
		t.Fatalf("selected the other ref's candidate %s — this is the FAC-561 bug", otherSHA)
	}
	if got.LastPassSHA != ownSHA {
		t.Fatalf("last pass = %q, want this branch's own candidate %q", got.LastPassSHA, ownSHA)
	}
	// The suppressed candidate must be reported, never silently filtered.
	var reported bool
	for _, sha := range got.OffBranchQueued {
		if sha == otherSHA {
			reported = true
		}
	}
	if !reported {
		t.Errorf("off-branch candidate %s must be reported, got %v", otherSHA, got.OffBranchQueued)
	}

	// Symmetry: the other branch still resolves its own candidate, so the
	// filter is not simply rejecting everything.
	other, err := resolveHarvestCandidateWithReconstructionAt(root, "reconstruct/cha-2205", "", "", "")
	if err != nil {
		t.Fatalf("resolve other: %v", err)
	}
	if other.LastPassSHA != otherSHA {
		t.Fatalf("other branch last pass = %q, want %q", other.LastPassSHA, otherSHA)
	}
}

// Latest-WITHIN-branch must survive: repeated reviews of one branch
// legitimately supersede each other, and over-scoping would freeze a lane on
// its first-ever pass.
func TestLatestWithinBranchStillSupersedes(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	gitCandidateTest(t, "init", "-q", "-b", "main")
	gitCandidateTest(t, "config", "user.email", "test@example.com")
	gitCandidateTest(t, "config", "user.name", "test")
	gitCandidateTest(t, "commit", "--allow-empty", "-q", "-m", "base")
	gitCandidateTest(t, "checkout", "-q", "-b", "standing/lane")
	writeCandidateFile(t, "first.go", "package first\n")
	gitCandidateTest(t, "add", "first.go")
	gitCandidateTest(t, "commit", "-q", "-m", "first")
	first := gitCandidateOutput(t, "rev-parse", "HEAD")
	writeCandidateFile(t, "second.go", "package second\n")
	gitCandidateTest(t, "add", "second.go")
	gitCandidateTest(t, "commit", "-q", "-m", "second")
	second := gitCandidateOutput(t, "rev-parse", "HEAD")

	l, err := reviewledger.NewReviewLedger(root, filepath.Join(root, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	addCandidatePass(t, l, first, "standing/lane")
	addCandidatePass(t, l, second, "standing/lane")

	got, err := resolveHarvestCandidateWithReconstructionAt(root, "standing/lane", "", "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.LastPassSHA != second {
		t.Fatalf("last pass = %q, want the later same-branch pass %q", got.LastPassSHA, second)
	}
}
