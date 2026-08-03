package gc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// Fixture uses raw git only — no gated creation paths, no live-repo access.
func initPressureFixture(t *testing.T) (repo, dirtyWT string) {
	repo = t.TempDir()
	run(t, repo, "git", "init", "-b", "main")
	run(t, repo, "git", "config", "user.email", "t@t")
	run(t, repo, "git", "config", "user.name", "t")
	run(t, repo, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "initial")

	dirtyWT = filepath.Join(repo, "wts", "herd-dirty")
	run(t, repo, "git", "worktree", "add", "-b", "herd/dirty", dirtyWT, "HEAD")
	// macOS TempDir is a symlink (/var -> /private/var); PlanReap reports
	// resolved paths, so compare against the resolved form.
	if resolved, err := filepath.EvalSymlinks(dirtyWT); err == nil {
		dirtyWT = resolved
	}
	// Uncommitted unique content: must never be reclaimed under pressure.
	if err := os.WriteFile(filepath.Join(dirtyWT, "precious.txt"), []byte("unrecoverable"), 0644); err != nil {
		t.Fatal(err)
	}
	return repo, dirtyWT
}

func TestPressureReclamationPlanIsReadOnlyAndExact(t *testing.T) {
	repo, dirtyWT := initPressureFixture(t)
	gcm := NewGCManager(repo, worktree.NewWorktreePool(repo, filepath.Join(repo, "wts")))

	report, err := gcm.PressureReclamationPlan(context.Background(), "main")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Strictly read-only: nothing reaped, dirty tree byte-for-byte intact.
	if len(report.Reaped) != 0 {
		t.Fatalf("dry-run plan removed worktrees: %v", report.Reaped)
	}
	data, err := os.ReadFile(filepath.Join(dirtyWT, "precious.txt"))
	if err != nil || string(data) != "unrecoverable" {
		t.Fatalf("dirty content disturbed: %q err=%v", data, err)
	}

	// Exact per-target evidence: the dirty worktree is classified and
	// refused — never eligible, whatever the refusal class resolves to.
	var found *worktree.ReapCandidate
	for i := range report.Candidates {
		if report.Candidates[i].Path == dirtyWT {
			found = &report.Candidates[i]
		}
	}
	if found == nil {
		t.Fatalf("dirty worktree missing from plan evidence: %+v", report.Candidates)
	}
	if found.Eligible {
		t.Fatalf("dirty worktree marked eligible for reclamation: %+v", found)
	}
	for _, e := range report.Eligible {
		if e.Path == dirtyWT {
			t.Fatalf("dirty worktree in eligible set: %+v", e)
		}
	}
}

func TestReclaimExactRefusesEmptyTargets(t *testing.T) {
	repo, _ := initPressureFixture(t)
	gcm := NewGCManager(repo, worktree.NewWorktreePool(repo, filepath.Join(repo, "wts")))
	if _, err := gcm.ReclaimExact(context.Background(), "main", nil); err == nil {
		t.Fatal("empty target set must be refused — no broad cleanup path")
	}
}

func TestReclaimExactJITRevalidationAndPreservation(t *testing.T) {
	repo, dirtyWT := initPressureFixture(t)
	cleanWT := filepath.Join(repo, "wts", "herd-done")
	run(t, repo, "git", "worktree", "add", "-b", "herd/done", cleanWT, "HEAD")
	if resolved, err := filepath.EvalSymlinks(cleanWT); err == nil {
		cleanWT = resolved
	}
	gcm := NewGCManager(repo, worktree.NewWorktreePool(repo, filepath.Join(repo, "wts")))

	// Plan says herd/done is eligible…
	plan, err := gcm.PressureReclamationPlan(context.Background(), "main")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	eligible := false
	for _, e := range plan.Eligible {
		if e.Path == cleanWT {
			eligible = true
		}
	}
	if !eligible {
		t.Fatalf("fixture not eligible in plan: %+v", plan.Candidates)
	}

	// …but it becomes dirty AFTER planning: just-in-time revalidation at
	// execution must refuse the stale plan and preserve the tree.
	staleFile := filepath.Join(cleanWT, "late-work.txt")
	if err := os.WriteFile(staleFile, []byte("appeared after plan"), 0644); err != nil {
		t.Fatal(err)
	}
	report, err := gcm.ReclaimExact(context.Background(), "main", []string{cleanWT})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(report.Reaped) != 0 {
		t.Fatalf("stale-plan target reaped despite new dirty state: %v", report.Reaped)
	}
	if data, rerr := os.ReadFile(staleFile); rerr != nil || string(data) != "appeared after plan" {
		t.Fatalf("late work not preserved: %q err=%v", data, rerr)
	}

	// Cleaned again: reclamation succeeds AND the tip survives behind the
	// durable salvage ref (preservation proof, FAC-117).
	if err := os.Remove(staleFile); err != nil {
		t.Fatal(err)
	}
	report, err = gcm.ReclaimExact(context.Background(), "main", []string{cleanWT})
	if err != nil {
		t.Fatalf("reclaim after cleanup: %v", err)
	}
	if len(report.Reaped) != 1 {
		t.Fatalf("expected exactly one reaped target, got %+v / refused=%+v", report.Reaped, report.Refused)
	}
	if _, statErr := os.Stat(cleanWT); !os.IsNotExist(statErr) {
		t.Fatal("reaped worktree still present")
	}
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/herd/salvage/herd/done")
	cmd.Dir = repo
	if out, verr := cmd.CombinedOutput(); verr != nil {
		t.Fatalf("salvage ref missing after reap: %v\n%s", verr, out)
	}
	// Sibling dirty tree untouched through both attempts.
	if data, rerr := os.ReadFile(filepath.Join(dirtyWT, "precious.txt")); rerr != nil || string(data) != "unrecoverable" {
		t.Fatalf("sibling dirty content disturbed: %q err=%v", data, rerr)
	}
}
