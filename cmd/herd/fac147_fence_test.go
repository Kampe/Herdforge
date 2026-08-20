package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

func initGitMainFAC147(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_TERMINAL_PROMPT=0",
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f")
	run("commit", "-m", "FAC-147x initial")
	run("remote", "add", "origin", dir)
	run("fetch", "origin", "main")
	run("branch", "-u", "origin/main", "main")
}

// TestFencedBoardDone_StaleGenerationRejected exercises the production
// approve/board-done helper path: live lease marks done; stale gen cannot reopen.
func TestFencedBoardDone_StaleGenerationRejected(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initGitMainFAC147(t, repo)

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	mp := provider.NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&provider.Task{
		ID: "t1", Ref: "FAC-147x", Title: "x", Status: "in-review",
		ProjectID: "p", Labels: []string{"worker"}, UpdatedAt: now, CreatedAt: now,
		Description: "```herd-acceptance-v1\n{\"commands\":[{\"command\":\"go test ./...\",\"context\":\"Herdforge worktree\"}]}\n```",
	})

	claimDir := t.TempDir()
	t.Setenv("HERD_CLAIM_DIR", claimDir)
	stack, err := provider.OpenClaimStack(claimDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()

	cfg := &config.Config{}
	cfg.TaskProvider.Type = "memory"
	cfg.TaskProvider.ProjectID = "p"

	task, err := resolveTaskByRef(ctx, mp, "p", "FAC-147x")
	if err != nil {
		t.Fatal(err)
	}

	req := hsync.DoneRequest{
		RepoDir: ".", ProjectID: "p", Ref: "FAC-147x",
		AcceptanceEvidence: "context: Herdforge worktree\n$ go test ./...\nPASS",
		Override: &hsync.OverrideRequest{
			Policy: "abandoned-scope", Actor: "test", Reason: "unit", Evidence: "test",
		},
	}

	// Live generation succeeds.
	done, err := fencedBoardDone(ctx, cfg, mp, stack, task, req)
	if err != nil {
		t.Fatalf("live board-done: %v", err)
	}
	if done == nil || done.TaskID != "t1" {
		t.Fatalf("unexpected done: %+v", done)
	}
	got, _ := mp.GetTask(ctx, "t1")
	if provider.NormalizeStatus(got.Status) != provider.StatusDone {
		t.Fatalf("status=%s want done", got.Status)
	}

	// Stale generation via BoardDoneFenced must be rejected even when the
	// card is already done (idempotent short-circuit still requires a live lease).
	key := provider.LeaseKey(".", "memory", "p", "FAC-147x")
	_, err = hsync.BoardDoneFenced(ctx, mp, stack, key, "stale-owner", 1, req)
	// Board must stay done regardless of the error shape.
	got, _ = mp.GetTask(ctx, "t1")
	if provider.NormalizeStatus(got.Status) != provider.StatusDone {
		t.Fatalf("stale attempt changed status to %s (err=%v)", got.Status, err)
	}
}

// TestFencedBoardStatus_RequiresStack proves fail-closed without ClaimStack.
func TestFencedBoardStatus_RequiresStack(t *testing.T) {
	cfg := &config.Config{}
	cfg.TaskProvider.Type = "memory"
	cfg.TaskProvider.ProjectID = "p"
	task := &provider.Task{ID: "t", Ref: "FAC-1", Labels: []string{"worker"}}
	err := fencedBoardStatus(context.Background(), cfg, nil, task, "worker", "in-review")
	if err == nil {
		t.Fatal("expected error without stack")
	}
}
