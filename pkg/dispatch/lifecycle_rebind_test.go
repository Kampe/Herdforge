package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/envplan"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/runstate"
)

type recoveredLifecycleFixture struct {
	rebinder      *LifecycleGenerationRebinder
	request       RecoveredLifecycleRequest
	lifecyclePath string
	oldCandidate  string
}

type refusingRecoveredLifecycle struct {
	mu      sync.Mutex
	calls   int
	request RecoveredLifecycleRequest
	err     error
}

func (f *refusingRecoveredLifecycle) RebindRecovered(_ context.Context, req RecoveredLifecycleRequest) (*lifecycle.GenerationRebindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.request = req
	if _, err := os.Stat(filepath.Join(req.WorktreePath, "TASK-PACKET.md")); err != nil {
		return nil, fmt.Errorf("packet was not durable before generation fence: %w", err)
	}
	if _, err := os.Stat(filepath.Join(req.WorktreePath, TaskContextFile)); err != nil {
		return nil, fmt.Errorf("receipt was not durable before generation fence: %w", err)
	}
	return nil, f.err
}

func TestRecoveredDispatchMustRebindAfterDurablePacketBeforeLauncher(t *testing.T) {
	ctx := context.Background()
	repo, worktrees := initDispatchRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".herd"), 0o700); err != nil {
		t.Fatal(err)
	}
	task := &provider.Task{
		ID: "fac-614-id", Ref: "FAC-614", ProjectID: "test", Title: "recovered lifecycle",
		Status: provider.StatusToDo, UpdatedAt: time.Unix(100, 0).UTC(),
		Description: emptyDepsFence("FAC-614", "fac-614-id"),
	}
	tasks := &statusTrackingProvider{mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{task}}}
	states, err := runstate.Open(filepath.Join(repo, ".herd", "dispatch-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = states.Close() })
	stale, err := runstate.FromTasks("dispatch:"+task.ID, "dispatch", task.Ref, "graph-generation-1", runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := states.Checkpoint(ctx, stale, 0); err != nil {
		t.Fatal(err)
	}
	recovered, err := runstate.FromTasks("dispatch:"+task.ID, "dispatch", task.Ref, "graph-generation-2", runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	recovered.Recovery = &runstate.StaleRecovery{
		FromRevision: 1, TaskRef: task.Ref, TaskID: task.ID, ProjectID: task.ProjectID,
		ProviderRevision: string(provider.EncodeRevision(task)), GraphRevision: "graph-generation-2",
	}
	if _, err := states.Checkpoint(ctx, recovered, 1); err != nil {
		t.Fatal(err)
	}

	plans, err := envplan.Open(filepath.Join(repo, ".herd", "environment-plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plans.Close() })
	now := time.Now().UTC()
	binding := envplan.Binding{
		TaskRef: task.Ref, TaskID: task.ID, Provider: "memory",
		ProviderRevision: string(provider.EncodeRevision(task)), GraphRevision: "graph-generation-2",
		RunID: "dispatch:" + task.ID, RunRevision: 2, RecoveryFromRevision: 1,
	}
	plan, err := plans.Create(ctx, envplan.Plan{
		Binding: binding,
		Requests: []envplan.Request{
			{Capability: envplan.CapabilityBoardWrite, Evidence: envplan.Evidence{Authority: "security", Revision: "fac-669", Subject: task.Ref}},
			{Capability: envplan.CapabilityCredential, Evidence: envplan.Evidence{Authority: "harness", Revision: "fac-669", Subject: task.Ref}},
		},
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []envplan.Capability{envplan.CapabilityBoardWrite, envplan.CapabilityCredential} {
		if _, err := plans.Grant(ctx, plan.ID, capability, "coordinator", now.Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	refusal := errors.New("injected lifecycle generation refusal")
	authority := &refusingRecoveredLifecycle{err: refusal}
	herdr := &fakeHerdr{available: true, workspace: "fac-669", model: "deepseek-v4-flash"}
	compensator := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tasks, worktrees)
	d.Compensator = compensator
	d.Herdr = herdr
	d.Ownership = &fixedGenerationOwnership{generation: 2}
	d.RunStates = states
	d.RunStateGraph = func(context.Context) (string, error) { return "graph-generation-2", nil }
	d.EnvironmentPlans = plans
	d.RecoveredLifecycle = authority
	opts := leasedLaunchOptions(t, task.Ref)
	opts.EnvironmentPlanID = plan.ID
	opts.LeaseID = "claim:68"
	opts.LeaseGeneration = 2
	result, err := d.Dispatch(ctx, opts)
	if !errors.Is(err, refusal) {
		t.Fatalf("dispatch error=%v, want lifecycle refusal", err)
	}
	if result != nil {
		t.Fatalf("recovered dispatch became launchable before lifecycle rebind: %+v", result)
	}
	authority.mu.Lock()
	calls, request := authority.calls, authority.request
	authority.mu.Unlock()
	if calls != 1 || !request.Binding.Recovered() || request.EnvironmentPlanID != plan.ID ||
		request.TaskID != task.ID || request.TaskRef != task.Ref || request.LeaseID != "claim:68" || request.LeaseGeneration != 2 {
		t.Fatalf("generation authority calls=%d request=%+v", calls, request)
	}
	if herdr.startCalls != 0 || herdr.tabCwd != "" {
		t.Fatalf("lifecycle refusal crossed launcher boundary: starts=%d cwd=%q", herdr.startCalls, herdr.tabCwd)
	}
	if !hasCompensateReason(compensator.compsCopy(), "lifecycle_generation_rebind_failed") {
		t.Fatalf("lifecycle refusal was not durably compensated: %+v", compensator.compsCopy())
	}
	if request.WorktreePath != "" {
		t.Cleanup(func() {
			_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", request.WorktreePath).Run()
		})
	}
}

func TestRecoveredLifecycleRebindProductionHistoryAndIdempotence(t *testing.T) {
	fixture := newRecoveredLifecycleFixture(t)

	first, err := fixture.rebinder.RebindRecovered(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.State.LeaseGeneration != 2 || first.State.CandidateSHA != fixture.oldCandidate {
		t.Fatalf("fresh rebind=%+v", first)
	}
	retry, err := fixture.rebinder.RebindRecovered(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Replayed || retry.State != first.State {
		t.Fatalf("idempotent retry=%+v first=%+v", retry, first)
	}

	machine, err := lifecycle.NewMachine(fixture.lifecyclePath)
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	events, err := machine.EventStore().Events("FAC-614")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].LeaseGeneration != 1 || events[0].CandidateSHA != fixture.oldCandidate ||
		events[1].ToState != lifecycle.StateRecovering || events[2].ToState != lifecycle.StateEligible ||
		events[1].CandidateSHA != fixture.oldCandidate || events[2].CandidateSHA != fixture.oldCandidate {
		t.Fatalf("immutable production history=%+v", events)
	}
}

func TestRecoveredLifecycleRebindExactIdentityRefusalsDoNotMutate(t *testing.T) {
	fixture := newRecoveredLifecycleFixture(t)
	tests := []struct {
		name   string
		mutate func(*RecoveredLifecycleRequest)
	}{
		{name: "environment plan", mutate: func(r *RecoveredLifecycleRequest) { r.EnvironmentPlanID = "env-mixed" }},
		{name: "provider", mutate: func(r *RecoveredLifecycleRequest) { r.ProviderType = "github" }},
		{name: "project", mutate: func(r *RecoveredLifecycleRequest) { r.ProjectID = "foreign-project" }},
		{name: "task id", mutate: func(r *RecoveredLifecycleRequest) { r.TaskID = "foreign-task" }},
		{name: "task ref", mutate: func(r *RecoveredLifecycleRequest) { r.TaskRef = "FAC-999" }},
		{name: "repository", mutate: func(r *RecoveredLifecycleRequest) { r.Repository = "foreign-repository" }},
		{name: "lifecycle repository", mutate: func(r *RecoveredLifecycleRequest) { r.LifecycleRepository = "foreign-lifecycle-repository" }},
		{name: "provider revision", mutate: func(r *RecoveredLifecycleRequest) { r.Binding.ProviderRevision = "mixed-provider-revision" }},
		{name: "graph revision", mutate: func(r *RecoveredLifecycleRequest) { r.Binding.GraphRevision = "mixed-graph-revision" }},
		{name: "run revision", mutate: func(r *RecoveredLifecycleRequest) { r.Binding.RunRevision = 3 }},
		{name: "ordinary fresh binding", mutate: func(r *RecoveredLifecycleRequest) { r.Binding.RecoveryFromRevision = 0 }},
		{name: "lease", mutate: func(r *RecoveredLifecycleRequest) { r.LeaseID = "claim:999" }},
		{name: "generation downgrade", mutate: func(r *RecoveredLifecycleRequest) { r.LeaseGeneration = 1 }},
		{name: "branch", mutate: func(r *RecoveredLifecycleRequest) { r.Branch = "herd/fac-999" }},
		{name: "worktree head", mutate: func(r *RecoveredLifecycleRequest) { r.WorktreeHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{name: "base", mutate: func(r *RecoveredLifecycleRequest) { r.BaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{name: "anchor", mutate: func(r *RecoveredLifecycleRequest) { r.AnchorRef = "refs/herd/anchors/fac-999" }},
		{name: "packet", mutate: func(r *RecoveredLifecycleRequest) { r.TaskPacket += "\nforeign packet" }},
		{name: "signed receipt", mutate: func(r *RecoveredLifecycleRequest) { r.NewReceipt.TaskID = "foreign-task" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := fixture.request
			tt.mutate(&req)
			if _, err := fixture.rebinder.RebindRecovered(context.Background(), req); !errors.Is(err, ErrRecoveredLifecycleRefused) {
				t.Fatalf("err=%v, want ErrRecoveredLifecycleRefused", err)
			}
			assertGenerationOneUnchanged(t, fixture.lifecyclePath, fixture.oldCandidate)
		})
	}
}

func TestRecoveredLifecycleRebindMixedMissingAndUnreachableEvidenceRefuses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, recoveredLifecycleFixture)
	}{
		{
			name: "mixed generation one worktree receipt",
			mutate: func(t *testing.T, fixture recoveredLifecycleFixture) {
				prior, err := LoadCanonicalReceiptSession(fixture.rebinder.RepoRoot, fixture.request.ProviderType,
					fixture.request.ProjectID, fixture.request.TaskRef, "worker-fac614-generation-1")
				if err != nil {
					t.Fatal(err)
				}
				if err := WriteTaskContext(fixture.request.WorktreePath, prior); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreachable rejected candidate",
			mutate: func(t *testing.T, fixture recoveredLifecycleFixture) {
				gitRun(t, fixture.rebinder.RepoRoot, "update-ref", "-d", "refs/herd/candidates/fac-614-generation-1")
			},
		},
		{
			name: "missing review record",
			mutate: func(t *testing.T, fixture recoveredLifecycleFixture) {
				writeFixtureFile(t, fixture.rebinder.ReviewLedgerPath, "")
			},
		},
		{
			name: "partial review queue write",
			mutate: func(t *testing.T, fixture recoveredLifecycleFixture) {
				writeFixtureFile(t, reviewledger.QueuePathFor(fixture.rebinder.ReviewLedgerPath), "")
			},
		},
		{
			name: "revoked coordinator plan grant",
			mutate: func(t *testing.T, fixture recoveredLifecycleFixture) {
				if _, err := fixture.rebinder.EnvironmentPlans.Revoke(context.Background(), fixture.request.EnvironmentPlanID, envplan.CapabilityBoardWrite); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "conflicting generation two completion",
			mutate: func(t *testing.T, fixture recoveredLifecycleFixture) {
				const sender = "shot:fac-614"
				if _, err := mail.NewMailbox(fixture.rebinder.CallbackPath).PostCallback(sender, mail.Callback{
					Ref: fixture.request.TaskRef, Kind: mail.CallbackComplete,
					SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", LeaseGeneration: 2,
					SenderRole: sender, DedupeID: "fac-614-conflicting-generation-2-complete",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unexpected dirty worktree",
			mutate: func(t *testing.T, fixture recoveredLifecycleFixture) {
				writeFixtureFile(t, filepath.Join(fixture.request.WorktreePath, "unapproved-edit.txt"), "not launchable\n")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newRecoveredLifecycleFixture(t)
			tt.mutate(t, fixture)
			if _, err := fixture.rebinder.RebindRecovered(context.Background(), fixture.request); !errors.Is(err, ErrRecoveredLifecycleRefused) {
				t.Fatalf("err=%v, want ErrRecoveredLifecycleRefused", err)
			}
			assertGenerationOneUnchanged(t, fixture.lifecyclePath, fixture.oldCandidate)
		})
	}
}

func TestRecoveredLifecycleRebindConcurrentExactRetries(t *testing.T) {
	fixture := newRecoveredLifecycleFixture(t)
	start := make(chan struct{})
	results := make(chan *lifecycle.GenerationRebindResult, 2)
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			result, err := fixture.rebinder.RebindRecovered(context.Background(), fixture.request)
			results <- result
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	replays := 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if result := <-results; result == nil {
			t.Fatal("concurrent rebind returned no readback")
		} else if result.Replayed {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("concurrent exact retries replayed=%d, want one fresh and one replay", replays)
	}
}

func newRecoveredLifecycleFixture(t *testing.T) recoveredLifecycleFixture {
	t.Helper()
	t.Setenv("HERD_ROLE", "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-b", "main")
	gitRun(t, root, "config", "user.email", "fac669@example.invalid")
	gitRun(t, root, "config", "user.name", "FAC-669 fixture")
	writeFixtureFile(t, filepath.Join(root, "base.txt"), "generation one base\n")
	gitRun(t, root, "add", "base.txt")
	gitRun(t, root, "commit", "-m", "test: generation one base")
	priorBase := gitRun(t, root, "rev-parse", "HEAD")

	writeFixtureFile(t, filepath.Join(root, "rejected.txt"), "rejected candidate\n")
	gitRun(t, root, "add", "rejected.txt")
	gitRun(t, root, "commit", "-m", "test: rejected generation one candidate")
	oldCandidate := gitRun(t, root, "rev-parse", "HEAD")
	gitRun(t, root, "update-ref", "refs/herd/candidates/fac-614-generation-1", oldCandidate)
	gitRun(t, root, "reset", "--hard", priorBase)

	writeFixtureFile(t, filepath.Join(root, "recovered-base.txt"), "generation two base\n")
	gitRun(t, root, "add", "recovered-base.txt")
	gitRun(t, root, "commit", "-m", "test: generation two base")
	newBase := gitRun(t, root, "rev-parse", "HEAD")
	anchor := "refs/herd/anchors/fac-614"
	gitRun(t, root, "update-ref", anchor, newBase)
	worktreePath := filepath.Join(filepath.Dir(root), "fac-614")
	gitRun(t, root, "worktree", "add", "-b", "herd/fac-614", worktreePath, newBase)

	keyDir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	signer, err := LoadOrCreateSigner(keyDir, "herdforge", root)
	if err != nil {
		t.Fatal(err)
	}
	const (
		providerType = "kaneo"
		projectID    = "b939c5jzixruza3vvywrg1hs"
		taskID       = "jvtxv59jmt6mvzq91x7da6jt"
		taskRef      = "FAC-614"
		repository   = "herdforge-fac669-fixture"
		branch       = "herd/fac-614"
	)
	receipt := func(generation int64, leaseID, sessionID, baseSHA string) TaskContext {
		t.Helper()
		signed, issueErr := signer.Issue(TaskContext{
			ProviderType: providerType, ProjectID: projectID, ProviderWorkspace: "workspace",
			ProviderProfile: "KANEO_API_KEY", Repository: repository, Role: RoleWorker,
			TaskRef: taskRef, TaskID: taskID, Branch: branch, BaseSHA: baseSHA, AnchorRef: anchor,
			LeaseID: leaseID, LeaseGeneration: generation, LeaseTaskRef: taskRef, SessionID: sessionID,
			AllowedOps: append([]string(nil), WorkerOps...), ExpiresAt: now.Add(2 * time.Hour),
		})
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		if err := StoreCanonicalReceipt(root, signed); err != nil {
			t.Fatal(err)
		}
		return signed
	}
	priorReceipt := receipt(1, "claim:55", "worker-fac614-generation-1", priorBase)
	_ = priorReceipt
	newReceipt := receipt(2, "claim:68", "worker-fac614-generation-2", newBase)
	if err := WriteTaskContext(worktreePath, newReceipt); err != nil {
		t.Fatal(err)
	}

	packet := fmt.Sprintf("BUILD FAC-614 — EXECUTE.\nWorktree branch %s.\nRead via broker (provider=%s project=%s).\nASSIGNMENT ENVELOPE: task_ref: %s; task_id: %s; lease_generation: 2; END.\nCompletion: herd shot %s --report complete --sha <sha> --lease 2\n",
		branch, providerType, projectID, taskRef, taskID, taskRef)
	writeFixtureFile(t, filepath.Join(worktreePath, "TASK-PACKET.md"), packet)

	binding := envplan.Binding{
		TaskRef: taskRef, TaskID: taskID, Provider: providerType,
		ProviderRevision: "provider-generation-2", GraphRevision: "graph-generation-2",
		RunID: "dispatch:" + taskID, RunRevision: 2, RecoveryFromRevision: 1,
	}
	plans, err := envplan.Open(filepath.Join(root, ".herd", "environment-plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plans.Close() })
	plan, err := plans.Create(ctx, envplan.Plan{
		Binding: binding,
		Requests: []envplan.Request{{
			Capability: envplan.CapabilityBoardWrite,
			Evidence:   envplan.Evidence{Authority: "security", Revision: "fac-669", Subject: taskRef},
		}},
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plans.Grant(ctx, plan.ID, envplan.CapabilityBoardWrite, "coordinator", now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	lifecyclePath := filepath.Join(root, ".herd", "lifecycle.db")
	machine, err := lifecycle.NewMachine(lifecyclePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Transition(lifecycle.TransitionRequest{
		TaskRef: taskRef, Repo: "herdforge", To: lifecycle.StateEligible, Actor: RoleWorker,
		IdempotencyKey:  "shot:fac-614:lease:1:candidate:" + oldCandidate,
		LeaseGeneration: 1, Branch: branch, CandidateSHA: oldCandidate,
	}); err != nil {
		machine.Close()
		t.Fatal(err)
	}
	if err := machine.Close(); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	const reviewer = "reviewer-generation-1"
	if err := ledger.Record(reviewledger.RecordOpts{
		SHA: oldCandidate, Branch: branch, BuilderFamily: "openai", BuilderIdentity: "gpt-5.6-sol",
		ReviewerFamily: "google", Reviewer: reviewer, Provider: "agy", Model: "gemini",
		Gate: "independent", Tier: "R1", Task: taskRef, Lease: "1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Verdict(reviewledger.VerdictOpts{
		SHA: oldCandidate, Reviewer: reviewer, Verdict: reviewledger.VerdictFAIL,
		ReviewerFamily: "google", BuilderFamily: "openai", Branch: branch, Lane: "review",
		Task: taskRef, Lease: "1", CandidateSHA: oldCandidate,
	}); err != nil {
		t.Fatal(err)
	}

	callbackPath := mail.CallbackMailPath(root)
	mailbox := mail.NewMailbox(callbackPath)
	const callbackSender = "shot:fac-614"
	if _, err := mailbox.PostCallback(callbackSender, mail.Callback{
		Ref: taskRef, Kind: mail.CallbackComplete, SHA: oldCandidate,
		LeaseGeneration: 1, SenderRole: callbackSender, DedupeID: "fac-614-generation-1-complete",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.PostCallback(callbackSender, mail.Callback{
		Ref: taskRef, Kind: mail.CallbackBlocked, Detail: "generation conflict",
		LeaseGeneration: 2, SenderRole: callbackSender,
	}); err != nil {
		t.Fatal(err)
	}

	rebinder := NewLifecycleGenerationRebinder(root, lifecyclePath, ledgerPath, callbackPath, plans)
	rebinder.Now = func() time.Time { return now }
	return recoveredLifecycleFixture{
		rebinder: rebinder, lifecyclePath: lifecyclePath, oldCandidate: oldCandidate,
		request: RecoveredLifecycleRequest{
			Binding: binding, EnvironmentPlanID: plan.ID,
			ProviderType: providerType, ProjectID: projectID, TaskID: taskID, TaskRef: taskRef,
			Repository: repository, LifecycleRepository: "herdforge", LeaseID: "claim:68", LeaseGeneration: 2, Branch: branch,
			WorktreePath: worktreePath, WorktreeHead: newBase, BaseSHA: newBase, AnchorRef: anchor,
			TaskPacket: packet, NewReceipt: newReceipt,
		},
	}
}

func assertGenerationOneUnchanged(t *testing.T, path, candidate string) {
	t.Helper()
	machine, err := lifecycle.NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	events, err := machine.EventStore().Events("FAC-614")
	if err != nil {
		t.Fatal(err)
	}
	state, err := machine.EventStore().CurrentState("FAC-614")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || state == nil || state.LeaseGeneration != 1 || state.CandidateSHA != candidate {
		t.Fatalf("refusal mutated lifecycle: events=%+v state=%+v", events, state)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-08-30T12:00:00Z",
		"GIT_COMMITTER_DATE=2026-08-30T12:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return stringTrimSpace(string(out))
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stringTrimSpace(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}
