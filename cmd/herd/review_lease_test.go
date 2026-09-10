package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

func TestApprovalLeaseKey_UsesOneIdentityAcrossRegisteredWorktrees(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "seed"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "seed")
	gitIn(t, root, "commit", "-m", "chore: seed")
	wtA := filepath.Join(t.TempDir(), "registered-a")
	wtB := filepath.Join(t.TempDir(), "registered-b")
	gitIn(t, root, "worktree", "add", "-b", "approval-a", wtA, "main")
	gitIn(t, root, "worktree", "add", "-b", "approval-b", wtB, "main")

	repository := dispatch.RepositoryIdentityOrName(root, "herdforge-test")
	stack := provider.NewTestStack(t, provider.NewMemoryProvider())
	aliases, err := approvalLeaseAliases(context.Background(), root, repository, "herdforge-test", "memory", "proj", "FAC-782:review")
	if err != nil {
		t.Fatal(err)
	}
	canonical := provider.LeaseKey(root, "memory", "proj", "FAC-782:review")
	if len(aliases) < 3 || aliases[0] != canonical {
		t.Fatalf("approval aliases=%+v, want canonical plus registered worktree aliases", aliases)
	}

	owner, err := stack.AcquireLeaseFromAliases(context.Background(), aliases, canonical, "live-owner", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.AcquireLeaseFromAliases(context.Background(), aliases, canonical, "foreign-owner", "worker", "worker"); err == nil {
		t.Fatal("live owner on the recognized worktree alias must block a second approval lease")
	}
	if _, _, err := stack.Leases.Release(context.Background(), owner.LeaseKey, owner.OwnerID, owner.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	next, err := stack.AcquireLeaseFromAliases(context.Background(), aliases, canonical, "next-owner", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != 2 {
		t.Fatalf("released generation did not continue across worktree cwd: got %d want 2", next.Generation)
	}
	foreignProject := provider.LeaseKey(root, "memory", "foreign-project", "FAC-782:review")
	if _, err := stack.AcquireLease(context.Background(), foreignProject, "foreign-project-owner", "worker", "worker"); err != nil {
		t.Fatalf("foreign project row should remain isolated, not join the approval key: %v", err)
	}
	foreignTask := provider.LeaseKey(root, "memory", "proj", "FAC-782:other")
	foreignAliases := append(append([]claim.LeaseKey(nil), aliases...), foreignTask)
	if _, err := stack.AcquireLeaseFromAliases(context.Background(), foreignAliases, canonical, "foreign-task-owner", "worker", "worker"); err == nil {
		t.Fatal("foreign task alias must be refused")
	}
	if _, err := approvalLeaseAliases(context.Background(), root, "foreign-repository", "herdforge-test", "memory", "proj", "FAC-782:review"); err == nil {
		t.Fatal("foreign repository identity must be refused")
	}
	if _, err := approvalLeaseAliases(context.Background(), filepath.Join(t.TempDir(), "unregistered"), "unregistered-repository", "herdforge-test", "memory", "proj", "FAC-782:review"); err == nil {
		t.Fatal("unregistered repository identity must be refused")
	}
	if owner.Generation != 1 {
		t.Fatalf("first approval generation=%d, want 1", owner.Generation)
	}
}

func TestApprovalLeaseKey_ContinuesRecognizedLegacyHistory(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "seed"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "seed")
	gitIn(t, root, "commit", "-m", "chore: seed")
	coordinatorCWD := filepath.Join(t.TempDir(), "registered-coordinator")
	otherCWD := filepath.Join(t.TempDir(), "registered-other")
	gitIn(t, root, "worktree", "add", "-b", "approval-coordinator", coordinatorCWD, "main")
	gitIn(t, root, "worktree", "add", "-b", "approval-other", otherCWD, "main")

	const providerType, projectID, taskRef = "memory", "proj", "FAC-752:review"
	repository := dispatch.RepositoryIdentityOrName(root, "herdforge-test")
	// Exact pre-FAC-782 shape: an opaque identity was passed to LeaseKey while
	// cwd was the registered coordinator worktree, so filepath.Abs bound it to
	// that worktree instead of the repository common root.
	coordinatorPath, err := filepath.EvalSymlinks(coordinatorCWD)
	if err != nil {
		t.Fatal(err)
	}
	legacyKey := claim.LeaseKey{Repo: filepath.Join(coordinatorPath, repository), Provider: providerType, Project: projectID, TaskRef: taskRef}
	stack := provider.NewTestStack(t, provider.NewMemoryProvider())
	for generation := 1; generation <= 2; generation++ {
		lease, err := stack.AcquireLease(context.Background(), legacyKey, fmt.Sprintf("legacy-owner-%d", generation), "worker", "worker")
		if err != nil {
			t.Fatal(err)
		}
		if lease.Generation != int64(generation) {
			t.Fatalf("legacy generation=%d want %d", lease.Generation, generation)
		}
		if _, _, err := stack.Leases.Release(context.Background(), legacyKey, lease.OwnerID, lease.Generation, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	aliasesFromCanonical, err := approvalLeaseAliases(context.Background(), root, repository, "herdforge-test", providerType, projectID, taskRef)
	if err != nil {
		t.Fatalf("canonical cwd legacy recovery inventory: %v", err)
	}
	next, err := stack.AcquireLeaseFromAliases(context.Background(), aliasesFromCanonical, provider.LeaseKey(root, providerType, projectID, taskRef), "recovered-owner", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != 3 || next.LeaseKey != legacyKey {
		t.Fatalf("recovery generation=%d want 3", next.Generation)
	}
	if _, _, err := stack.Leases.Release(context.Background(), next.LeaseKey, next.OwnerID, next.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}

	aliasesFromOther, err := approvalLeaseAliases(context.Background(), otherCWD, repository, "herdforge-test", providerType, projectID, taskRef)
	if err != nil {
		t.Fatalf("registered other cwd legacy recovery: %v", err)
	}
	live, err := stack.AcquireLeaseFromAliases(context.Background(), aliasesFromOther, provider.LeaseKey(otherCWD, providerType, projectID, taskRef), "live-legacy-owner", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.AcquireLeaseFromAliases(context.Background(), aliasesFromOther, provider.LeaseKey(otherCWD, providerType, projectID, taskRef), "blocked-owner", "worker", "worker"); err == nil || !strings.Contains(err.Error(), "active owner") {
		t.Fatalf("live recognized legacy owner was not refused: %v", err)
	}
	_ = live
}

func TestApprovalLeaseKey_RefusesAmbiguousRecognizedLegacyHistory(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "seed"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "seed")
	gitIn(t, root, "commit", "-m", "chore: seed")
	wtA := filepath.Join(t.TempDir(), "registered-a")
	wtB := filepath.Join(t.TempDir(), "registered-b")
	gitIn(t, root, "worktree", "add", "-b", "approval-a", wtA, "main")
	gitIn(t, root, "worktree", "add", "-b", "approval-b", wtB, "main")
	repository := dispatch.RepositoryIdentityOrName(root, "herdforge-test")
	stack := provider.NewTestStack(t, provider.NewMemoryProvider())
	for _, cwd := range []string{wtA, wtB} {
		resolvedCWD, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			t.Fatal(err)
		}
		key := claim.LeaseKey{Repo: filepath.Join(resolvedCWD, repository), Provider: "memory", Project: "proj", TaskRef: "FAC-782:review"}
		lease, err := stack.AcquireLease(context.Background(), key, cwd, "worker", "worker")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := stack.Leases.Release(context.Background(), key, lease.OwnerID, lease.Generation, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	aliases, err := approvalLeaseAliases(context.Background(), root, repository, "herdforge-test", "memory", "proj", "FAC-782:review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.AcquireLeaseFromAliases(context.Background(), aliases, provider.LeaseKey(root, "memory", "proj", "FAC-782:review"), "ambiguous-owner", "worker", "worker"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("equal recognized legacy histories were not refused: %v", err)
	}
}

func TestRequireLiveLeaseReacquiresReviewLeaseAfterWorkerRelease(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	store, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: "FAC-1723"}
	worker, err := store.Acquire(context.Background(), key, "coordinator-worker", dispatch.RoleWorker, "", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Release(context.Background(), key, worker.OwnerID, worker.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	tc := dispatch.TaskContext{
		ProviderType: "memory", ProjectID: "project", Repository: "repo", Role: dispatch.RoleWorker,
		TaskRef: "FAC-1723", TaskID: "task-1723", Branch: "worker", BaseSHA: "base",
		LeaseID: fmt.Sprintf("claim:%d", worker.ID), LeaseGeneration: worker.Generation,
		LeaseTaskRef: "FAC-1723", SessionID: "session", AllowedOps: dispatch.WorkerOps,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := requireLiveLease(context.Background(), root, tc); err != nil {
		t.Fatal(err)
	}
	review, err := store.CurrentLease(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: reviewLeaseTaskRef("FAC-1723")})
	if err != nil {
		t.Fatal(err)
	}
	if review == nil || review.Status != claim.StatusActive || review.Role != dispatch.RoleWorker {
		t.Fatalf("review lease not active: %+v", review)
	}
}

func TestOpenCompletionGateAnchorsStoresAtCommonRoot(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	caller := filepath.Join(root, "nested", "lane")
	if err := os.MkdirAll(caller, 0755); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(caller); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(previous) }()

	cfg := &config.Config{Verification: config.Verification{TestCommand: "true"}}
	gate, machine, err := openCompletionGate(cfg)
	if err != nil {
		t.Fatalf("open completion gate: %v", err)
	}
	if gate == nil || machine == nil {
		t.Fatal("completion gate returned nil authority")
	}
	defer machine.Close()
	if _, err := os.Stat(filepath.Join(root, defaultLifecycleDB)); err != nil {
		t.Fatalf("common-root lifecycle store missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(caller, defaultLifecycleDB)); !os.IsNotExist(err) {
		t.Fatalf("nested lifecycle store created from caller cwd: %v", err)
	}
	canonicalRoot, err := canonicalHerdRoot()
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptDir := filepath.Join(canonicalRoot, defaultReceiptDir)
	if filepath.Clean(gate.Store.(*verifier.FileReceiptStore).Dir) != wantReceiptDir {
		t.Fatalf("receipt store dir = %q, want common root %q", gate.Store.(*verifier.FileReceiptStore).Dir, wantReceiptDir)
	}
}

func TestBindingForWorktree_UsesAuthenticatedReceiptForLegacyLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".herd/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".gitignore")
	gitIn(t, root, "commit", "-m", "chore: ignore runtime state")
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "candidate.txt")
	gitIn(t, root, "commit", "-m", "feat: legacy candidate")

	keyDir := t.TempDir()
	signer := fixtureSigner(t, keyDir, root)
	tc := dispatch.TaskContext{
		ProviderType: "kaneo", ProjectID: "proj-x", Repository: dispatch.RepositoryIdentityOrName(root, "herdforge-test"),
		Role: dispatch.RoleWorker, TaskRef: "FAC-326", TaskID: "task-326", Branch: "herd/fac-326", BaseSHA: "base",
		LeaseID: "claim:326", LeaseGeneration: 7, LeaseTaskRef: "FAC-326", SessionID: "legacy-worker",
		AllowedOps: dispatch.WorkerOps, ExpiresAt: time.Now().Add(time.Hour),
	}
	signed, err := signer.Issue(tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(root, signed); err != nil {
		t.Fatal(err)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()

	bind, err := bindingForWorktreeAtRoot(nil, machine, "FAC-326", root, root)
	if err != nil {
		t.Fatalf("legacy receipt fallback refused: %v", err)
	}
	if bind.LeaseGeneration != 7 || bind.Branch != tc.Branch || bind.Repo != tc.Repository {
		t.Fatalf("binding did not inherit authenticated receipt: %+v", bind)
	}
}

func TestBindingForWorktree_PreservesWorkerAuthorityAcrossReviewerRetries(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".herd/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".gitignore")
	gitIn(t, root, "commit", "-m", "chore: ignore runtime state")
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "candidate.txt")
	gitIn(t, root, "commit", "-m", "feat: candidate")

	keyDir := t.TempDir()
	signer := fixtureSigner(t, keyDir, root)
	worker := dispatch.TaskContext{
		ProviderType: "kaneo", ProjectID: "proj-x", Repository: dispatch.RepositoryIdentityOrName(root, "herdforge-test"),
		Role: dispatch.RoleWorker, TaskRef: "FAC-343", TaskID: "task-343", Branch: "herd/fac-343", BaseSHA: "base",
		LeaseID: "claim:2", LeaseGeneration: 2, LeaseTaskRef: "FAC-343", SessionID: "worker-session",
		AllowedOps: dispatch.WorkerOps, ExpiresAt: time.Now().Add(time.Hour),
	}
	signedWorker, err := signer.Issue(worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(root, signedWorker); err != nil {
		t.Fatal(err)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	for _, retry := range []struct {
		name       string
		generation int64
		leaseID    string
	}{
		{name: "first review", generation: 4, leaseID: "claim:4"},
		{name: "second review retry", generation: 5, leaseID: "claim:5"},
	} {
		t.Run(retry.name, func(t *testing.T) {
			// Each later review launch leaves a newer canonical reviewer receipt.
			// It must not replace the worker lease owning verification evidence.
			reviewer := worker
			reviewer.Role = dispatch.RoleReviewer
			reviewer.CandidateSHA = gitIn(t, root, "rev-parse", "HEAD")
			reviewer.LeaseID = retry.leaseID
			reviewer.LeaseGeneration = retry.generation
			reviewer.LeaseTaskRef = reviewLeaseTaskRef("FAC-343")
			reviewer.SessionID = retry.name
			signedReviewer, err := signer.Issue(reviewer)
			if err != nil {
				t.Fatal(err)
			}
			if err := dispatch.StoreCanonicalReceipt(root, signedReviewer); err != nil {
				t.Fatal(err)
			}

			bind, err := bindingForWorktreeAtRoot(nil, machine, "FAC-343", root, root)
			if err != nil {
				t.Fatalf("worker receipt fallback refused: %v", err)
			}
			if bind.LeaseGeneration != worker.LeaseGeneration {
				t.Fatalf("binding used reviewer generation %d, want worker generation %d", bind.LeaseGeneration, worker.LeaseGeneration)
			}
		})
	}
}

func TestUseHarnessHooksFromWorktreeUsesCandidatePolicyByDefault(t *testing.T) {
	wt := t.TempDir()
	hooks := filepath.Join(wt, ".herd", "harness-hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooks), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks, []byte(`{"providers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_HARNESS_HOOKS_FILE", "")
	restore := useHarnessHooksFromWorktree(wt)
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != hooks {
		t.Fatalf("hook policy path = %q, want %q", got, hooks)
	}
	restore()
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != "" {
		t.Fatalf("hook policy override not restored: %q", got)
	}
}

func TestUseHarnessHooksFromWorktreePreservesExplicitOverride(t *testing.T) {
	wt := t.TempDir()
	hooks := filepath.Join(wt, ".herd", "harness-hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooks), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks, []byte(`{"providers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_HARNESS_HOOKS_FILE", "explicit-policy.json")
	restore := useHarnessHooksFromWorktree(wt)
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != "explicit-policy.json" {
		t.Fatalf("explicit hook policy path changed: %q", got)
	}
	restore()
}

// TestStandingHookPolicyScopeUsesLaneWorktreePolicy is FAC-624's `standing`
// defect: standing's AdmitRoute admits through the same launchAdmission ->
// preflightHooks -> harness.DefaultDiscovery chain `herd up` does, and had
// the identical unscoped-cwd gap FAC-767/185679cd fixed for `up`.
func TestStandingHookPolicyScopeUsesLaneWorktreePolicy(t *testing.T) {
	// lane.Worktree is configured relative (e.g. "." or "target-worktree"),
	// exactly like runUpCommand's cwd := filepath.Join(".", lane.Worktree) --
	// a lane never carries an absolute worktree path in production.
	root := t.TempDir()
	t.Chdir(root)
	worktreeRel := "target-worktree"
	worktreeAbs := filepath.Join(root, worktreeRel)
	hooks := filepath.Join(worktreeAbs, ".herd", "harness-hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooks), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks, []byte(`{"providers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_HARNESS_HOOKS_FILE", "")
	lane := &config.LaneDef{Name: "chain-indexer", Worktree: worktreeRel}
	restore := laneHookPolicyScope(lane)
	wantRel := filepath.Join(worktreeRel, ".herd", "harness-hooks.json")
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != wantRel {
		t.Fatalf("standing admission did not scope hook policy to the lane's own worktree: got %q want %q", got, wantRel)
	}
	restore()
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != "" {
		t.Fatalf("standing hook policy scoping was not restored: %q", got)
	}
}

// TestStandingHookPolicyScopePreservesExplicitOverride mirrors up's boundary:
// an operator's explicit HERD_HARNESS_HOOKS_FILE must still win over a
// standing lane's own worktree pin.
func TestStandingHookPolicyScopePreservesExplicitOverride(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	worktreeRel := "target-worktree"
	hooks := filepath.Join(root, worktreeRel, ".herd", "harness-hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooks), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks, []byte(`{"providers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_HARNESS_HOOKS_FILE", "explicit-policy.json")
	lane := &config.LaneDef{Name: "chain-indexer", Worktree: worktreeRel}
	restore := laneHookPolicyScope(lane)
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != "explicit-policy.json" {
		t.Fatalf("standing scoping clobbered an explicit operator override: %q", got)
	}
	restore()
}

func TestRecordForgeLifecycle_ProjectsClaimThroughBuilding(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	tok := &deps.OwnershipToken{Generation: 3}
	if err := recordForgeLifecycle(root, "FAC-326", "herdforge", tok, "herd/fac-326", "base-sha"); err != nil {
		t.Fatal(err)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	state, err := machine.EventStore().CurrentState("FAC-326")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.State != lifecycle.StateBuilding || state.LeaseGeneration != 3 {
		t.Fatalf("forge lifecycle state = %+v, want building/gen3", state)
	}
	if err := recordForgeLifecycle(root, "FAC-326", "herdforge", tok, "herd/fac-326", "base-sha"); err != nil {
		t.Fatalf("idempotent lifecycle projection: %v", err)
	}
}

func TestRecoverVerificationDigest_RestampsLegacyPassReceipt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd", "verification-receipts"), 0755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".herd/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".gitignore")
	gitIn(t, root, "commit", "-m", "chore: ignore runtime state")
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "candidate.txt")
	gitIn(t, root, "commit", "-m", "feat: receipt candidate")
	sha := gitIn(t, root, "rev-parse", "HEAD")
	receipt := verifier.Receipt{
		Version: 1, TaskRef: "FAC-327", LeaseGeneration: "7", CandidateSHA: sha,
		Command: []string{"true"}, Outcome: verifier.OutcomePASS,
		EnvironmentPolicy: verifier.EnvironmentPolicyInherited,
	}
	receipt.Digest = receipt.ComputeDigest()
	legacy := receipt
	legacy.Digest = ""
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "verification-receipts", "legacy.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	digest, err := recoverVerificationDigest(context.Background(), root, "FAC-327", root, sha, 7)
	if err != nil {
		t.Fatalf("legacy digest recovery: %v", err)
	}
	if digest != receipt.Digest {
		t.Fatalf("digest = %q, want %q", digest, receipt.Digest)
	}
	store, err := verifier.NewFileReceiptStore(filepath.Join(root, ".herd", "verification-receipts"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), digest)
	if err != nil || loaded.Digest != digest {
		t.Fatalf("restamped receipt load = %+v, %v", loaded, err)
	}
}

func TestRecoverVerificationDigest_MissingStoreIsCleanRefusal(t *testing.T) {
	root := t.TempDir()
	_, err := recoverVerificationDigest(context.Background(), root, "FAC-327", root, "deadbeef", 1)
	if err == nil || err.Error() != "no legacy verification receipt found for FAC-327" {
		t.Fatalf("missing store error = %v, want clean legacy refusal", err)
	}
}
