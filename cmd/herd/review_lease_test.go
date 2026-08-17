package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

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

func TestBindingForWorktree_PrefersWorkerContextOverNewerReviewerReceipt(t *testing.T) {
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
		Role: dispatch.RoleWorker, TaskRef: "FAC-340", TaskID: "task-340", Branch: "herd/fac-340", BaseSHA: "base",
		LeaseID: "claim:2", LeaseGeneration: 2, LeaseTaskRef: "FAC-340", SessionID: "worker-session",
		AllowedOps: dispatch.WorkerOps, ExpiresAt: time.Now().Add(time.Hour),
	}
	signedWorker, err := signer.Issue(worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(root, signedWorker); err != nil {
		t.Fatal(err)
	}
	// A later review launch leaves a newer canonical reviewer receipt. It
	// must not replace the worker lease that owns the verification evidence.
	reviewer := worker
	reviewer.Role = dispatch.RoleReviewer
	reviewer.CandidateSHA = gitIn(t, root, "rev-parse", "HEAD")
	reviewer.LeaseID = "claim:4"
	reviewer.LeaseGeneration = 4
	reviewer.LeaseTaskRef = reviewLeaseTaskRef("FAC-340")
	reviewer.SessionID = "reviewer-session"
	signedReviewer, err := signer.Issue(reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.StoreCanonicalReceipt(root, signedReviewer); err != nil {
		t.Fatal(err)
	}

	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	bind, err := bindingForWorktreeAtRoot(nil, machine, "FAC-340", root, root)
	if err != nil {
		t.Fatalf("worker receipt fallback refused: %v", err)
	}
	if bind.LeaseGeneration != worker.LeaseGeneration {
		t.Fatalf("binding used reviewer generation %d, want worker generation %d", bind.LeaseGeneration, worker.LeaseGeneration)
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
