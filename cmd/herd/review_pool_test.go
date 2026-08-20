package main

import (
	"os"
	"path/filepath"
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
