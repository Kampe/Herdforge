package gc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// FAC-178: this test must remain disposable. It intentionally proves that the
// historical global wrapper refuses before any removal operation; it never
// discovers or mutates the developer checkout.
func TestGCManager_GlobalAutoReapIsContained(t *testing.T) {
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)
	defaultBranch := gitOutput(t, tmpDir, "symbolic-ref", "--short", "HEAD")
	if defaultBranch == "" {
		t.Fatal("disposable fixture must resolve a non-empty symbolic branch")
	}
	fixtureWorktrees(t, tmpDir)
	wm := worktree.NewWorktreeManager(tmpDir)
	gcm := NewGCManager(tmpDir, wm)
	gcm.HoldReader = gcAllowHolds{}

	report, err := gcm.ScanOverlap(context.Background(), 2)
	if err != nil || report == nil {
		t.Fatalf("expected clean overlap scan, got err: %v", err)
	}

	pruned, err := gcm.PruneStaleWorktrees(context.Background())
	if err == nil || !errors.Is(err, errGlobalAutoReapDisabled) {
		t.Fatalf("expected global auto-reap refusal, got count=%d err=%v", pruned, err)
	}
	if pruned != 0 {
		t.Fatalf("refused global auto-reap must report zero removals, got %d", pruned)
	}

	// The disposable fixture includes every protection class. Planning is
	// permitted, but the active lease must be preserved and no path outside an
	// explicit target set may enter an action plan.
	reapReport, err := wm.PlanReap(context.Background(), worktree.ReapPolicy{
		DefaultBranch: defaultBranch,
		LeaseProbe: func(_ context.Context, _ string, branch string) (bool, error) {
			return branch == "herd/active", nil
		},
	})
	if err != nil {
		t.Fatalf("isolated fixture plan: %v", err)
	}
	for _, candidate := range reapReport.Candidates {
		if candidate.Branch == "herd/active" && candidate.Eligible {
			t.Fatal("active lease fixture was eligible")
		}
	}

	// Ambient-dot mutation guard: redirect the exact target to the caller's
	// checkout spelling. The disposable manager must reject it before the
	// injected removal seam is reached.
	removals := 0
	wm.RemoveWorktreeFunc = func(_ context.Context, _ string) error {
		removals++
		return errors.New("MUTATION GUARD FAILED: removal reached")
	}
	_, err = wm.Reap(context.Background(), worktree.ReapPolicy{
		DefaultBranch: defaultBranch, AutoReap: true, TargetPaths: []string{"."},
		LeaseProbe:           func(context.Context, string, string) (bool, error) { return false, nil },
		LeaseGenerationProbe: func(context.Context, string, string) (string, error) { return "generation-1", nil },
		BoardEvidenceProbe:   func(context.Context, string, string) (string, error) { return "board-proof", nil },
		Evidence: worktree.ReapEvidence{
			IntegrationSHA: gitOutput(t, tmpDir, "rev-parse", defaultBranch),
			BoardEvidence:  "board-proof", LeaseGeneration: "generation-1", PolicyDigest: "policy-1", Actor: "fac-178-test",
		},
		ReceiptSink:  func(worktree.ReapReceipt) error { return nil },
		ActionPolicy: "remove",
	})
	if err == nil || removals != 0 {
		t.Fatalf("ambient-dot guard failed: err=%v removal calls=%d", err, removals)
	}
}

type gcAllowHolds struct{}

func (gcAllowHolds) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Generation: 1}, nil
}

func fixtureWorktrees(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"merged", "dirty", "unique", "active", "sibling"} {
		path := filepath.Join(root, name)
		runGit(t, root, "worktree", "add", "-b", "herd/"+name, path, "HEAD")
	}
	runGit(t, root, "worktree", "add", "--detach", filepath.Join(root, "detached"), "HEAD")
	if err := os.WriteFile(filepath.Join(root, "dirty", "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unique", "unique.txt"), []byte("unique"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, filepath.Join(root, "unique"), "add", "unique.txt")
	runGit(t, filepath.Join(root, "unique"), "commit", "-m", "unique fixture")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testgit.Command(dir, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(dir, args...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(bytes.TrimSpace(out))
}
