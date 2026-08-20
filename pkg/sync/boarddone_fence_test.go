package sync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func initGitMain(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Minimal env: avoid 1Password/gpg signing hooks from the host.
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
	run("commit", "-m", "FAC-147f initial")
	run("remote", "add", "origin", dir)
	run("fetch", "origin", "main")
	run("branch", "-u", "origin/main", "main")
}

func testOverride() *OverrideRequest {
	return &OverrideRequest{
		Policy:   "abandoned-scope",
		Actor:    "test-operator",
		Reason:   "fence test close without receipt",
		Evidence: "unit-test",
	}
}

// TestBoardDoneFenced_StaleGenerationRejected proves approve path fencing:
// BoardDoneFenced with a stale generation does not mark done.
func TestBoardDoneFenced_StaleGenerationRejected(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initGitMain(t, repo)

	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "bd-1", Ref: "FAC-147f", Title: "board done fence",
		Status: "in-review", Priority: provider.PriorityHigh,
		ProjectID: "p1", Labels: []string{"worker"}, Description: testAcceptanceDescription,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	stack, err := provider.OpenClaimStack(t.TempDir(), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()

	key := provider.LeaseKey(".", "kaneo", "p1", "FAC-147f")
	lease, err := stack.AcquireLease(ctx, key, "owner-1", "worker", "worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := stack.CAS.AdvanceFence(ctx, "bd-1", lease.Generation); err != nil {
		t.Fatal(err)
	}
	// Release and reclaim as owner-2 so gen advances.
	if err := stack.Manager.Release(ctx, key, "owner-1", lease.Generation); err != nil {
		t.Fatal(err)
	}
	lease2, err := stack.AcquireLease(ctx, key, "owner-2", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.CAS.AdvanceFence(ctx, "bd-1", lease2.Generation); err != nil {
		t.Fatal(err)
	}

	req := DoneRequest{RepoDir: repo, ProjectID: "p1", Ref: "FAC-147f", Override: testOverride(), AcceptanceEvidence: testAcceptanceEvidence}

	// Stale owner+generation must not mark done.
	_, err = BoardDoneFenced(ctx, mp, stack, key, "owner-1", lease.Generation, req)
	if err == nil {
		t.Fatal("expected stale generation rejection")
	}
	got, _ := mp.GetTask(ctx, "bd-1")
	if got.Status == "done" {
		t.Fatalf("stale fence marked done; err was %v", err)
	}
	if !errors.Is(err, claim.ErrLeaseNotCurrent) && !errors.Is(err, claim.ErrProviderFenceRejected) &&
		!strings.Contains(err.Error(), "not the current active lease") &&
		!strings.Contains(err.Error(), "fence") {
		t.Logf("rejection err: %v", err)
	}

	// Current generation succeeds with attributable override.
	res, err := BoardDoneFenced(ctx, mp, stack, key, "owner-2", lease2.Generation, req)
	if err != nil {
		t.Fatalf("current gen BoardDoneFenced: %v", err)
	}
	if res.TaskID != "bd-1" {
		t.Fatalf("res=%+v", res)
	}
	got, _ = mp.GetTask(ctx, "bd-1")
	if got.Status != "done" {
		t.Fatalf("status=%s want done", got.Status)
	}
}

// TestBoardDoneFenced_RequiresLiveLease proves empty owner/generation is refused.
func TestBoardDoneFenced_RequiresLiveLease(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initGitMain(t, repo)
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "bd-2", Ref: "FAC-147g", Status: "in-review", ProjectID: "p1",
		Labels: []string{"worker"}, Description: testAcceptanceDescription,
	})
	stack, err := provider.OpenClaimStack(t.TempDir(), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	key := provider.LeaseKey(".", "kaneo", "p1", "FAC-147g")
	req := DoneRequest{RepoDir: repo, ProjectID: "p1", Ref: "FAC-147g", Override: testOverride(), AcceptanceEvidence: testAcceptanceEvidence}
	if _, err := BoardDoneFenced(ctx, mp, stack, key, "", 0, req); err == nil {
		t.Fatal("expected fail-closed without live lease")
	}
	if _, err := BoardDoneFenced(ctx, mp, nil, key, "o", 1, req); err == nil {
		t.Fatal("expected fail-closed without stack")
	}
}
