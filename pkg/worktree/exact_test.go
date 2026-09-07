package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

func exactFixture(t *testing.T) (*WorktreeManager, string, string) {
	t.Helper()
	root := t.TempDir()
	initRepo(t, root)
	w := NewWorktreePool(root, filepath.Join(root, ".herd", "integration-worktrees"))
	// Fixture disk capacity is deterministic. Native callers keep the real gate.
	w.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		return resources.DiskDecision{Allowed: true}
	})
	sha, err := w.revParse(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return w, filepath.Join(w.WorktreeDir, "candidate"), sha
}

func TestFAC601ExactWorktreeUsesReviewedCommitAndResumes(t *testing.T) {
	for _, branchAlreadyCreated := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "crash_after_ref"}[branchAlreadyCreated], func(t *testing.T) {
			w, target, sha := exactFixture(t)
			ctx := context.Background()
			branch := "integration/fac-601"
			if branchAlreadyCreated {
				if err := w.updateRef(ctx, "refs/heads/"+branch, sha); err != nil {
					t.Fatal(err)
				}
			}
			// Caller HEAD advances; a worktree created from HEAD is now wrong.
			if err := runCmd(w.RepoRoot, "git", "commit", "--allow-empty", "-m", "unrelated later main"); err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 2; n++ {
				got, err := w.CreateExactWorktree(ctx, branch, target, sha)
				if err != nil || got == nil || got.Commit != sha || got.Branch != branch {
					t.Fatalf("exact attachment %d: %+v %v", n, got, err)
				}
			}
			if err := runCmd(target, "git", "diff", "--exit-code", sha); err != nil {
				t.Fatal("checkout differs from reviewed content:", err)
			}
		})
	}
}

func TestFAC601ExactWorktreeRefusesStaleBranchWithoutReset(t *testing.T) {
	w, target, sha := exactFixture(t)
	if err := runCmd(w.RepoRoot, "git", "commit", "--allow-empty", "-m", "other content identity"); err != nil {
		t.Fatal(err)
	}
	other, err := w.revParse(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	branch := "integration/stale"
	if err := w.updateRef(context.Background(), "refs/heads/"+branch, other); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateExactWorktree(context.Background(), branch, target, sha); err == nil {
		t.Fatal("stale branch was accepted")
	}
	current, err := w.revParse(context.Background(), "refs/heads/"+branch)
	if err != nil || current != other {
		t.Fatal("stale branch was reset")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("refused branch produced a checkout")
	}
}

func TestFAC601ExactWorktreeRefusesUnregisteredAndDirtyDestinations(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "unregistered", true: "registered_dirty"}[registered], func(t *testing.T) {
			w, target, sha := exactFixture(t)
			if registered {
				if _, err := w.CreateExactWorktree(context.Background(), "integration/owned", target, sha); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(target, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(target, "uncommitted.txt")
			if err := os.WriteFile(path, []byte("preserve me"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := w.CreateExactWorktree(context.Background(), "integration/owned", target, sha); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != "preserve me" {
				t.Fatal("refusal destroyed retained work")
			}
		})
	}
}

func TestFAC601ExactWorktreeRefusesNestedSymlinkAndForeignBinding(t *testing.T) {
	w, target, sha := exactFixture(t)
	ctx := context.Background()
	if _, err := w.CreateExactWorktree(ctx, "integration/source", target, sha); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(w.WorktreeDir, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateExactWorktree(ctx, "integration/nested", filepath.Join(alias, "nested"), sha); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("nested symlink admitted: %v", err)
	}
	if _, err := w.CreateExactWorktree(ctx, "integration/source", filepath.Join(w.WorktreeDir, "other"), sha); err == nil {
		t.Fatal("branch already attached elsewhere was reused")
	}
	if _, err := w.CreateExactWorktree(ctx, "integration/wrong", target, sha); err == nil {
		t.Fatal("foreign branch at destination was accepted")
	}
}

func TestFAC601ExactWorktreeDiskDenialCreatesNoRefOrDirectory(t *testing.T) {
	w, target, sha := exactFixture(t)
	w.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		return resources.DiskDecision{Allowed: false, State: resources.DiskBlocked}
	})
	if _, err := w.CreateExactWorktree(context.Background(), "integration/no-space", target, sha); err == nil {
		t.Fatal("disk denial ignored")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("disk denial created checkout")
	}
	if head, err := w.BranchHead(context.Background(), "integration/no-space"); err != nil || head != "" {
		t.Fatalf("disk denial created branch: %s %v", head, err)
	}
}
