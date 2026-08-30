package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type recoveryReceiptFixture struct {
	root, worktree, base, candidate, advancedMain string
	cfg                                           *config.Config
	task                                          *provider.Task
	prior                                         dispatch.TaskContext
	signer                                        *dispatch.Signer
}

func recoveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRecoveryReceiptFixture(t *testing.T) recoveryReceiptFixture {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	recoveryGit(t, root, "init", "-b", "main")
	recoveryGit(t, root, "config", "user.name", "Recovery Test")
	recoveryGit(t, root, "config", "user.email", "recovery@example.invalid")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".herd/\nTASK-CONTEXT.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recoveryGit(t, root, "add", ".gitignore", "base.txt")
	recoveryGit(t, root, "commit", "-m", "base")
	base := recoveryGit(t, root, "rev-parse", "HEAD")

	worktree := filepath.Join(root, ".herd", "worktrees", "fac-631")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	recoveryGit(t, root, "worktree", "add", "-b", "herd/fac-631", worktree, base)
	if err := os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recoveryGit(t, worktree, "add", "candidate.txt")
	recoveryGit(t, worktree, "commit", "-m", "candidate")
	candidate := recoveryGit(t, worktree, "rev-parse", "HEAD")

	// Advance main on a sibling commit after the worker candidate. This is the
	// FAC-631 topology: origin/main is not an ancestor of the candidate, while
	// the signed historical base remains their exact merge-base.
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("advanced main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recoveryGit(t, root, "add", "main.txt")
	recoveryGit(t, root, "commit", "-m", "advance main")
	advancedMain := recoveryGit(t, root, "rev-parse", "HEAD")
	recoveryGit(t, root, "update-ref", "refs/remotes/origin/main", advancedMain)
	if exec.Command("git", "-C", worktree, "merge-base", "--is-ancestor", advancedMain, candidate).Run() == nil {
		t.Fatal("fixture invalid: advanced origin/main must not be candidate ancestor")
	}
	if got := recoveryGit(t, worktree, "merge-base", advancedMain, candidate); got != base {
		t.Fatalf("fixture merge-base = %s, want signed base %s", got, base)
	}

	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "herdforge-test", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{
			Type: "kaneo", ProjectID: "project-fac", WorkspaceID: "workspace-fac", APIKeyEnv: "KANEO_API_KEY",
		},
	}
	task := &provider.Task{ID: "task-fac-631", Ref: "FAC-631", ProjectID: cfg.TaskProvider.ProjectID, Status: provider.StatusInProgress}
	keyDir := filepath.Join(parent, "keys")
	if err := dispatch.WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	identity, err := dispatch.RepositoryIdentity(root, cfg.Project.Name)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := dispatch.LoadOrCreateSigner(keyDir, identity, root)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := signer.Issue(dispatch.TaskContext{
		ProviderType: cfg.TaskProvider.Type, ProjectID: cfg.TaskProvider.ProjectID,
		ProviderWorkspace: cfg.TaskProvider.WorkspaceID, ProviderProfile: cfg.TaskProvider.APIKeyEnv,
		Repository: dispatch.RepositoryIdentityOrName(root, cfg.Project.Name), Role: dispatch.RoleWorker,
		TaskRef: task.Ref, TaskID: task.ID, Branch: "herd/fac-631", BaseSHA: base, CandidateSHA: candidate,
		LeaseID: "claim-47", LeaseGeneration: 1, LeaseTaskRef: task.Ref, SessionID: "fac-631-worker",
		AllowedOps: dispatch.OpsForRole(dispatch.RoleWorker), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(worktree, prior); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(root, prior); err != nil {
		t.Fatal(err)
	}
	return recoveryReceiptFixture{root: root, worktree: worktree, base: base, candidate: candidate, advancedMain: advancedMain, cfg: cfg, task: task, prior: prior, signer: signer}
}

func TestRecoveryReceiptIdentityPreservesAuthenticatedBaseWhenOriginMainAdvances(t *testing.T) {
	f := newRecoveryReceiptFixture(t)
	prior, err := authenticatedRecoveryIdentity(context.Background(), f.root, f.worktree, f.task.Ref, "herd/fac-631", f.candidate, f.cfg, f.task)
	if err != nil {
		t.Fatal(err)
	}
	if prior.BaseSHA != f.base {
		t.Fatalf("recovery base = %s, want authenticated %s", prior.BaseSHA, f.base)
	}
	if prior.BaseSHA == f.advancedMain {
		t.Fatal("recovery substituted current origin/main for authenticated base")
	}
	if prior.TaskID != f.task.ID || prior.ProjectID != f.task.ProjectID || prior.Branch != "herd/fac-631" {
		t.Fatalf("immutable task/project/branch identity changed: %+v", prior)
	}
}

func TestRecoveryReceiptIdentityFailurePreservesCurrentReceipts(t *testing.T) {
	f := newRecoveryReceiptFixture(t)
	localBefore, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}
	wrongTask := *f.task
	wrongTask.ProjectID = "different-project"
	if _, err := authenticatedRecoveryIdentity(context.Background(), f.root, f.worktree, f.task.Ref, "herd/fac-631", f.candidate, f.cfg, &wrongTask); err == nil {
		t.Fatal("independent provider task project mismatch must fail recovery issuance")
	}
	localAfter, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(localAfter) != string(localBefore) {
		t.Fatal("failed prevalidation mutated TASK-CONTEXT.json")
	}
	canonical, err := dispatch.LoadCanonicalReceiptSession(f.root, f.prior.ProviderType, f.prior.ProjectID, f.prior.TaskRef, f.prior.SessionID)
	if err != nil || !canonical.EqualsIssued(f.prior) {
		t.Fatalf("failed prevalidation mutated canonical receipt: %v", err)
	}
}

func TestPersistRecoveryReceiptRollsBackPostCanonicalWriteFailure(t *testing.T) {
	f := newRecoveryReceiptFixture(t)
	recovery, err := f.signer.Issue(dispatch.TaskContext{
		ProviderType: f.prior.ProviderType, ProjectID: f.prior.ProjectID,
		ProviderWorkspace: f.prior.ProviderWorkspace, ProviderProfile: f.prior.ProviderProfile,
		Repository: f.prior.Repository, Role: dispatch.RoleRecovery, TaskRef: f.prior.TaskRef, TaskID: f.prior.TaskID,
		Branch: f.prior.Branch, BaseSHA: f.prior.BaseSHA, CandidateSHA: f.candidate,
		LeaseID: "recovery-lease", LeaseGeneration: 1, LeaseTaskRef: f.task.Ref + ":recovery", SessionID: "fac-631-recovery",
		AllowedOps: dispatch.OpsForRole(dispatch.RoleRecovery), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	postCommitFailure := func(root string, receipt dispatch.TaskContext) error {
		if err := dispatch.StoreCanonicalReceipt(root, receipt); err != nil {
			return err
		}
		return errors.New("injected failure after canonical commit")
	}
	if err := persistRecoveryReceipt(f.root, f.worktree, recovery, f.prior, postCommitFailure); err == nil {
		t.Fatal("post-canonical persistence failure must be reported")
	}
	local, err := dispatch.ReadTaskContext(f.worktree)
	if err != nil || !local.EqualsIssued(f.prior) {
		t.Fatalf("TASK-CONTEXT rollback failed: %v", err)
	}
	canonical, err := dispatch.LoadCanonicalReceiptSession(f.root, f.prior.ProviderType, f.prior.ProjectID, f.prior.TaskRef, f.prior.SessionID)
	if err != nil || !canonical.EqualsIssued(f.prior) {
		t.Fatalf("prior canonical receipt changed: %v", err)
	}
	if _, err := dispatch.LoadCanonicalReceiptSession(f.root, recovery.ProviderType, recovery.ProjectID, recovery.TaskRef, recovery.SessionID); err == nil {
		t.Fatal("partially committed recovery canonical receipt survived compensation")
	}
}
