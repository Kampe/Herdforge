package reviewroot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rrGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestSameRootFromCheckoutAndWorktree is the FAC-572 gate.
//
// The handoff mailbox was anchored to the Git common root while review artifact
// roots were cwd-relative, so the shared checkout saw 63 artifacts and a
// supervisor worktree saw 255 with no outbox. A supervisor could inspect or
// ingest a different corpus than the one its queue referred to.
func TestSameRootFromCheckoutAndWorktree(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	rrGit(t, repo, "init", "-q", "-b", "main")
	rrGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")

	wt := filepath.Join(base, "wt")
	rrGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	// Env override must not mask the defect being tested.
	t.Setenv("HERD_ROOT", "")
	t.Setenv("HERD_REPO_ROOT", "")
	if err := os.Unsetenv("HERD_ROOT"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("HERD_REPO_ROOT"); err != nil {
		t.Fatal(err)
	}

	fromRepo := Resolve(repo)
	fromWorktree := Resolve(wt)
	if !fromRepo.Canonical || !fromWorktree.Canonical {
		t.Fatalf("both resolutions must be canonical: %+v %+v", fromRepo, fromWorktree)
	}
	if fromRepo.Root != fromWorktree.Root {
		t.Fatalf("the same project must resolve one review root:\n  checkout: %s\n  worktree: %s",
			fromRepo.Root, fromWorktree.Root)
	}
	// And every derived path follows, since those were the diverging ones.
	if fromRepo.Inbox() != fromWorktree.Inbox() || fromRepo.Outbox() != fromWorktree.Outbox() {
		t.Error("inbox and outbox must follow the one root")
	}
}

// A populated cwd-local root that is NOT canonical must be reported: that is
// the 255-artifact corpus a supervisor would otherwise act on unknowingly.
func TestNonEmptyDivergentRootIsReported(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	rrGit(t, repo, "init", "-q", "-b", "main")
	rrGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	wt := filepath.Join(base, "wt")
	rrGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	if err := os.Unsetenv("HERD_ROOT"); err != nil {
		t.Fatal(err)
	}

	// An EMPTY local root is noise, not a finding.
	if err := os.MkdirAll(filepath.Join(wt, Rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if p := Resolve(wt); p.Divergent != "" {
		t.Errorf("an empty local root must not be reported, got %q", p.Divergent)
	}

	// A POPULATED one is.
	if err := os.WriteFile(filepath.Join(wt, Rel, "verdict.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Resolve(wt)
	if p.Divergent == "" {
		t.Fatal("a non-empty divergent review root must be reported, not silently orphaned")
	}
	if !strings.Contains(p.Describe(), "WARNING") {
		t.Errorf("the description must surface the divergence: %s", p.Describe())
	}
}

// Outside a repository, resolution degrades but says so, rather than refusing to
// name any corpus at all.
func TestNonRepoResolutionIsMarkedNonCanonical(t *testing.T) {
	dir := t.TempDir()
	if err := os.Unsetenv("HERD_ROOT"); err != nil {
		t.Fatal(err)
	}
	p := Resolve(dir)
	if p.Root == "" {
		t.Fatal("a review root must always be named")
	}
	if p.Canonical {
		t.Error("a cwd-relative fallback must not claim to be canonical")
	}
	if !strings.Contains(p.Describe(), "NOT canonical") {
		t.Errorf("the degraded resolution must be visible: %s", p.Describe())
	}
}

// TestNoCwdRelativeReviewRootRemains is the mechanical half of FAC-572.
//
// The defect was not one bad path, it was the review root being resolved
// independently in several places. A gate on the literal keeps a fourth
// resolution from appearing and diverging again — the same reason the ancestry
// check and the pool option schema each ended up with one definition.
func TestNoCwdRelativeReviewRootRemains(t *testing.T) {
	root := repoRootForTest(t)
	guarded := map[string]bool{
		"cmd/herd/reviewingest.go":     true,
		"pkg/candidateindex/index.go":  true,
		"pkg/next/next.go":             true,
	}
	for file := range guarded {
		body, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			// A moved or renamed file is not a silent pass.
			t.Fatalf("guarded file %s is unreadable: %v", file, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			code := strings.TrimSpace(line)
			if code == "" || strings.HasPrefix(code, "//") {
				continue
			}
			// Building the review root from path segments rather than the
			// resolver is how the two corpora diverged.
			if strings.Contains(code, `".herd", "review"`) ||
				strings.Contains(code, `".herd/review`) {
				t.Errorf("%s:%d builds the review root directly; use reviewroot.Resolve: %s", file, i+1, code)
			}
		}
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
	}
	return strings.TrimSpace(string(out))
}
