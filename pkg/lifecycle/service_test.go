package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/ledger"
)

type fakeWorktrees struct {
	mu    sync.Mutex
	facts map[string]WorktreeFacts
}

func (f *fakeWorktrees) Inspect(_ context.Context, path string) (WorktreeFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	facts, ok := f.facts[path]
	if !ok {
		return WorktreeFacts{}, errors.New("unknown fake worktree")
	}
	return facts, nil
}

func (f *fakeWorktrees) setHead(path, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	facts := f.facts[path]
	facts.HeadSHA = sha
	f.facts[path] = facts
}

type serviceFixture struct {
	t        *testing.T
	path     string
	machine  *Machine
	service  *Service
	git      *fakeWorktrees
	human    Identity
	agent    Identity
	reviewer Identity
	base     string
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	m, err := NewMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0).UTC()
	identity := func(actorID, actorKind, principalID, principalKind string) Identity {
		return Identity{
			Actor:     ledger.Actor{Version: ledger.Version1, ID: actorID, Kind: actorKind, DisplayName: actorID, CreatedAt: fixed},
			Principal: ledger.Principal{Version: ledger.Version1, ID: principalID, ActorID: actorID, Kind: principalKind, Label: principalID, CreatedAt: fixed},
		}
	}
	git := &fakeWorktrees{facts: map[string]WorktreeFacts{}}
	s, err := NewService(m, WithWorktreeInspector(git), WithServiceClock(func() time.Time { return fixed }))
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	fx := &serviceFixture{
		t: t, path: path, machine: m, service: s, git: git, base: testSHA("1"),
		human:    identity("actor-human", "operator", "principal-human", "local_operator"),
		agent:    identity("actor-agent", "agent", "principal-agent", "local_service"),
		reviewer: identity("actor-reviewer", "agent", "principal-reviewer", "local_service"),
	}
	t.Cleanup(func() { _ = fx.machine.Close() })
	return fx
}

func testSHA(ch string) string    { return strings.Repeat(ch, 40) }
func testDigest(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }

func (f *serviceFixture) checkout(task string) string { return "/repo/.worktrees/" + task }

func (f *serviceFixture) addWorktree(task string) {
	path := f.checkout(task)
	f.git.facts[path] = WorktreeFacts{
		CheckoutPath: path, SharedRoot: "/repo", PortablePath: "./.worktrees/" + task,
		Branch: "herd/" + strings.ToLower(task), HeadSHA: f.base,
	}
}

func (f *serviceFixture) command(task, key string, identity Identity) CommandContext {
	return CommandContext{IdempotencyKey: key, TaskRef: task, Repo: "Herdforge", CheckoutPath: f.checkout(task), LeaseGeneration: 1, Identity: identity}
}

func (f *serviceFixture) own(task string) {
	f.t.Helper()
	f.addWorktree(task)
	_, err := f.service.OwnWorktree(context.Background(), OwnWorktreeRequest{
		Command: f.command(task, task+":own", f.agent), WorktreeID: task + "-worktree",
		Branch: "herd/" + strings.ToLower(task), BaseSHA: f.base,
	})
	if err != nil {
		f.t.Fatalf("own %s: %v", task, err)
	}
}

func (f *serviceFixture) approvePlan(task string) {
	f.t.Helper()
	_, err := f.service.ApprovePlan(context.Background(), ApprovePlanRequest{
		Command: f.command(task, task+":plan", f.human), PlanDigest: testDigest("2"),
	})
	if err != nil {
		f.t.Fatalf("approve plan %s: %v", task, err)
	}
}

func (f *serviceFixture) start(task string) {
	f.t.Helper()
	_, err := f.service.StartWork(context.Background(), StartWorkRequest{Command: f.command(task, task+":start", f.agent)})
	if err != nil {
		f.t.Fatalf("start %s: %v", task, err)
	}
}

type queuedCandidate struct {
	id, sha, reviewReceiptID string
}

func (f *serviceFixture) submitThroughPromotion(task, suffix string) queuedCandidate {
	f.t.Helper()
	sha := testSHA(suffix)
	candidateID := task + "-candidate-" + suffix
	f.git.setHead(f.checkout(task), sha)
	candidate := ledger.Candidate{
		Version: ledger.Version1, ID: candidateID, RunID: task + "-run", PhaseID: task + "-phase",
		GitSHA: sha, BaseSHA: f.base, EvidenceDigest: testDigest("3"),
	}
	if _, err := f.service.SubmitCandidate(context.Background(), SubmitCandidateRequest{
		Command: f.command(task, task+":submit:"+suffix, f.agent), Candidate: candidate,
	}); err != nil {
		f.t.Fatalf("submit %s: %v", task, err)
	}
	verification := ledger.Receipt{
		Version: ledger.Version1, ID: task + "-verify-" + suffix, CandidateID: candidateID,
		Kind: "verification", EvidenceDigest: testDigest("4"), Payload: json.RawMessage(`{"checks":["go test"]}`),
	}
	if _, err := f.service.RecordVerification(context.Background(), VerificationRequest{
		Command: f.command(task, task+":verify:"+suffix, f.agent), CandidateID: candidateID,
		CandidateSHA: sha, Outcome: "pass", Receipt: verification,
	}); err != nil {
		f.t.Fatalf("verify %s: %v", task, err)
	}
	reviewReceipt := ledger.Receipt{
		Version: ledger.Version1, ID: task + "-review-receipt-" + suffix, CandidateID: candidateID,
		Kind: "review", EvidenceDigest: testDigest("5"), Payload: json.RawMessage(`{"review":"independent"}`),
	}
	review := ledger.Review{
		Version: ledger.Version1, ID: task + "-review-" + suffix, CandidateID: candidateID,
		ReviewerID: f.reviewer.Principal.ID, Outcome: "pass", ReceiptID: reviewReceipt.ID,
	}
	if _, err := f.service.RecordReview(context.Background(), ReviewRequest{
		Command: f.command(task, task+":review:"+suffix, f.reviewer), CandidateID: candidateID,
		CandidateSHA: sha, Receipt: reviewReceipt, Review: review,
	}); err != nil {
		f.t.Fatalf("review %s: %v", task, err)
	}
	// Promotion is intentionally autonomous: a local service principal, not a
	// human, can advance a fully verified/reviewed exact candidate.
	if _, err := f.service.PromotePullRequest(context.Background(), PromotePRRequest{
		Command: f.command(task, task+":promote:"+suffix, f.agent), CandidateID: candidateID,
		CandidateSHA: sha, PullRequest: "pr://" + task + "/" + suffix,
	}); err != nil {
		f.t.Fatalf("promote %s: %v", task, err)
	}
	return queuedCandidate{id: candidateID, sha: sha, reviewReceiptID: reviewReceipt.ID}
}

func (f *serviceFixture) prepareQueued(task, suffix string) queuedCandidate {
	f.t.Helper()
	f.own(task)
	f.approvePlan(task)
	f.start(task)
	return f.submitThroughPromotion(task, suffix)
}

func (f *serviceFixture) approveMerge(task string, c queuedCandidate, decision, approvalID string, identity Identity) ledger.Approval {
	f.t.Helper()
	a := ledger.Approval{
		Version: ledger.Version1, ID: approvalID, CandidateID: c.id,
		ApproverID: identity.Principal.ID, Decision: decision, ReceiptID: c.reviewReceiptID,
	}
	_, err := f.service.RecordMergeApproval(context.Background(), MergeApprovalRequest{
		Command: f.command(task, task+":approval:"+approvalID, identity), CandidateID: c.id, CandidateSHA: c.sha, Approval: a,
	})
	if err != nil {
		f.t.Fatalf("approve merge %s: %v", task, err)
	}
	return a
}

func TestServiceStateChangingReplaySurvivesRestartWithoutDuplicate(t *testing.T) {
	fx := newServiceFixture(t)
	const task = "SPE-559"
	fx.own(task)
	req := ApprovePlanRequest{Command: fx.command(task, task+":plan", fx.human), PlanDigest: testDigest("2")}
	first, err := fx.service.ApprovePlan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 || first.Replayed {
		t.Fatalf("first command must append exactly one event: %+v", first)
	}

	if err := fx.machine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMachine(fx.path)
	if err != nil {
		t.Fatal(err)
	}
	fx.machine = reopened
	restarted, err := NewService(reopened, WithWorktreeInspector(fx.git))
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.ApprovePlan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || len(second.Events) != 1 || second.Events[0].ID != first.Events[0].ID {
		t.Fatalf("restart replay did not return the original event: first=%+v second=%+v", first, second)
	}
	events, err := reopened.EventStore().Events(task)
	if err != nil || len(events) != 1 {
		t.Fatalf("replay duplicated durable state: events=%d err=%v", len(events), err)
	}
}

func TestServiceIllegalTransitionFailsClosedAndWritesNothing(t *testing.T) {
	fx := newServiceFixture(t)
	const task = "SPE-ILLEGAL"
	fx.own(task)
	_, err := fx.service.StartWork(context.Background(), StartWorkRequest{Command: fx.command(task, task+":start-before-plan", fx.agent)})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected canonical transition rejection, got %v", err)
	}
	state, _ := fx.machine.EventStore().CurrentState(task)
	if state != nil {
		t.Fatalf("illegal command changed state: %+v", state)
	}
	var commands int
	if err := fx.machine.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_service_commands WHERE idempotency_key = ?`, task+":start-before-plan").Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if commands != 0 {
		t.Fatalf("illegal command was recorded as successful: %d", commands)
	}
}

func TestServiceHumanGatesPlanAndMergeToMain(t *testing.T) {
	fx := newServiceFixture(t)
	const task = "SPE-HUMAN"
	fx.own(task)
	_, err := fx.service.ApprovePlan(context.Background(), ApprovePlanRequest{
		Command: fx.command(task, task+":agent-plan", fx.agent), PlanDigest: testDigest("2"),
	})
	if !errors.Is(err, ErrHumanPrincipalRequired) {
		t.Fatalf("agent approved a long-lived plan: %v", err)
	}
	fx.approvePlan(task)
	fx.start(task)
	candidate := fx.submitThroughPromotion(task, "a")
	approval := ledger.Approval{Version: ledger.Version1, ID: "agent-approval", CandidateID: candidate.id,
		ApproverID: fx.agent.Principal.ID, Decision: "approved", ReceiptID: candidate.reviewReceiptID}
	_, err = fx.service.RecordMergeApproval(context.Background(), MergeApprovalRequest{
		Command: fx.command(task, task+":agent-merge", fx.agent), CandidateID: candidate.id, CandidateSHA: candidate.sha, Approval: approval,
	})
	if !errors.Is(err, ErrHumanPrincipalRequired) {
		t.Fatalf("agent approved merge-to-main: %v", err)
	}
}

func TestServiceMergeAdmissionRejectsMissingRejectedStaleAndSHAMismatch(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fx := newServiceFixture(t)
		c := fx.prepareQueued("SPE-MISSING", "a")
		_, err := fx.service.BeginIntegration(context.Background(), BeginIntegrationRequest{
			Command: fx.command("SPE-MISSING", "missing:integrate", fx.agent), CandidateID: c.id,
			CandidateSHA: c.sha, ApprovalID: "does-not-exist", TargetBranch: "main",
		})
		if !errors.Is(err, ErrApprovalMissing) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		fx := newServiceFixture(t)
		c := fx.prepareQueued("SPE-REJECTED", "a")
		approved := fx.approveMerge("SPE-REJECTED", c, "approved", "approval-before-rejection", fx.human)
		rejected := fx.approveMerge("SPE-REJECTED", c, "rejected", "approval-rejected", fx.human)
		_, err := fx.service.BeginIntegration(context.Background(), BeginIntegrationRequest{
			Command: fx.command("SPE-REJECTED", "rejected:old-approval", fx.agent), CandidateID: c.id,
			CandidateSHA: c.sha, ApprovalID: approved.ID, TargetBranch: "main",
		})
		if !errors.Is(err, ErrApprovalStale) {
			t.Fatalf("superseded approval remained admissible: %v", err)
		}
		_, err = fx.service.BeginIntegration(context.Background(), BeginIntegrationRequest{
			Command: fx.command("SPE-REJECTED", "rejected:integrate", fx.agent), CandidateID: c.id,
			CandidateSHA: c.sha, ApprovalID: rejected.ID, TargetBranch: "main",
		})
		if !errors.Is(err, ErrApprovalRejected) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("sha mismatch", func(t *testing.T) {
		fx := newServiceFixture(t)
		const task = "SPE-SHA"
		c := fx.prepareQueued(task, "a")
		a := fx.approveMerge(task, c, "approved", "approval-sha", fx.human)
		wrong := testSHA("b")
		fx.git.setHead(fx.checkout(task), wrong)
		_, err := fx.service.BeginIntegration(context.Background(), BeginIntegrationRequest{
			Command: fx.command(task, "sha:integrate", fx.agent), CandidateID: c.id,
			CandidateSHA: wrong, ApprovalID: a.ID, TargetBranch: "main",
		})
		if !errors.Is(err, ErrApprovalSHAMismatch) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("stale candidate", func(t *testing.T) {
		fx := newServiceFixture(t)
		const task = "SPE-STALE"
		old := fx.prepareQueued(task, "a")
		approval := fx.approveMerge(task, old, "approved", "approval-old", fx.human)
		if _, err := fx.service.Recover(context.Background(), RecoverRequest{
			Command: fx.command(task, "stale:recover", fx.agent), EvidenceDigest: testDigest("6"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.service.ResumeRecovery(context.Background(), ResumeRecoveryRequest{
			Command: fx.command(task, "stale:resume", fx.agent), Target: StateBuilding,
			CandidateSHA: old.sha, EvidenceDigest: testDigest("7"),
		}); err != nil {
			t.Fatal(err)
		}
		current := fx.submitThroughPromotion(task, "b")
		_, err := fx.service.BeginIntegration(context.Background(), BeginIntegrationRequest{
			Command: fx.command(task, "stale:integrate", fx.agent), CandidateID: current.id,
			CandidateSHA: current.sha, ApprovalID: approval.ID, TargetBranch: "main",
		})
		if !errors.Is(err, ErrApprovalStale) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestServiceRejectsSharedRootAndUnownedWorktree(t *testing.T) {
	t.Run("shared root", func(t *testing.T) {
		parent := t.TempDir()
		repo := filepath.Join(parent, "repo")
		if err := os.Mkdir(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit := func(args ...string) string {
			cmd := testgit.Command(repo, args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		runGit("init", "-q")
		runGit("config", "user.email", "test@example.invalid")
		runGit("config", "user.name", "test")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("root\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "README.md")
		runGit("commit", "-q", "-m", "init")
		head := runGit("rev-parse", "HEAD")
		m, err := NewMachine(filepath.Join(parent, "service.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		s, err := NewService(m)
		if err != nil {
			t.Fatal(err)
		}
		fx := newServiceFixture(t)
		_, err = s.OwnWorktree(context.Background(), OwnWorktreeRequest{
			Command:    CommandContext{IdempotencyKey: "root:own", TaskRef: "SPE-ROOT", Repo: "Herdforge", CheckoutPath: repo, LeaseGeneration: 1, Identity: fx.agent},
			WorktreeID: "root", Branch: runGit("branch", "--show-current"), BaseSHA: head,
		})
		if !errors.Is(err, ErrSharedCheckout) {
			t.Fatalf("shared checkout mutation was not rejected: %v", err)
		}
		state, _ := m.EventStore().CurrentState("SPE-ROOT")
		if state != nil {
			t.Fatalf("root rejection changed state: %+v", state)
		}

		// Non-vacuity guard: the same real inspector accepts an actual linked
		// task worktree, proving it does not merely reject every checkout.
		linked := filepath.Join(repo, ".worktrees", "SPE-LINKED")
		runGit("worktree", "add", "-q", "-b", "herd/spe-linked", linked)
		if _, err := s.OwnWorktree(context.Background(), OwnWorktreeRequest{
			Command:    CommandContext{IdempotencyKey: "linked:own", TaskRef: "SPE-LINKED", Repo: "Herdforge", CheckoutPath: linked, LeaseGeneration: 1, Identity: fx.agent},
			WorktreeID: "linked", Branch: "herd/spe-linked", BaseSHA: head,
		}); err != nil {
			t.Fatalf("real linked worktree was rejected: %v", err)
		}
	})

	t.Run("different linked worktree", func(t *testing.T) {
		fx := newServiceFixture(t)
		fx.own("SPE-OWNED")
		fx.addWorktree("SPE-OTHER")
		cmd := fx.command("SPE-OWNED", "wrong-worktree:plan", fx.human)
		cmd.CheckoutPath = fx.checkout("SPE-OTHER")
		_, err := fx.service.ApprovePlan(context.Background(), ApprovePlanRequest{Command: cmd, PlanDigest: testDigest("2")})
		if !errors.Is(err, ErrWorktreeNotOwned) {
			t.Fatalf("unowned worktree accepted: %v", err)
		}
	})
}

func TestServiceIntegrationLockSerializesMain(t *testing.T) {
	fx := newServiceFixture(t)
	first := fx.prepareQueued("SPE-LOCK-1", "a")
	firstApproval := fx.approveMerge("SPE-LOCK-1", first, "approved", "approval-lock-1", fx.human)
	second := fx.prepareQueued("SPE-LOCK-2", "b")
	secondApproval := fx.approveMerge("SPE-LOCK-2", second, "approved", "approval-lock-2", fx.human)

	// A second Machine/Service handle simulates another coordinator process.
	otherMachine, err := NewMachine(fx.path)
	if err != nil {
		t.Fatal(err)
	}
	defer otherMachine.Close()
	otherService, err := NewService(otherMachine, WithWorktreeInspector(fx.git))
	if err != nil {
		t.Fatal(err)
	}

	beginFirst := BeginIntegrationRequest{
		Command: fx.command("SPE-LOCK-1", "lock:first", fx.agent), CandidateID: first.id,
		CandidateSHA: first.sha, ApprovalID: firstApproval.ID, TargetBranch: "main",
	}
	if _, err := fx.service.BeginIntegration(context.Background(), beginFirst); err != nil {
		t.Fatal(err)
	}
	// Replay by the lock owner is a no-op, not lock contention or a duplicate merge intent.
	replayed, err := fx.service.BeginIntegration(context.Background(), beginFirst)
	if err != nil || !replayed.Replayed {
		t.Fatalf("owner replay failed: result=%+v err=%v", replayed, err)
	}

	beginSecond := BeginIntegrationRequest{
		Command: fx.command("SPE-LOCK-2", "lock:second", fx.agent), CandidateID: second.id,
		CandidateSHA: second.sha, ApprovalID: secondApproval.ID, TargetBranch: "main",
	}
	if _, err := otherService.BeginIntegration(context.Background(), beginSecond); !errors.Is(err, ErrIntegrationLocked) {
		t.Fatalf("second integration was not excluded: %v", err)
	}
	state, _ := fx.machine.EventStore().CurrentState("SPE-LOCK-2")
	if state == nil || state.State != StateIntegrationQueued {
		t.Fatalf("contending task changed state: %+v", state)
	}
	foreignComplete := fx.command("SPE-LOCK-1", "lock:foreign:complete", fx.reviewer)
	if _, err := fx.service.CompleteIntegration(context.Background(), CompleteIntegrationRequest{
		Command: foreignComplete, CandidateID: first.id, CandidateSHA: first.sha,
		TargetBranch: "main", EvidenceDigest: testDigest("8"),
	}); !errors.Is(err, ErrIntegrationLocked) {
		t.Fatalf("foreign principal completed another owner's integration: %v", err)
	}

	// Recovery does not drop integration ownership. The same exact candidate
	// can resume Integrated and complete, while another task remains excluded.
	if _, err := fx.service.Recover(context.Background(), RecoverRequest{
		Command: fx.command("SPE-LOCK-1", "lock:first:recover", fx.agent), EvidenceDigest: testDigest("9"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.service.ResumeRecovery(context.Background(), ResumeRecoveryRequest{
		Command: fx.command("SPE-LOCK-1", "lock:first:resume", fx.agent), Target: StateIntegrated,
		CandidateSHA: first.sha, EvidenceDigest: testDigest("a"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := fx.service.CompleteIntegration(context.Background(), CompleteIntegrationRequest{
		Command: fx.command("SPE-LOCK-1", "lock:first:complete", fx.agent), CandidateID: first.id,
		CandidateSHA: first.sha, TargetBranch: "main", EvidenceDigest: testDigest("8"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := otherService.BeginIntegration(context.Background(), beginSecond); err != nil {
		t.Fatalf("second integration did not acquire released lock: %v", err)
	}
	pending, err := fx.machine.Outbox().Pending("git.merge", 10, time.Now().Add(time.Hour))
	if err != nil || len(pending) != 2 {
		t.Fatalf("expected one merge intent per admitted candidate, got %d err=%v", len(pending), err)
	}
}
