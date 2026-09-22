package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-845: exclusive worktree branch fix/fac607-diagnostics-3342 used to be
// parsed as DIAGNOSTICS-3342 by drainRefToken first-match. Card identity now
// comes from verified TASK-CONTEXT on that branch.

func fac845BranchHome(t *testing.T, branch, taskRef, taskID string) (root, author, sha string) {
	t.Helper()
	root = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main", ".")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("TASK-CONTEXT.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	signer := fixtureSigner(t, keyDir, root)
	git("add", ".")
	git("add", "-f", ".herd/receipt.pub")
	git("commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("commit", "-qm", "candidate")
	sha = git("rev-parse", "HEAD")
	base := git("rev-parse", "HEAD^")

	author = filepath.Join(filepath.Dir(root), "author-"+filepath.Base(root))
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", branch, author, sha).CombinedOutput(); err != nil {
		t.Fatalf("author worktree: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", root, "worktree", "remove", "--force", author).Run()
		_ = os.RemoveAll(author)
	})
	git("checkout", "-q", "main")

	signed, err := signer.Issue(dispatch.TaskContext{
		ProviderType:    "memory",
		ProjectID:       "project-1",
		Repository:      dispatch.RepositoryIdentityOrName(root, "herdforge-test"),
		Role:            dispatch.RoleWorker,
		TaskRef:         taskRef,
		TaskID:          taskID,
		Branch:          branch,
		BaseSHA:         base,
		CandidateSHA:    sha,
		LeaseID:         "lease-" + taskID,
		LeaseGeneration: 1,
		LeaseTaskRef:    taskRef,
		SessionID:       "session-" + taskID,
		AllowedOps:      dispatch.WorkerOps,
		ExpiresAt:       time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(author, signed); err != nil {
		t.Fatal(err)
	}
	return root, author, sha
}

func TestFAC845DiagnosticsBranchResolvesFromAuthenticatedContext(t *testing.T) {
	const branch = "fix/fac607-diagnostics-3342"
	if drainCandidateRef(branch) != "DIAGNOSTICS-3342" {
		t.Fatalf("drainCandidateRef(%q) = %q, want DIAGNOSTICS-3342 so this still documents the guessing bug", branch, drainCandidateRef(branch))
	}
	root, _, sha := fac845BranchHome(t, branch, "FAC-607", "lewie1yelta24pdvajked7g8")
	p := provider.NewMemoryProvider()
	p.AddTask(&provider.Task{ID: "lewie1yelta24pdvajked7g8", Ref: "FAC-607", ProjectID: "project-1", Status: provider.StatusInProgress})

	got, err := resolveReviewTaskRef(context.Background(), p, "project-1", branch, root, sha)
	if err != nil {
		t.Fatalf("authenticated diagnostics branch must resolve FAC-607, not DIAGNOSTICS-3342: %v", err)
	}
	if got == nil || got.Ref != "FAC-607" {
		t.Fatalf("resolved %+v, want FAC-607", got)
	}
	card, err := reviewPacketTaskIdentity(got)
	if err != nil {
		t.Fatal(err)
	}
	if card != "FAC-607" {
		t.Fatalf("packet identity = %q, want FAC-607", card)
	}
}

func TestFAC845ArbitraryBranchResolvesFromAuthenticatedContext(t *testing.T) {
	const branch = "wt/arbitrary-home"
	root, _, sha := fac845BranchHome(t, branch, "FAC-321", "task-321")
	p := provider.NewMemoryProvider()
	p.AddTask(&provider.Task{ID: "task-321", Ref: "FAC-321", ProjectID: "project-1", Status: provider.StatusInProgress})

	got, err := resolveReviewTaskRef(context.Background(), p, "project-1", branch, root, sha)
	if err != nil {
		t.Fatalf("arbitrary branch must take identity from authenticated context: %v", err)
	}
	if got == nil || got.Ref != "FAC-321" {
		t.Fatalf("resolved %+v, want FAC-321", got)
	}
}

func TestFAC845DiagnosticsBranchWithoutContextRefusesGuess(t *testing.T) {
	p := provider.NewMemoryProvider()
	p.AddTask(&provider.Task{ID: "lewie1yelta24pdvajked7g8", Ref: "FAC-607", ProjectID: "project-1", Status: provider.StatusInProgress})
	_, err := resolveReviewTaskRef(context.Background(), p, "project-1", "fix/fac607-diagnostics-3342", "", "")
	if err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("missing authenticated context must refuse, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "DIAGNOSTICS-3342") {
		t.Fatalf("refusal leaked the guessed token: %v", err)
	}
}
