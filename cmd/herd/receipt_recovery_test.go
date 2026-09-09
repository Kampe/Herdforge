package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type recoveryReceiptFixture struct {
	root, worktree, base, candidate, advancedMain, keyDir string
	cfg                                                   *config.Config
	task                                                  *provider.Task
	prior                                                 dispatch.TaskContext
	signer                                                *dispatch.Signer
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
	return recoveryReceiptFixture{root: root, worktree: worktree, base: base, candidate: candidate, advancedMain: advancedMain, keyDir: keyDir, cfg: cfg, task: task, prior: prior, signer: signer}
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
		Repository: f.prior.Repository, Role: dispatch.RoleRecovery, AuthorityScope: dispatch.AuthorityScopeCandidateSupersession, TaskRef: f.prior.TaskRef, TaskID: f.prior.TaskID,
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

func TestAuthenticatedRecoveryIdentityRejectsGenericRecoverySentinelAsSupersessionSource(t *testing.T) {
	f := newRecoveryReceiptFixture(t)
	generic := f.prior
	generic.Role = dispatch.RoleRecovery
	generic.LeaseID = "sentinel-lease"
	generic.LeaseTaskRef = f.task.Ref + ":recovery"
	generic.SessionID = "fac-631-generic-recovery"
	generic.AllowedOps = dispatch.OpsForRole(dispatch.RoleRecovery)
	generic.Signature = ""
	generic, err := f.signer.Issue(generic)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(f.worktree, generic); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, generic); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticatedRecoveryIdentity(context.Background(), f.root, f.worktree, f.task.Ref, f.prior.Branch, f.candidate, f.cfg, f.task); err == nil {
		t.Fatal("generic recovery-sentinel receipt was promoted into candidate-supersession provenance")
	}
}

func TestSelectCanonicalRecoveryReceiptIgnoresStaleHigherGenerationOtherLease(t *testing.T) {
	f := newRecoveryReceiptFixture(t)
	stale := f.prior
	stale.LeaseID = "claim:227"
	stale.LeaseGeneration = 3
	stale.SessionID = "stale-worker-session"
	stale.ExpiresAt = time.Now().Add(-time.Hour)
	if err := dispatch.WriteTaskContext(f.worktree, stale); err != nil {
		t.Fatal(err)
	}
	stale, err := f.signer.Issue(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, stale); err != nil {
		t.Fatal(err)
	}

	recovery := f.prior
	recovery.Role = dispatch.RoleRecovery
	recovery.AuthorityScope = dispatch.AuthorityScopeCandidateSupersession
	recovery.LeaseID = "claim:318"
	recovery.LeaseGeneration = 1
	recovery.LeaseTaskRef = f.task.Ref + ":recovery"
	recovery.SessionID = "current-recovery-session"
	recovery.AllowedOps = dispatch.OpsForRole(dispatch.RoleRecovery)
	recovery.ExpiresAt = time.Now().Add(time.Hour)
	recovery, err = f.signer.Issue(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, recovery); err != nil {
		t.Fatal(err)
	}

	selected, err := dispatch.SelectCanonicalRecoveryReceipt(f.root, dispatch.RecoveryReceiptSelector{
		ProviderType: recovery.ProviderType, ProjectID: recovery.ProjectID,
		Repository: recovery.Repository, Role: dispatch.RoleRecovery,
		TaskRef: recovery.TaskRef, TaskID: recovery.TaskID, Branch: recovery.Branch,
		BaseSHA: recovery.BaseSHA, CandidateSHA: recovery.CandidateSHA, LeaseID: recovery.LeaseID,
		LeaseGeneration: recovery.LeaseGeneration, LeaseTaskRef: recovery.LeaseTaskRef,
		SessionID: recovery.SessionID,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !selected.EqualsIssued(recovery) {
		t.Fatalf("selected stale or altered receipt: got session %s lease %s generation %d", selected.SessionID, selected.LeaseID, selected.LeaseGeneration)
	}
	ambiguous := recovery
	ambiguous.SessionID = "second-current-recovery-session"
	ambiguous, err = f.signer.Issue(ambiguous)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, ambiguous); err != nil {
		t.Fatal(err)
	}
	selector := dispatch.RecoveryReceiptSelector{
		ProviderType: recovery.ProviderType, ProjectID: recovery.ProjectID,
		Repository: recovery.Repository, Role: dispatch.RoleRecovery,
		TaskRef: recovery.TaskRef, TaskID: recovery.TaskID, Branch: recovery.Branch,
		BaseSHA: recovery.BaseSHA, CandidateSHA: recovery.CandidateSHA, LeaseID: recovery.LeaseID,
		LeaseGeneration: recovery.LeaseGeneration, LeaseTaskRef: recovery.LeaseTaskRef,
	}
	if _, err := dispatch.SelectCanonicalRecoveryReceipt(f.root, selector, time.Now()); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("omitted session must reject conflicting exact candidates: %v", err)
	}
	generic, err := dispatch.LoadCanonicalReceipt(f.root, f.task.Ref)
	if err != nil || generic.LeaseGeneration != stale.LeaseGeneration {
		t.Fatalf("fixture did not reproduce old newest-generation selection: %v, got generation %d", err, generic.LeaseGeneration)
	}
}

func recoveryCLIArgs(tc dispatch.TaskContext, target string) []string {
	return []string{"receipt", "recover", "--provider-type", tc.ProviderType, "--project-id", tc.ProjectID,
		"--repository", tc.Repository, "--role", tc.Role, "--task-id", tc.TaskID, "--branch", tc.Branch,
		"--base-sha", tc.BaseSHA, "--candidate-sha", tc.CandidateSHA, "--lease-id", tc.LeaseID,
		"--lease-generation", fmt.Sprintf("%d", tc.LeaseGeneration), "--lease-task-ref", tc.LeaseTaskRef,
		"--session-id", tc.SessionID, tc.TaskRef, target}
}

func TestReceiptRecoverCLIRequiresExactTargetAndReadOnlyLeaseObservation(t *testing.T) {
	f := newRecoveryReceiptFixture(t)
	recovery := f.prior
	recovery.Role = dispatch.RoleRecovery
	recovery.AuthorityScope = dispatch.AuthorityScopeCandidateSupersession
	recovery.LeaseTaskRef = f.task.Ref + ":recovery"
	recovery.SessionID = "public-cli-recovery"
	recovery.AllowedOps = dispatch.OpsForRole(dispatch.RoleRecovery)
	recovery.ExpiresAt = time.Now().Add(time.Hour)
	storePath := filepath.Join(f.root, ".herd", "herdforge.db")
	leaseStore, err := claim.NewSQLiteLeaseStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := leaseStore.Acquire(context.Background(), claim.LeaseKey{Repo: recovery.Repository, Provider: recovery.ProviderType, Project: recovery.ProjectID, TaskRef: recovery.LeaseTaskRef}, "coordinator-recovery", dispatch.RoleRecovery, f.worktree, time.Now(), time.Hour)
	if err != nil {
		leaseStore.Close()
		t.Fatal(err)
	}
	if err := leaseStore.Close(); err != nil {
		t.Fatal(err)
	}
	recovery.LeaseID = fmt.Sprintf("claim:%d", lease.ID)
	recovery.LeaseGeneration = lease.Generation
	recovery, err = f.signer.Issue(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, recovery); err != nil {
		t.Fatal(err)
	}
	binary := buildHerd(t)
	before, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}
	badBranch := recoveryCLIArgs(recovery, f.worktree)
	for i := range badBranch {
		if badBranch[i] == "--branch" && i+1 < len(badBranch) {
			badBranch[i+1] = "wrong/branch"
		}
	}
	if out, err := herdCmd(binary, f.root, f.keyDir, badBranch...).CombinedOutput(); err == nil {
		t.Fatalf("wrong branch selector must fail closed: %s", out)
	}
	if after, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile)); err != nil || string(after) != string(before) {
		t.Fatalf("wrong branch refusal changed target: %v", err)
	}
	foreign := filepath.Join(filepath.Dir(f.root), "foreign-recovery-target")
	if out, err := herdCmd(binary, f.root, f.keyDir, recoveryCLIArgs(recovery, foreign)...).CombinedOutput(); err == nil {
		t.Fatalf("foreign target must fail closed: %s", out)
	}
	if after, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile)); err != nil || string(after) != string(before) {
		t.Fatalf("foreign target refusal changed source target: %v", err)
	}
	canonicalPaths, err := filepath.Glob(filepath.Join(f.root, dispatch.CanonicalTaskContextDir, "fac-631-*.json"))
	if err != nil || len(canonicalPaths) == 0 {
		t.Fatalf("locate canonical receipts: %v", err)
	}
	var recoveryPath string
	canonicalPaths, err = filepath.Glob(filepath.Join(f.root, dispatch.CanonicalTaskContextDir, "fac-631-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range canonicalPaths {
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), recovery.SessionID) {
			recoveryPath = path
			break
		}
	}
	if recoveryPath == "" {
		t.Fatal("locate current recovery canonical receipt")
	}
	originalCanonical, err := os.ReadFile(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	badCanonical := strings.Replace(string(originalCanonical), recovery.Signature, strings.Repeat("0", len(recovery.Signature)), 1)
	if err := os.WriteFile(recoveryPath, []byte(badCanonical), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := herdCmd(binary, f.root, f.keyDir, recoveryCLIArgs(recovery, f.worktree)...).CombinedOutput(); err == nil || !strings.Contains(string(out), "authenticate") {
		t.Fatalf("invalid signature must fail closed: err=%v output=%s", err, out)
	}
	if err := os.WriteFile(recoveryPath, originalCanonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile)); err != nil || string(after) != string(before) {
		t.Fatalf("invalid signature refusal changed target: %v", err)
	}
	expired := recovery
	expired.SessionID = "public-cli-expired-session"
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	expired, err = f.signer.Issue(expired)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, expired); err != nil {
		t.Fatal(err)
	}
	if out, err := herdCmd(binary, f.root, f.keyDir, recoveryCLIArgs(expired, f.worktree)...).CombinedOutput(); err == nil || !strings.Contains(string(out), "authorized") {
		t.Fatalf("expired receipt must fail closed: err=%v output=%s", err, out)
	}
	if after, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile)); err != nil || string(after) != string(before) {
		t.Fatalf("expired receipt refusal changed target: %v", err)
	}
	canonicalPaths, err = filepath.Glob(filepath.Join(f.root, dispatch.CanonicalTaskContextDir, "fac-631-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range canonicalPaths {
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), expired.SessionID) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	ambiguous := recovery
	ambiguous.SessionID = "public-cli-conflicting-session"
	ambiguous, err = f.signer.Issue(ambiguous)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, ambiguous); err != nil {
		t.Fatal(err)
	}
	noSession := recoveryCLIArgs(recovery, f.worktree)
	for i := 0; i < len(noSession); i++ {
		if noSession[i] == "--session-id" && i+1 < len(noSession) {
			noSession = append(noSession[:i], noSession[i+2:]...)
			break
		}
	}
	if out, err := herdCmd(binary, f.root, f.keyDir, noSession...).CombinedOutput(); err == nil || !strings.Contains(string(out), "ambiguous") {
		t.Fatalf("ambiguous public recovery must fail closed: err=%v output=%s", err, out)
	}
	if after, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile)); err != nil || string(after) != string(before) {
		t.Fatalf("ambiguity refusal changed target: %v", err)
	}
	if err := os.Remove(storePath); err != nil {
		t.Fatal(err)
	}
	out, err := herdCmd(binary, f.root, f.keyDir, recoveryCLIArgs(recovery, f.worktree)...).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "read-only") && !strings.Contains(string(out), "lease store") {
		t.Fatalf("missing lease DB must fail closed: err=%v output=%s", err, out)
	}
	if _, statErr := os.Stat(storePath); !os.IsNotExist(statErr) {
		t.Fatalf("missing DB refusal recreated lease store: %v", statErr)
	}
	after, err := os.ReadFile(filepath.Join(f.worktree, dispatch.TaskContextFile))
	if err != nil || string(after) != string(before) {
		t.Fatalf("missing DB refusal changed target receipt: %v", err)
	}

	leaseStore, err = claim.NewSQLiteLeaseStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err = leaseStore.Acquire(context.Background(), claim.LeaseKey{Repo: recovery.Repository, Provider: recovery.ProviderType, Project: recovery.ProjectID, TaskRef: recovery.LeaseTaskRef}, "coordinator-recovery", dispatch.RoleRecovery, f.worktree, time.Now(), time.Hour)
	if err != nil {
		leaseStore.Close()
		t.Fatal(err)
	}
	if err := leaseStore.Close(); err != nil {
		t.Fatal(err)
	}
	recovery.LeaseID = fmt.Sprintf("claim:%d", lease.ID)
	recovery.LeaseGeneration = lease.Generation
	recovery.SessionID = "public-cli-recovery-success"
	recovery, err = f.signer.Issue(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(f.root, recovery); err != nil {
		t.Fatal(err)
	}
	out, err = herdCmd(binary, f.root, f.keyDir, recoveryCLIArgs(recovery, f.worktree)...).CombinedOutput()
	if err != nil {
		t.Fatalf("exact registered recovery target should succeed: %v output=%s", err, out)
	}
	got, err := dispatch.ReadTaskContext(f.worktree)
	if err != nil || !got.EqualsIssued(recovery) || got.BaseSHA != f.base {
		t.Fatalf("successful recovery changed signed authority/base: %v got=%+v", err, got)
	}
}
