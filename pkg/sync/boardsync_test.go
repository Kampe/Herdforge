package sync

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// bsyncRepo builds a git repo with specific facts for board-sync tests.
// It creates origin/main with one shipped commit and returns facts for
// test setup.
type bsyncRepo struct {
	dir      string
	mainSHA  string
	shipped  string // SHA naming a shipped ref
}

func newBsyncRepo(t *testing.T) *bsyncRepo {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@herdforge.local")
	run("config", "user.name", "herdforge-test")
	run("config", "commit.gpgsign", "false")
	run("config", "tag.gpgsign", "false")
	// A commit that shipped FAC-18 to origin/main.
	run("commit", "--allow-empty", "-q", "-m", "feat: implement widget renderer (FAC-18)")
	mainSHA := run("rev-parse", "HEAD")
	run("update-ref", "refs/remotes/origin/main", mainSHA)
	return &bsyncRepo{dir: dir, mainSHA: mainSHA, shipped: mainSHA}
}

// addStrandedBranch creates a branch with one commit that is NOT on origin/main.
func (r *bsyncRepo) addStrandedBranch(t *testing.T, branch, subj string) string {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("checkout", "-q", "-b", branch)
	run("commit", "--allow-empty", "-q", "-m", subj)
	return run("rev-parse", "HEAD")
}

// addWorktree creates a linked worktree with an optional ahead commit.
func (r *bsyncRepo) addWorktree(t *testing.T, wtDir, branch string, aheadSubj string) {
	t.Helper()
	runIn := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runIn(r.dir, "worktree", "add", "-q", "-b", branch, wtDir, "origin/main")
	if aheadSubj != "" {
		runIn(wtDir, "commit", "--allow-empty", "-q", "-m", aheadSubj)
	}
}

// addMergedCommit adds a commit to origin/main (simulates a merge landing).
func (r *bsyncRepo) addMergedCommit(t *testing.T, subj string) string {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("checkout", "-q", "main")
	run("commit", "--allow-empty", "-q", "-m", subj)
	sha := run("rev-parse", "HEAD")
	run("update-ref", "refs/remotes/origin/main", sha)
	return sha
}

func newBoard(tasks ...*provider.Task) *provider.MemoryProvider {
	mp := provider.NewMemoryProvider()
	for _, t := range tasks {
		mp.AddTask(t)
	}
	return mp
}

func TestReconcileBoard_StaleInProgress(t *testing.T) {
	// A card in in-progress with no active branch and no shipped evidence.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-99", Title: "unstarted work", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 1 {
		t.Fatalf("expected 1 drift, got %d", drift.Drift)
	}
	if len(drift.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(drift.Findings))
	}
	f := drift.Findings[0]
	if f.Kind != "STALE" {
		t.Fatalf("expected STALE, got %s", f.Kind)
	}
	if f.Ref != "FAC-99" {
		t.Fatalf("expected ref FAC-99, got %s", f.Ref)
	}
}

func TestReconcileBoard_ShippedInProgress(t *testing.T) {
	// A card in in-progress but the ref is already on origin/main.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-18", Title: "shipped work", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 1 {
		t.Fatalf("expected 1 drift, got %d", drift.Drift)
	}
	f := drift.Findings[0]
	if f.Kind != "SHIPPED" {
		t.Fatalf("expected SHIPPED, got %s", f.Kind)
	}
	if !strings.Contains(f.Action, "done") {
		t.Fatalf("SHIPPED action should suggest done: %s", f.Action)
	}
}

func TestReconcileBoard_ActiveInProgressHonest(t *testing.T) {
	// A card in in-progress with a live worktree branch: board is honest.
	repo := newBsyncRepo(t)
	wt := t.TempDir()
	repo.addWorktree(t, wt, "fac-18-widget-renderer", "feat: work in progress on FAC-18")

	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-18", Title: "widget renderer", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift for active work, got %d (findings: %+v)", drift.Drift, drift.Findings)
	}
}

func TestReconcileBoard_ActiveInReviewHonest(t *testing.T) {
	// A card in in-review with a live branch: board is honest.
	repo := newBsyncRepo(t)
	wt := t.TempDir()
	repo.addWorktree(t, wt, "fac-18-widget", "feat: widget in review for FAC-18")

	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-18", Title: "widget", Status: "in-review", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift for active review, got %d", drift.Drift)
	}
}

func TestReconcileBoard_BoardLagTodo(t *testing.T) {
	// A card in to-do but a live branch exists for it.
	repo := newBsyncRepo(t)
	wt := t.TempDir()
	repo.addWorktree(t, wt, "fac-77-new-feature", "feat: new feature for FAC-77")

	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "new feature", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 1 {
		t.Fatalf("expected 1 drift, got %d", drift.Drift)
	}
	f := drift.Findings[0]
	if f.Kind != "BOARD_LAG" {
		t.Fatalf("expected BOARD_LAG, got %s", f.Kind)
	}
	if !strings.Contains(f.Action, "in-progress") {
		t.Fatalf("BOARD_LAG action should suggest in-progress: %s", f.Action)
	}
}

func TestReconcileBoard_TodoNoBranchHonest(t *testing.T) {
	// A card in to-do with no branch: board is honest.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-99", Title: "backlog item", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift for honest to-do, got %d", drift.Drift)
	}
}

func TestReconcileBoard_StandingEpicSkipped(t *testing.T) {
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-7", Title: "Standing Epic: Dashboard", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift for standing epic, got %d", drift.Drift)
	}
}

func TestReconcileBoard_DoneStatusSkipped(t *testing.T) {
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "CHA-5", Title: "completed task", Status: "done", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift for done status, got %d", drift.Drift)
	}
}

func TestReconcileBoard_EmptyRefSkipped(t *testing.T) {
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "", Title: "no ref", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift for empty ref, got %d", drift.Drift)
	}
}

func TestReconcileBoard_StrandedBranchStale(t *testing.T) {
	// A stranded branch exists but does NOT name the ref in question.
	// The workInFlight flag triggers UNKNOWN (cannot prove dead), NOT STALE.
	repo := newBsyncRepo(t)
	repo.addStrandedBranch(t, "feature-x", "feat: unrelated work")
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "unrelated", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 1 {
		t.Fatalf("expected 1 drift for unrelated branch, got %d", drift.Drift)
	}
	f := drift.Findings[0]
	if f.Kind != "UNKNOWN" {
		t.Fatalf("expected UNKNOWN (work in flight exists), got %s", f.Kind)
	}
}

func TestReconcileBoard_ShippedWithNonDigitBoundary(t *testing.T) {
	// FAC-1 must not match the FAC-18 commit on origin/main.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-1", Title: "shorter ref", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 1 {
		t.Fatalf("expected 1 drift (FAC-1 not shipped), got %d", drift.Drift)
	}
	if drift.Findings[0].Kind != "STALE" {
		t.Fatalf("expected STALE (FAC-1 not shipped), got %s", drift.Findings[0].Kind)
	}
}
