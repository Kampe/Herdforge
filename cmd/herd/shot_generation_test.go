package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

func validShotGenerationFacts() shotGenerationFacts {
	return shotGenerationFacts{
		shotSupersessionFacts: validShotSupersessionFacts(),
		PriorLeaseGeneration:  1,
		PriorCandidateSHA:     shotLifecycleTestSHA,
		PriorSequence:         1,
		PriorState:            lifecycle.StateEligible,
	}
}

func TestValidateShotGenerationFactsRejectsRollbackUnsignedStaleAndUnknown(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*shotGenerationFacts)
	}{
		{"generation rollback", func(f *shotGenerationFacts) { f.ReportedLease = 1; f.LeaseGeneration = 1 }},
		{"generation jump without live lease", func(f *shotGenerationFacts) { f.LeaseLive = false }},
		{"unsigned context", func(f *shotGenerationFacts) { f.ReceiptVerified = false }},
		{"signature session mismatch", func(f *shotGenerationFacts) { f.LaunchSession = "other" }},
		{"stale candidate", func(f *shotGenerationFacts) { f.GitHeadSHA = "cccccccccccccccccccccccccccccccccccccccc" }},
		{"dirty worktree", func(f *shotGenerationFacts) { f.Clean = false }},
		{"provider unknown", func(f *shotGenerationFacts) { f.ProviderStatus = "unknown" }},
		{"wrong branch", func(f *shotGenerationFacts) { f.GitBranch = "herd/wrong" }},
		{"wrong worktree", func(f *shotGenerationFacts) { f.RegisteredWorktree = "./.herd/worktrees/wrong" }},
		{"wrong base", func(f *shotGenerationFacts) { f.GitBaseSHA = "cccccccccccccccccccccccccccccccccccccccc" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := validShotGenerationFacts()
			facts.ReportedLease = 2
			facts.LeaseGeneration = 2
			facts.ReplacementSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			facts.GitHeadSHA = facts.ReplacementSHA
			tc.mutate(&facts)
			if err := validateShotGenerationFacts(facts); err == nil {
				t.Fatal("invalid generation reconcile facts were accepted")
			}
		})
	}
}

func TestValidateShotGenerationFactsAcceptsNewerSignedWorkerCallback(t *testing.T) {
	facts := validShotGenerationFacts()
	facts.ReportedLease = 2
	facts.LeaseGeneration = 2
	facts.ReplacementSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	facts.GitHeadSHA = facts.ReplacementSHA
	if err := validateShotGenerationFacts(facts); err != nil {
		t.Fatal(err)
	}
}

func TestRecordShotLifecycleLeaseDrivesNewerGenerationReconcilePath(t *testing.T) {
	root := t.TempDir()
	oldSHA := shotLifecycleTestSHA
	if err := recordShotLifecycleLease(root, "FAC-738", 1, oldSHA); err != nil {
		t.Fatal(err)
	}
	original := runShotGenerationReconcile
	t.Cleanup(func() { runShotGenerationReconcile = original })
	called := false
	newSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	runShotGenerationReconcile = func(_ context.Context, gotRoot, gotRef string, gotLease int64, gotSHA string, _ *lifecycle.Machine, current *lifecycle.TaskState) error {
		called = true
		if gotRoot != root || gotRef != "FAC-738" || gotLease != 2 || gotSHA != newSHA {
			t.Fatalf("generation reconcile args root=%q ref=%q lease=%d sha=%q", gotRoot, gotRef, gotLease, gotSHA)
		}
		if current == nil || current.LeaseGeneration != 1 || current.CandidateSHA != oldSHA {
			t.Fatalf("generation reconcile current state = %+v", current)
		}
		return nil
	}
	if err := recordShotLifecycleLease(root, "FAC-738", 2, newSHA); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("newer signed worker callback did not drive generation reconcile")
	}
}

func TestRecordShotLifecycleLeaseGenerationRollbackDoesNotMutate(t *testing.T) {
	root := t.TempDir()
	if err := recordShotLifecycleLease(root, "FAC-738", 2, shotLifecycleTestSHA); err != nil {
		t.Fatal(err)
	}
	if err := recordShotLifecycleLease(root, "FAC-738", 1, shotLifecycleTestSHA); err == nil {
		t.Fatal("generation rollback was accepted")
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	state, err := machine.EventStore().CurrentState("FAC-738")
	if err != nil || state == nil || state.LeaseGeneration != 2 || state.CandidateSHA != shotLifecycleTestSHA {
		t.Fatalf("rollback mutated lifecycle: %+v err=%v", state, err)
	}
}

func TestNewerSignedWorkerCallbackAdmitsExistingGenerationTwoPASS(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=t@example.com",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "-q", "-b", "main")
	run(root, "config", "user.email", "t@example.com")
	run(root, "config", "user.name", "test")
	run(root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".herd/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(root, "add", ".gitignore", "README", "ok.sh")
	run(root, "commit", "-q", "-m", "old")
	oldSHA := run(root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(root, "add", "README")
	run(root, "commit", "-q", "-m", "new")
	newSHA := run(root, "rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o700); err != nil {
		t.Fatal(err)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	if _, err := machine.Transition(lifecycle.TransitionRequest{
		TaskRef: "FAC-738", Repo: "herdforge", To: lifecycle.StateEligible, Actor: "worker-session",
		IdempotencyKey: "fac-738:eligible", LeaseGeneration: 1, Branch: "herd/fac-738", CandidateSHA: oldSHA,
	}); err != nil {
		t.Fatal(err)
	}

	v := verifier.NewVerifierArgs([]string{"./ok.sh"})
	receiptDir := filepath.Join(root, ".herd", "verification-receipts")
	store, err := verifier.NewFileReceiptStore(receiptDir)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := v.VerifyAndPersist(context.Background(), root, verifier.VerificationRequest{
		TaskRef: "FAC-738", LeaseGeneration: "2", CandidateSHA: newSHA, EnvironmentPolicy: verifier.EnvironmentPolicyInherited,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Outcome != verifier.OutcomePASS {
		t.Fatalf("want PASS receipt, got %+v", pass)
	}

	gate, err := daemon.NewCompletionGate(v, receiptDir, machine)
	if err != nil {
		t.Fatal(err)
	}
	staleBind := daemon.CompletionBinding{
		TaskRef: "FAC-738", Repo: "herdforge", LeaseGeneration: 1, CandidateSHA: newSHA, Branch: "herd/fac-738", WorktreeDir: root,
	}
	if _, err := gate.AdmitReview(context.Background(), staleBind, pass.Digest); !errors.Is(err, daemon.ErrBindingMismatch) {
		t.Fatalf("generation-1 lifecycle must refuse generation-2 PASS, got %v", err)
	}

	req := lifecycle.WorkerGenerationReconcileRequest{
		TaskRef: "FAC-738", TaskID: "task-id", ProjectID: "project-id", Repo: "herdforge",
		ExpectedSequence: 1, OldLeaseGeneration: 1, NewLeaseGeneration: 2,
		Branch: "herd/fac-738", BaseSHA: oldSHA, OldCandidateSHA: oldSHA, NewCandidateSHA: newSHA,
		Worktree: "./.herd/worktrees/fac-738", Actor: "worker-session", SessionID: "worker-session",
		BuilderSession: "worker-session", BuilderModel: "gpt-test", BuilderFamily: "openai",
		IdempotencyKey: "fac-738:generation-reconcile:2:" + newSHA,
	}
	_, digest, err := lifecycle.EncodeWorkerGenerationReconcileEvidence(lifecycle.WorkerGenerationReconcileEvidence{
		TaskID: req.TaskID, ProjectID: req.ProjectID, BaseSHA: req.BaseSHA, Worktree: req.Worktree,
		OldLeaseGeneration: req.OldLeaseGeneration, NewLeaseGeneration: req.NewLeaseGeneration,
		OldCandidateSHA: req.OldCandidateSHA, NewCandidateSHA: req.NewCandidateSHA,
		PriorSequence: req.ExpectedSequence, SessionID: req.SessionID,
		BuilderSession: req.BuilderSession, BuilderModel: req.BuilderModel, BuilderFamily: req.BuilderFamily,
	})
	if err != nil {
		t.Fatal(err)
	}
	req.EvidenceDigest = digest
	if _, err := machine.ReconcileWorkerGeneration(req); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.ReconcileWorkerGeneration(req); err != nil {
		t.Fatal(err)
	}
	events, err := machine.EventStore().Events("FAC-738")
	if err != nil || len(events) != 2 {
		t.Fatalf("retry duplicated history: events=%+v err=%v", events, err)
	}
	current, err := machine.EventStore().CurrentState("FAC-738")
	if err != nil || current == nil || current.LeaseGeneration != 2 || current.CandidateSHA != newSHA {
		t.Fatalf("reconciled state = %+v err=%v", current, err)
	}
	freshBind := daemon.CompletionBinding{
		TaskRef: "FAC-738", Repo: "herdforge", LeaseGeneration: current.LeaseGeneration, CandidateSHA: newSHA, Branch: "herd/fac-738", WorktreeDir: root,
	}
	if _, err := gate.AdmitReview(context.Background(), freshBind, pass.Digest); err != nil {
		t.Fatalf("existing generation-2 PASS was not review-admissible: %v", err)
	}
}

func TestShotGenerationReconcileHashesCompleteGenerationEvidence(t *testing.T) {
	source, err := os.ReadFile("shot_generation.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(source)
	if strings.Contains(src, "EncodeCandidateSupersessionEvidence(facts)") {
		t.Fatal("generation reconcile digest hashes narrower shotSupersessionFacts")
	}
	if strings.Contains(src, "shot: lifecycle lease generation %d conflicts with reported %d") {
		t.Fatal("generation reconcile reimplemented the shot lease-generation conflict message")
	}
	if !strings.Contains(src, "shotLeaseGenerationConflict") {
		t.Fatal("generation reconcile must use the canonical shot lease-generation conflict owner")
	}
	for _, needle := range []string{
		"EncodeWorkerGenerationReconcileEvidence",
		"WorkerGenerationReconcileEvidence{",
		"OldLeaseGeneration:",
		"NewLeaseGeneration:",
		"OldCandidateSHA:",
		"NewCandidateSHA:",
		"PriorSequence:",
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("removing %q would unbind generation fences from the evidence digest", needle)
		}
	}
}
