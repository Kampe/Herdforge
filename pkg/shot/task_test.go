package shot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/eligibility"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

const (
	testRef     = "FAC-89"
	testBranch  = "herd/fac-89"
	testLease   = int64(7)
	candidate   = "1111111111111111111111111111111111111111"
	otherCommit = "2222222222222222222222222222222222222222"
)

// taskRepo makes a real git checkout on testBranch. The isolation stage reads
// the branch back out of git, so a fake directory would not exercise it.
func taskRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", testBranch},
		{"config", "user.email", "shot@test.invalid"},
		{"config", "user.name", "Shot Test"},
		{"config", "commit.gpgSign", "false"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
	} {
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// probe records what each seam saw so a test can prove a stage did or did not
// run, and what the process cwd was when verification happened.
type probe struct {
	eligible, dispatched, awaited, verified, handed int
	verifyCWD                                       string
	verifyReq                                       verifier.VerificationRequest
	handoff                                         Evidence
}

type stubs struct {
	worktree    string
	result      eligibility.Result
	resultErr   error
	facts       DispatchFacts
	dispatchErr error
	callback    mail.Callback
	awaitErr    error
	receipt     *verifier.Receipt
	verifyErr   error
	handoffErr  error
}

func newRun(t *testing.T, s stubs, p *probe) *TaskRun {
	t.Helper()
	return &TaskRun{
		Eligible: func(context.Context, string) (eligibility.Result, error) {
			p.eligible++
			return s.result, s.resultErr
		},
		Dispatch: func(context.Context, string, string) (DispatchFacts, error) {
			p.dispatched++
			return s.facts, s.dispatchErr
		},
		Await: func(context.Context, string, int64) (mail.Callback, error) {
			p.awaited++
			return s.callback, s.awaitErr
		},
		Verify: func(_ context.Context, dir string, req verifier.VerificationRequest) (*verifier.Receipt, error) {
			p.verified++
			p.verifyCWD, _ = os.Getwd()
			p.verifyReq = req
			return s.receipt, s.verifyErr
		},
		Handoff: func(_ context.Context, ev Evidence) error {
			p.handed++
			p.handoff = ev
			return s.handoffErr
		},
	}
}

func passingStubs(t *testing.T) stubs {
	t.Helper()
	wt := taskRepo(t)
	return stubs{
		worktree: wt,
		result:   eligibility.Result{Ref: testRef, State: eligibility.StateEligible},
		facts: DispatchFacts{
			Worktree: wt, Branch: testBranch, BaseSHA: otherCommit,
			LeaseGeneration: testLease, Lane: "smith", Launched: true,
		},
		callback: mail.Callback{
			Ref: testRef, Kind: mail.CallbackComplete,
			SHA: candidate, LeaseGeneration: testLease,
		},
		receipt: &verifier.Receipt{
			CandidateSHA: candidate, Outcome: verifier.OutcomePASS,
			Digest: "sha256:deadbeef",
		},
	}
}

func request(t *testing.T) TaskRequest {
	t.Helper()
	return TaskRequest{TaskRef: testRef, Lane: "worker", Root: t.TempDir(), CallbackTimeout: time.Second}
}

func TestRunReachesReviewHandoff(t *testing.T) {
	s := passingStubs(t)
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err != nil {
		t.Fatalf("run: %v (evidence %+v)", err, ev)
	}
	if !ev.OK || ev.Stage != StageReview {
		t.Fatalf("want OK at %s, got %+v", StageReview, ev)
	}
	if p.eligible != 1 || p.dispatched != 1 || p.awaited != 1 || p.verified != 1 || p.handed != 1 {
		t.Fatalf("each stage must run exactly once: %+v", p)
	}
	if ev.CandidateSHA != candidate || ev.Branch != testBranch || ev.LeaseGeneration != testLease {
		t.Fatalf("evidence lost candidate/branch/lease: %+v", ev)
	}
	if ev.ReceiptDigest != "sha256:deadbeef" || ev.VerificationOutcome != string(verifier.OutcomePASS) {
		t.Fatalf("evidence lost the receipt: %+v", ev)
	}
	if ev.Lane != "smith" {
		t.Fatalf("evidence must record the lane dispatch actually used, got %q", ev.Lane)
	}
	// Handoff sees the same candidate the verifier passed — that is what makes
	// it a handoff of THIS commit rather than of whatever is on the branch now.
	if p.handoff.CandidateSHA != candidate {
		t.Fatalf("handoff got candidate %q", p.handoff.CandidateSHA)
	}
}

func TestRunVerifiesInsideAssignedWorktree(t *testing.T) {
	s := passingStubs(t)
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if p.verifyCWD != ev.Worktree {
		t.Fatalf("process cwd during verification is %q, assigned worktree is %q", p.verifyCWD, ev.Worktree)
	}
	if p.verifyReq.CandidateSHA != candidate || p.verifyReq.BaseSHA != otherCommit {
		t.Fatalf("verification request lost its exact SHAs: %+v", p.verifyReq)
	}
	if p.verifyReq.LeaseGeneration != strconv.FormatInt(testLease, 10) {
		t.Fatalf("verification request lost the lease: %+v", p.verifyReq)
	}
	if p.verifyReq.EnvironmentPolicy != verifier.EnvironmentPolicyHermetic {
		t.Fatalf("verification must be hermetic, got %q", p.verifyReq.EnvironmentPolicy)
	}
}

func TestRunRestoresCallerDirectory(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := passingStubs(t)
	var p probe
	if _, err := newRun(t, s, &p).Run(context.Background(), request(t)); err != nil {
		t.Fatalf("run: %v", err)
	}
	after, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("shot left the process in %q, started in %q", after, before)
	}
}

func TestRunStopsBeforeClaimWhenIneligible(t *testing.T) {
	s := passingStubs(t)
	s.result = eligibility.Result{
		Ref: testRef, State: eligibility.StateBlocked,
		Reasons: []string{"dependency: blocked by open FAC-127"},
	}
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a blocked card must not be claimed")
	}
	if ev.Stage != StageEligibility || ev.OK {
		t.Fatalf("want failure at %s, got %+v", StageEligibility, ev)
	}
	if ev.Recoverable {
		t.Fatal("nothing was claimed; the retry is clean, not recoverable state")
	}
	if p.dispatched != 0 {
		t.Fatal("dispatch ran after an ineligible verdict")
	}
	if !strings.Contains(ev.Error, "FAC-127") {
		t.Fatalf("evidence must carry the eligibility reason, got %q", ev.Error)
	}
}

func TestRunTreatsDispatchFailureAsRecoverable(t *testing.T) {
	s := passingStubs(t)
	s.dispatchErr = errors.New("tab create failed after worktree")
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a failed dispatch must exit non-zero")
	}
	if ev.Stage != StageDispatch {
		t.Fatalf("want failure at %s, got %+v", StageDispatch, ev)
	}
	// Dispatch is where side effects begin: a part-way failure leaves state for
	// the compensator, so calling it a clean retry would lose that work.
	if !ev.Recoverable {
		t.Fatal("a failed dispatch may have left a claim or worktree behind")
	}
	if p.awaited != 0 {
		t.Fatal("waited for a callback from an agent that never launched")
	}
}

func TestRunRefusesEligibilityForAnotherTask(t *testing.T) {
	s := passingStubs(t)
	s.result = eligibility.Result{Ref: "FAC-90", State: eligibility.StateEligible}
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("an eligibility answer about another card must not admit this one")
	}
	if p.dispatched != 0 {
		t.Fatal("dispatch ran on another card's eligibility")
	}
	if !strings.Contains(ev.Error, "FAC-90") {
		t.Fatalf("error should name the mismatched ref, got %q", ev.Error)
	}
}

func TestRunRefusesBranchDriftFromDispatch(t *testing.T) {
	s := passingStubs(t)
	s.facts.Branch = "herd/fac-999"
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a worktree on a different branch than dispatch reported must fail")
	}
	if ev.Stage != StageIsolation {
		t.Fatalf("want failure at %s, got %+v", StageIsolation, ev)
	}
	if !ev.Recoverable {
		t.Fatal("the claim and worktree already exist; this is recoverable state")
	}
	if ev.Branch != testBranch {
		t.Fatalf("evidence must record the ACTUAL branch %q, got %q", testBranch, ev.Branch)
	}
	if p.awaited != 0 {
		t.Fatal("waited for a callback despite branch drift")
	}
}

func TestRunRefusesMissingLeaseGeneration(t *testing.T) {
	s := passingStubs(t)
	s.facts.LeaseGeneration = 0
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a dispatch without a lease generation cannot be fenced")
	}
	if ev.Stage != StageIsolation || p.awaited != 0 {
		t.Fatalf("want failure at %s before the callback wait, got %+v %+v", StageIsolation, ev, p)
	}
}

func TestRunRejectsCallbacksAboutOtherTasks(t *testing.T) {
	s := passingStubs(t)
	s.callback.Ref = "FAC-90"
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("another card's callback must not complete this shot")
	}
	if ev.Stage != StageCallback || p.verified != 0 {
		t.Fatalf("want failure at %s with no verification, got %+v %+v", StageCallback, ev, p)
	}
}

func TestRunRejectsStaleLeaseCallback(t *testing.T) {
	s := passingStubs(t)
	s.callback.LeaseGeneration = testLease - 1
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a callback from a previous lease must not complete this shot")
	}
	if !strings.Contains(ev.Error, "stale agent") {
		t.Fatalf("error should name the stale lease, got %q", ev.Error)
	}
}

func TestRunPropagatesBlockedCallback(t *testing.T) {
	s := passingStubs(t)
	s.callback = mail.Callback{
		Ref: testRef, Kind: mail.CallbackBlocked,
		Detail: "needs FAC-172", LeaseGeneration: testLease,
	}
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a BLOCKED report must exit non-zero, not silently pass")
	}
	if ev.Stage != StageCallback || !strings.Contains(ev.Error, "needs FAC-172") {
		t.Fatalf("evidence must carry the blocked detail: %+v", ev)
	}
	if p.verified != 0 {
		t.Fatal("verified a candidate the agent never produced")
	}
}

func TestRunRejectsCallbackWithoutExactSHA(t *testing.T) {
	s := passingStubs(t)
	s.callback.SHA = "1111111"
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("an abbreviated SHA cannot anchor an exact-SHA verification")
	}
	if p.verified != 0 {
		t.Fatalf("verification ran on %q", s.callback.SHA)
	}
	if ev.Stage != StageCallback {
		t.Fatalf("want failure at %s, got %+v", StageCallback, ev)
	}
}

func TestRunRefusesReceiptForAnotherCommit(t *testing.T) {
	s := passingStubs(t)
	s.receipt = &verifier.Receipt{CandidateSHA: otherCommit, Outcome: verifier.OutcomePASS}
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a receipt for a different commit proves nothing about this candidate")
	}
	if ev.Stage != StageVerify || p.handed != 0 {
		t.Fatalf("want failure at %s with no handoff, got %+v %+v", StageVerify, ev, p)
	}
}

func TestRunDoesNotHandOffFailedVerification(t *testing.T) {
	for _, outcome := range []verifier.Outcome{verifier.OutcomeFAIL, verifier.OutcomeBLOCKED} {
		s := passingStubs(t)
		s.receipt = &verifier.Receipt{CandidateSHA: candidate, Outcome: outcome, Digest: "sha256:x"}
		var p probe
		ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
		if err == nil {
			t.Fatalf("%s verification must exit non-zero", outcome)
		}
		if p.handed != 0 {
			t.Fatalf("%s candidate was handed to review", outcome)
		}
		if ev.VerificationOutcome != string(outcome) || !ev.Recoverable {
			t.Fatalf("evidence must record the %s outcome as recoverable: %+v", outcome, ev)
		}
	}
}

func TestRunPropagatesHandoffFailure(t *testing.T) {
	s := passingStubs(t)
	s.handoffErr = errors.New("review ledger is frozen")
	var p probe
	ev, err := newRun(t, s, &p).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("a failed review handoff must not report success")
	}
	if ev.OK || ev.Stage != StageReview || !ev.Recoverable {
		t.Fatalf("want recoverable failure at %s, got %+v", StageReview, ev)
	}
}

func TestRunRefusesSecondInvocationForSameTask(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockTask(root, testRef)
	if err != nil {
		t.Fatalf("first holder: %v", err)
	}
	defer unlock()

	s := passingStubs(t)
	var p probe
	req := TaskRequest{TaskRef: testRef, Lane: "worker", Root: root, CallbackTimeout: time.Second}
	ev, err := newRun(t, s, &p).Run(context.Background(), req)
	if err == nil {
		t.Fatal("a duplicate shot must refuse rather than race the claim")
	}
	if ev.Stage != StageLock || ev.Recoverable {
		t.Fatalf("want a clean refusal at %s, got %+v", StageLock, ev)
	}
	if p.eligible != 0 || p.dispatched != 0 {
		t.Fatalf("duplicate shot reached the board: %+v", p)
	}

	// Releasing the first holder must let the next shot through, or the guard
	// would wedge the card instead of serialising it.
	unlock()
	if _, err := newRun(t, s, &p).Run(context.Background(), req); err != nil {
		t.Fatalf("shot refused after the lock was released: %v", err)
	}
}

func TestLockTaskBreaksDeadHolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".herd", "locks", "shot-fac-89.lock.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// PID 0 can never be a live holder, so this stands in for a crashed shot.
	if err := os.WriteFile(filepath.Join(dir, "holder"), []byte("4194304\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockTask(root, testRef)
	if err != nil {
		t.Fatalf("a crashed holder must not wedge the card: %v", err)
	}
	unlock()
}

func TestLockTaskKeepsLiveHolder(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockTask(root, testRef)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// This process is the holder and is very much alive.
	if _, err := lockTask(root, testRef); err == nil {
		t.Fatal("a live holder's lock was broken")
	}
}

func TestLockTaskIsPerTask(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockTask(root, testRef)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other, err := lockTask(root, "FAC-90")
	if err != nil {
		t.Fatalf("holding FAC-89 must not block FAC-90: %v", err)
	}
	other()
}

func TestRunRefusesUnwiredSeams(t *testing.T) {
	ev, err := (&TaskRun{}).Run(context.Background(), request(t))
	if err == nil {
		t.Fatal("an unwired run must fail closed, not skip its gates")
	}
	for _, seam := range []string{"callback", "dispatch", "eligibility", "review", "verify"} {
		if !strings.Contains(ev.Error, seam) {
			t.Fatalf("error must name the missing %s seam: %q", seam, ev.Error)
		}
	}
}

func TestRunRefusesBadRequests(t *testing.T) {
	s := passingStubs(t)
	for name, req := range map[string]TaskRequest{
		"not a ref": {TaskRef: "do the thing", Lane: "worker", Root: t.TempDir()},
		"empty ref": {TaskRef: "", Lane: "worker", Root: t.TempDir()},
		"no lane":   {TaskRef: testRef, Root: t.TempDir()},
		"no root":   {TaskRef: testRef, Lane: "worker"},
	} {
		var p probe
		ev, err := newRun(t, s, &p).Run(context.Background(), req)
		if err == nil {
			t.Fatalf("%s: must be refused", name)
		}
		if p.eligible != 0 {
			t.Fatalf("%s: reached the board", name)
		}
		if ev.Stage != StageLock {
			t.Fatalf("%s: want %s, got %s", name, StageLock, ev.Stage)
		}
	}
}

func TestIsTaskRef(t *testing.T) {
	for _, ok := range []string{"FAC-89", "fac-89", "ENG-1234", "A-1"} {
		if !IsTaskRef(ok) {
			t.Errorf("%q should be a task ref", ok)
		}
	}
	for _, bad := range []string{"", "FAC", "89", "FAC-", "-89", "FAC-89-1", "review the diff", "FAC 89"} {
		if IsTaskRef(bad) {
			t.Errorf("%q should not be a task ref", bad)
		}
	}
}

func TestEnterWorktreeRefusesDetachedHead(t *testing.T) {
	dir := taskRepo(t)
	if out, err := testgit.Command(dir, "checkout", "-q", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	before, _ := os.Getwd()
	restore, _, _, err := enterWorktree(dir)
	if err == nil {
		restore()
		t.Fatal("a detached HEAD has no branch to record")
	}
	if after, _ := os.Getwd(); after != before {
		t.Fatalf("failed entry left the process in %q", after)
	}
}
