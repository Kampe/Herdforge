package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeReviewSurfacePart(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "ticket", ref: "FAC-435", want: "fac-435"},
		{name: "slashes and punctuation", ref: "review/CHA-12#r2", want: "review-cha-12-r2"},
		{name: "repeated separators", ref: "review//standing---nft", want: "review-standing-nft"},
		{name: "empty", ref: "!!!", want: "candidate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeReviewSurfacePart(tt.ref); got != tt.want {
				t.Fatalf("safeReviewSurfacePart(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestReviewNamesAreStableBoundedAndDistinct(t *testing.T) {
	sha := "c3c368c84bb02ea604a2b29f112dbe231b6159a3"
	refs := []string{
		"standing/nft-data-engineer",
		"standing/nft-data-engineer-variant",
	}
	for _, ref := range refs {
		name := reviewAgentName(ref, sha)
		if len(name) > reviewAgentNameLimit || name == "" {
			t.Fatalf("reviewAgentName(%q) = %q, length %d; want 1-%d characters", ref, name, len(name), reviewAgentNameLimit)
		}
		if name != reviewAgentName(ref, sha) {
			t.Fatalf("reviewAgentName(%q) is not stable: %q", ref, name)
		}
		if name[0] == '-' || name[len(name)-1] == '-' || strings.Contains(name, "--") {
			t.Fatalf("reviewAgentName(%q) has invalid dash shape: %q", ref, name)
		}
	}
	if got := reviewAgentName(refs[0], sha); got == reviewAgentName(refs[1], sha) {
		t.Fatalf("long refs collided after truncation: %q", got)
	}
	if got := reviewTabLabel(refs[0], sha); len(got) > reviewAgentNameLimit {
		t.Fatalf("reviewTabLabel length = %d, want <= %d", len(got), reviewAgentNameLimit)
	}
}

func TestReviewPoolMode(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{args: []string{"FAC-1", "--pool"}, want: true},
		{args: []string{"--pool=true", "FAC-1"}, want: true},
		{args: []string{"FAC-1", "--spawn"}, want: false},
	} {
		if got := reviewPoolMode(tt.args); got != tt.want {
			t.Fatalf("reviewPoolMode(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestResolvePoolReviewCandidateTicketAndBranchWorktrees(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-b", "main")
	runGitT(t, root, "config", "user.email", "test@example.invalid")
	runGitT(t, root, "config", "user.name", "Herdforge test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", "README.md")
	runGitT(t, root, "commit", "-m", "fixture")

	ticketWorktree := filepath.Join(root, ".herd", "worktrees", "fac-123")
	runGitT(t, root, "worktree", "add", "--detach", ticketWorktree, "HEAD")
	if got, err := resolvePoolReviewCandidate(root, "FAC-123"); err != nil || got != ticketWorktree {
		t.Fatalf("ticket candidate = %q, %v; want %q", got, err, ticketWorktree)
	}

	runGitT(t, root, "branch", "goal/review")
	branchWorktree := filepath.Join(root, "goal-review-worktree")
	runGitT(t, root, "worktree", "add", branchWorktree, "goal/review")
	got, err := resolvePoolReviewCandidate(root, "goal/review")
	if err != nil {
		t.Fatalf("branch candidate: %v", err)
	}
	gotReal, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	wantReal, err := filepath.EvalSymlinks(branchWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if gotReal != wantReal {
		t.Fatalf("branch candidate = %q, want %q", gotReal, wantReal)
	}
}

func TestParseReviewPoolArgs(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name    string
		args    []string
		wantRef string
		wantSHA string
		wantErr bool
	}{
		{name: "selector after ref", args: []string{"FAC-478", "--pool", "--sha", sha}, wantRef: "FAC-478", wantSHA: sha},
		{name: "unknown flag", args: []string{"FAC-478", "--pool", "--not-a-review-option"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRef, gotSHA, err := parseReviewPoolArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("unknown flag was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotRef != tt.wantRef || gotSHA != tt.wantSHA {
				t.Fatalf("parseReviewPoolArgs(%v) = (%q, %q), want (%q, %q)", tt.args, gotRef, gotSHA, tt.wantRef, tt.wantSHA)
			}
		})
	}
}

// FAC-648: a DETACHED worktree sitting at the exact SHA is a legitimate candidate.
// Pool.Ensure creates slots with `git worktree add --detach` and the remote
// launcher prepares its surface the same way, but resolution accepted only a
// porcelain `branch refs/heads/<ref>` line -- so every branch-style remote review
// failed with "candidate branch is not checked out in a worktree" against a
// surface already at exactly the right commit. Requiring a branch was never the
// safety property: the pool resets --hard to the SHA regardless.
func TestResolvePoolCandidateAcceptsDetachedSurfaceAtExactSHA(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
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
	run("branch", "feat/deep/candidate")

	shaOut, err := exec.Command("git", "-C", root, "rev-parse", "refs/heads/feat/deep/candidate").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))

	// The launcher's sanitized directory name, checked out DETACHED -- exactly
	// what the remote launcher and Pool.Ensure produce.
	surface := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart("feat/deep/candidate"))
	run("worktree", "add", "-q", "--detach", surface, sha)

	got, err := resolvePoolReviewCandidateAt(root, "feat/deep/candidate", sha)
	if err != nil {
		t.Fatalf("a detached surface at the exact SHA must resolve: %v", err)
	}
	if got != surface {
		t.Fatalf("resolved %q, want the prepared surface %q", got, surface)
	}
}

// The SHA is VERIFIED, not assumed: a detached surface at the wrong commit must
// not be accepted just because its directory name matches.
func TestResolvePoolCandidateRejectsDetachedSurfaceAtWrongSHA(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
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
	first, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "second")
	second, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()

	surface := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart("feat/deep/candidate"))
	run("worktree", "add", "-q", "--detach", surface, strings.TrimSpace(string(first)))

	// Ask for the SECOND sha; the surface holds the first.
	if _, err := resolvePoolReviewCandidateAt(root, "feat/deep/candidate", strings.TrimSpace(string(second))); err == nil {
		t.Fatal("a surface at the wrong commit must not resolve; the SHA is verified, not assumed")
	}
}
