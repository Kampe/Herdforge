package sync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// The real durable state store must satisfy the authority interface — if this
// stops compiling, the CLI can no longer supply lifecycle state at all.
var _ LifecycleAuthority = (*lifecycle.EventStore)(nil)

// receiptRepo builds a repo with a real local origin so both `git fetch` and
// RepositoryIdentity work offline. origin/main carries:
//   - baseSHA:  the base commit
//   - mergeSHA: a real content commit whose subject names FAC-132
//   - emptySHA: an EMPTY commit whose subject names FAC-777
//   - otherSHA: an unrelated real commit, ancestor of origin/main
func receiptRepo(t *testing.T) (dir, baseSHA, mergeSHA, emptySHA, otherSHA string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	dir = filepath.Join(root, "work")

	run := func(d string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, d, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	run(root, "init", "-q", "--bare", "-b", "main", origin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(root, "init", "-q", "-b", "main", dir)
	run(dir, "config", "user.email", "test@herdforge.local")
	run(dir, "config", "user.name", "herdforge-test")
	run(dir, "config", "commit.gpgsign", "false")
	run(dir, "config", "tag.gpgsign", "false")
	run(dir, "remote", "add", "origin", origin)

	write("base.txt", "base\n")
	run(dir, "add", "-A")
	run(dir, "commit", "-q", "-m", "chore: base")
	baseSHA = run(dir, "rev-parse", "HEAD")

	write("other.txt", "other\n")
	run(dir, "add", "-A")
	run(dir, "commit", "-q", "-m", "chore: unrelated work")
	otherSHA = run(dir, "rev-parse", "HEAD")

	run(dir, "commit", "--allow-empty", "-q", "-m", "chore: nothing at all (FAC-777)")
	emptySHA = run(dir, "rev-parse", "HEAD")

	write("feature.txt", "the actual work\n")
	run(dir, "add", "-A")
	run(dir, "commit", "-q", "-m", "feat(donereceipt): task-bound completion receipt (FAC-132)")
	mergeSHA = run(dir, "rev-parse", "HEAD")

	run(dir, "push", "-q", "origin", "main")
	return dir, baseSHA, mergeSHA, emptySHA, otherSHA
}

const (
	testTaskID   = "task-id-fac-132"
	testLeaseGen = int64(7)
	testCandSHA  = "abcdef0123456789abcdef0123456789abcdef01"
)

func validReceipt(t *testing.T, dir, ref, mergeSHA, baseSHA string) *CompletionReceipt {
	t.Helper()
	pid, err := PatchID(dir, mergeSHA)
	if err != nil {
		t.Fatalf("PatchID(%s): %v", mergeSHA, err)
	}
	repoID, err := toolchild.RepositoryIdentity(dir)
	if err != nil {
		t.Fatalf("RepositoryIdentity: %v", err)
	}
	r := &CompletionReceipt{
		RepoID: repoID, TaskRef: ref, TaskID: testTaskID,
		ProviderRevision: "provider-rev-1", LeaseGeneration: testLeaseGen,
		BaseSHA: baseSHA, CandidateSHA: testCandSHA, MergeSHA: mergeSHA,
		PatchID: pid, AcceptanceDigest: "acceptance-digest-1", VerificationDigest: "verification-digest-1",
		RiskTier: "R3", AuthorFamily: "anthropic", ReviewerFamily: "openai",
		Verdict: "PASS", IntegrationResult: IntegrationMerged,
	}
	r.Seal()
	return r
}

// fakeLifecycle is the durable-state authority under test control.
type fakeLifecycle struct {
	st  *lifecycle.TaskState
	err error
}

func (f fakeLifecycle) CurrentState(string) (*lifecycle.TaskState, error) { return f.st, f.err }

func integratedState(ref string) *lifecycle.TaskState {
	return &lifecycle.TaskState{
		TaskRef: ref, State: lifecycle.StateIntegrated,
		LeaseGeneration: testLeaseGen, CandidateSHA: testCandSHA,
	}
}

// countingProvider counts mutations and can lie about the readback, which is
// exactly the failure mode BoardDone exists to catch.
type countingProvider struct {
	*provider.MemoryProvider
	updates     int
	comments    int
	lieOnReadTo string
}

func (c *countingProvider) UpdateStatus(ctx context.Context, taskID, status string) error {
	c.updates++
	return c.MemoryProvider.UpdateStatus(ctx, taskID, status)
}

func (c *countingProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	task, err := c.MemoryProvider.GetTask(ctx, id)
	if err != nil || c.lieOnReadTo == "" {
		return task, err
	}
	lied := *task
	lied.Status = c.lieOnReadTo
	return &lied, nil
}

func (c *countingProvider) AddComment(ctx context.Context, taskID, body string) error {
	c.comments++
	return c.MemoryProvider.AddComment(ctx, taskID, body)
}

func newReceiptBoard(t *testing.T, ref, taskID string) *countingProvider {
	t.Helper()
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: taskID, Ref: ref, Title: "receipt gate", Status: "in-review", ProjectID: "p1"})
	return &countingProvider{MemoryProvider: mp}
}

func statusOf(t *testing.T, cp *countingProvider, taskID string) string {
	t.Helper()
	got, err := cp.MemoryProvider.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return got.Status
}

// --- acceptance criterion: an empty commit naming a ticket cannot close it ---

func TestEmptyCommitNamingTicketCannotClose(t *testing.T) {
	dir, baseSHA, realMergeSHA, emptySHA, _ := receiptRepo(t)
	ctx := context.Background()

	t.Run("commit subject alone is not authority", func(t *testing.T) {
		cp := newReceiptBoard(t, "FAC-777", "task-777")
		// The commit-subject hint IS there — MergeEvidence still finds it.
		hint, err := MergeEvidence(dir, "FAC-777", "")
		if err != nil || !strings.Contains(hint, "FAC-777") {
			t.Fatalf("fixture must produce a commit-subject hint, got %q err %v", hint, err)
		}
		_, err = BoardDone(ctx, cp, DoneRequest{RepoDir: dir, ProjectID: "p1", Ref: "FAC-777"})
		if !errors.Is(err, ErrNoEvidence) {
			t.Fatalf("a commit naming the ticket must not close it, got %v", err)
		}
		if cp.updates != 0 {
			t.Fatalf("refused card must not be written, updates=%d", cp.updates)
		}
		if got := statusOf(t, cp, "task-777"); got != "in-review" {
			t.Fatalf("refused card moved to %q", got)
		}
	})

	t.Run("a receipt claiming the empty commit is refused", func(t *testing.T) {
		if _, err := PatchID(dir, emptySHA); err == nil {
			t.Fatal("fixture: the empty commit must have no patch id at all")
		}
		r := validReceipt(t, dir, "FAC-132", realMergeSHA, baseSHA)
		// Point the receipt at the empty commit: it is on origin/main and it
		// names a ticket, but it carries nothing.
		r.MergeSHA = emptySHA
		r.Seal()
		err := r.Validate(dir, "FAC-132", integratedState("FAC-132"))
		if err == nil || !strings.Contains(err.Error(), "no patch") {
			t.Fatalf("empty merge commit must be refused for carrying no patch, got %v", err)
		}
	})
}

// --- acceptance criterion: an unrelated ancestor is not task evidence ---

func TestUnrelatedAncestorIsNotTaskEvidence(t *testing.T) {
	dir, baseSHA, mergeSHA, _, otherSHA := receiptRepo(t)

	// otherSHA is a real, non-empty, genuine ancestor of origin/main — the
	// exact shape the old --evidence flag accepted as first-class proof.
	if _, err := git(dir, "merge-base", "--is-ancestor", otherSHA, "origin/main"); err != nil {
		t.Fatalf("fixture: otherSHA must be an ancestor of origin/main: %v", err)
	}
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	r.MergeSHA = otherSHA
	r.Seal()

	err := r.Validate(dir, "FAC-132", integratedState("FAC-132"))
	if err == nil || !strings.Contains(err.Error(), "not the accepted candidate's patch") {
		t.Fatalf("an unrelated ancestor must not satisfy the receipt, got %v", err)
	}
}

// --- acceptance criterion: stale lease/candidate/verification/review/
// acceptance/merge data refuses the transition ---

func TestStaleOrMissingReceiptDataRefuses(t *testing.T) {
	dir, baseSHA, mergeSHA, _, otherSHA := receiptRepo(t)
	ref := "FAC-132"

	cases := []struct {
		name    string
		mutate  func(*CompletionReceipt)
		state   *lifecycle.TaskState
		wantSub string
	}{
		{"stale lease generation", func(r *CompletionReceipt) { r.LeaseGeneration = testLeaseGen - 1 }, integratedState(ref), "stale"},
		{"missing lease generation", func(r *CompletionReceipt) { r.LeaseGeneration = 0 }, integratedState(ref), "lease generation"},
		{"stale candidate", func(r *CompletionReceipt) {
			r.CandidateSHA = strings.Repeat("b", 40)
		}, integratedState(ref), "stale"},
		{"missing verification digest", func(r *CompletionReceipt) { r.VerificationDigest = "" }, integratedState(ref), "verification_digest"},
		{"missing acceptance digest", func(r *CompletionReceipt) { r.AcceptanceDigest = "" }, integratedState(ref), "acceptance_digest"},
		{"missing risk tier", func(r *CompletionReceipt) { r.RiskTier = "" }, integratedState(ref), "risk_tier"},
		{"missing provider revision", func(r *CompletionReceipt) { r.ProviderRevision = "" }, integratedState(ref), "provider_revision"},
		{"self-review families", func(r *CompletionReceipt) { r.ReviewerFamily = r.AuthorFamily }, integratedState(ref), "self-verdict"},
		{"unknown reviewer family", func(r *CompletionReceipt) { r.ReviewerFamily = "acme" }, integratedState(ref), "not a known builder family"},
		{"non-PASS verdict", func(r *CompletionReceipt) { r.Verdict = "FAIL" }, integratedState(ref), "not PASS"},
		{"unmerged integration result", func(r *CompletionReceipt) { r.IntegrationResult = "queued" }, integratedState(ref), "integration result"},
		{"merge sha not on origin/main", func(r *CompletionReceipt) {
			r.MergeSHA = strings.Repeat("c", 40)
		}, integratedState(ref), "not an ancestor of origin/main"},
		{"base not before merge", func(r *CompletionReceipt) { r.BaseSHA = otherSHA; r.MergeSHA = baseSHA }, integratedState(ref), "not an ancestor of merge sha"},
		{"receipt for another task ref", func(r *CompletionReceipt) { r.TaskRef = "FAC-999" }, integratedState(ref), "bound to FAC-999"},
		{"receipt from another repository", func(r *CompletionReceipt) { r.RepoID = "file/somewhere/else" }, integratedState(ref), "bound to repository"},
		{"no lifecycle state at all", func(r *CompletionReceipt) {}, nil, "no durable lifecycle state"},
		{"lifecycle not past integration", func(r *CompletionReceipt) {}, &lifecycle.TaskState{
			TaskRef: ref, State: lifecycle.StateReviewing, LeaseGeneration: testLeaseGen, CandidateSHA: testCandSHA,
		}, "not \"integrated\""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validReceipt(t, dir, ref, mergeSHA, baseSHA)
			tc.mutate(r)
			r.Seal() // a producer would reseal; the gate must still refuse
			err := r.Validate(dir, ref, tc.state)
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("%s: error %q does not mention %q", tc.name, err, tc.wantSub)
			}
		})
	}

	t.Run("hand-edited receipt fails its own digest", func(t *testing.T) {
		r := validReceipt(t, dir, ref, mergeSHA, baseSHA)
		r.RiskTier = "R0" // edited AFTER sealing: no reseal
		err := r.Validate(dir, ref, integratedState(ref))
		if err == nil || !strings.Contains(err.Error(), "digest does not match") {
			t.Fatalf("tampered receipt must be refused, got %v", err)
		}
	})

	t.Run("the unmutated receipt is accepted", func(t *testing.T) {
		r := validReceipt(t, dir, ref, mergeSHA, baseSHA)
		if err := r.Validate(dir, ref, integratedState(ref)); err != nil {
			t.Fatalf("the valid receipt must pass, else every case above proves nothing: %v", err)
		}
	})
}

func TestBoardDoneRefusesWithoutLifecycleAuthority(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	cp := newReceiptBoard(t, "FAC-132", testTaskID)
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)

	_, err := BoardDone(context.Background(), cp, DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
	})
	if !errors.Is(err, ErrNoEvidence) || !strings.Contains(err.Error(), "lifecycle state authority") {
		t.Fatalf("a receipt without lifecycle state must refuse, got %v", err)
	}
	if cp.updates != 0 {
		t.Fatalf("refused card must not be written, updates=%d", cp.updates)
	}
}

func TestBoardDoneRefusesReceiptBoundToAnotherTaskID(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	// Same ref, different provider task id: a re-minted card is a new task.
	cp := newReceiptBoard(t, "FAC-132", "re-minted-task-id")
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)

	_, err := BoardDone(context.Background(), cp, DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
		Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
	})
	if !errors.Is(err, ErrNoEvidence) || !strings.Contains(err.Error(), "task id") {
		t.Fatalf("receipt bound to another task id must refuse, got %v", err)
	}
	if got := statusOf(t, cp, "re-minted-task-id"); got != "in-review" {
		t.Fatalf("refused card moved to %q", got)
	}
}

// --- acceptance criterion: a valid receipt advances exactly once and
// repeated delivery is idempotent ---

func TestValidReceiptAdvancesExactlyOnce(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()
	cp := newReceiptBoard(t, "FAC-132", testTaskID)
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	req := DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
		Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
	}

	res, err := BoardDone(ctx, cp, req)
	if err != nil {
		t.Fatalf("valid receipt must close the card: %v", err)
	}
	if res.Idempotent || res.Overridden || res.ReceiptDigest != r.Digest {
		t.Fatalf("unexpected first result %+v", res)
	}
	if got := statusOf(t, cp, testTaskID); got != "done" {
		t.Fatalf("status = %q, want done", got)
	}

	// Repeated delivery of the same receipt.
	again, err := BoardDone(ctx, cp, req)
	if err != nil {
		t.Fatalf("repeated delivery must be idempotent, got %v", err)
	}
	if !again.Idempotent {
		t.Fatal("repeated delivery must report Idempotent")
	}
	if cp.updates != 1 {
		t.Fatalf("card must advance exactly once, updates=%d", cp.updates)
	}
	if cp.comments != 1 {
		t.Fatalf("repeated delivery must not re-comment, comments=%d", cp.comments)
	}

	log, err := ReadDoneLog(dir)
	if err != nil {
		t.Fatalf("ReadDoneLog: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("done log must hold exactly one record, got %d", len(log))
	}
	if log[0].ReceiptDigest != r.Digest || log[0].MergeSHA != mergeSHA || log[0].ProviderReadback != "done" {
		t.Fatalf("done record does not carry its evidence: %+v", log[0])
	}
}

// --- acceptance criterion: provider write success without matching readback
// is a hard failure ---

func TestProviderWriteWithoutMatchingReadbackIsHardFailure(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	cp := newReceiptBoard(t, "FAC-132", testTaskID)
	cp.lieOnReadTo = "in-review" // write "succeeds", readback disagrees
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)

	_, err := BoardDone(context.Background(), cp, DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
		Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
	})
	if err == nil || !strings.Contains(err.Error(), "reads back as") {
		t.Fatalf("readback mismatch must be a hard failure, got %v", err)
	}
	log, logErr := ReadDoneLog(dir)
	if logErr != nil {
		t.Fatalf("ReadDoneLog: %v", logErr)
	}
	if len(log) != 0 {
		t.Fatalf("a write with no matching readback must record nothing, got %+v", log)
	}
}

// --- acceptance criterion: manual override records actor, reason, evidence,
// and policy decision ---

func TestManualOverrideIsExplicitPolicyLimitedAndAttributable(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()
	full := OverrideRequest{
		Actor: "kampe", Reason: "landed via a hotfix push outside the fleet",
		Evidence: mergeSHA, Policy: "operator-external-merge",
	}

	t.Run("unknown policy is refused", func(t *testing.T) {
		cp := newReceiptBoard(t, "FAC-132", testTaskID)
		bad := full
		bad.Policy = "because-i-said-so"
		_, err := BoardDone(ctx, cp, DoneRequest{RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Override: &bad})
		if !errors.Is(err, ErrNoEvidence) || !strings.Contains(err.Error(), "permitted policies") {
			t.Fatalf("unknown policy must be refused, got %v", err)
		}
		if cp.updates != 0 {
			t.Fatalf("refused override must not write, updates=%d", cp.updates)
		}
	})

	for _, missing := range []string{"actor", "reason", "evidence", "policy"} {
		t.Run("missing "+missing+" is refused", func(t *testing.T) {
			cp := newReceiptBoard(t, "FAC-132", testTaskID)
			req := full
			switch missing {
			case "actor":
				req.Actor = ""
			case "reason":
				req.Reason = ""
			case "evidence":
				req.Evidence = ""
			case "policy":
				req.Policy = ""
			}
			_, err := BoardDone(ctx, cp, DoneRequest{RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Override: &req})
			if !errors.Is(err, ErrNoEvidence) || !strings.Contains(err.Error(), missing) {
				t.Fatalf("override without %s must be refused, got %v", missing, err)
			}
			if cp.updates != 0 {
				t.Fatalf("refused override must not write, updates=%d", cp.updates)
			}
		})
	}

	t.Run("an override cannot ride along with a receipt", func(t *testing.T) {
		cp := newReceiptBoard(t, "FAC-132", testTaskID)
		r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
		_, err := BoardDone(ctx, cp, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-132")}, Override: &full,
		})
		if !errors.Is(err, ErrNoEvidence) {
			t.Fatalf("ambiguous authority must be refused, got %v", err)
		}
		if cp.updates != 0 {
			t.Fatalf("refused request must not write, updates=%d", cp.updates)
		}
	})

	t.Run("a complete override closes the card and is recorded", func(t *testing.T) {
		// A fresh repo so this subtest's done log is its own.
		odir, _, omerge, _, _ := receiptRepo(t)
		cp := newReceiptBoard(t, "FAC-132", testTaskID)
		req := full
		req.Evidence = omerge
		res, err := BoardDone(ctx, cp, DoneRequest{RepoDir: odir, ProjectID: "p1", Ref: "FAC-132", Override: &req})
		if err != nil {
			t.Fatalf("complete override must close the card: %v", err)
		}
		if !res.Overridden {
			t.Fatal("result must report the override")
		}
		if got := statusOf(t, cp, testTaskID); got != "done" {
			t.Fatalf("status = %q, want done", got)
		}
		log, err := ReadDoneLog(odir)
		if err != nil || len(log) != 1 || log[0].Override == nil {
			t.Fatalf("override must be recorded, got %+v err %v", log, err)
		}
		ov := log[0].Override
		if ov.Actor != req.Actor || ov.Reason != req.Reason || ov.Evidence != req.Evidence ||
			ov.Policy != req.Policy || ov.Decision != OverridePolicies[req.Policy] {
			t.Fatalf("override record is incomplete: %+v", ov)
		}
		if log[0].ReceiptDigest != "" {
			t.Fatalf("an override must never claim a receipt digest: %+v", log[0])
		}
	})
}

// --- crash points ---

func TestBoardDoneCrashBetweenReadbackAndRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file-mode injection is meaningless as root")
	}
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()
	cp := newReceiptBoard(t, "FAC-132", testTaskID)
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	req := DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
		Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
	}

	// Readable but unwritable log: the read gate passes, the append fails —
	// i.e. the process dies after the provider readback, before the record.
	logPath := DoneLogPath(dir)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o444); err != nil {
		t.Fatal(err)
	}

	_, err := BoardDone(ctx, cp, req)
	if err == nil || !strings.Contains(err.Error(), "could not be recorded") {
		t.Fatalf("an unrecordable closure must be reported, got %v", err)
	}
	if got := statusOf(t, cp, testTaskID); got != "done" {
		t.Fatalf("the provider write did land; status = %q", got)
	}

	// Replay after the crash: the card is already done, the record is absent.
	if err := os.Chmod(logPath, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := BoardDone(ctx, cp, req)
	if err != nil {
		t.Fatalf("replay after a crash must converge, got %v", err)
	}
	if res.Idempotent {
		t.Fatal("the record was never written, so this replay must actually record it")
	}
	log, err := ReadDoneLog(dir)
	if err != nil || len(log) != 1 {
		t.Fatalf("replay must leave exactly one record, got %+v err %v", log, err)
	}

	// And a third delivery is now the idempotent no-op.
	third, err := BoardDone(ctx, cp, req)
	if err != nil || !third.Idempotent {
		t.Fatalf("third delivery must be idempotent, got %+v err %v", third, err)
	}
	if cp.updates != 2 {
		t.Fatalf("exactly the two pre-record attempts should have written, updates=%d", cp.updates)
	}
}

func TestReadDoneLogRefusesCorruptLog(t *testing.T) {
	dir := t.TempDir()
	logPath := DoneLogPath(dir)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("{\"ref\":\"FAC-1\"}\nnot json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDoneLog(dir); err == nil {
		t.Fatal("a corrupt log must be an error, never an empty 'nothing recorded'")
	}
}

// --- audit ---

func TestAuditDoneReportsSuspiciousClosuresWithoutMutating(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()

	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: testTaskID, Ref: "FAC-132", Title: "closed by receipt", Status: "in-review", ProjectID: "p1"})
	mp.AddTask(&provider.Task{ID: "task-777", Ref: "FAC-777", Title: "closed by an old commit-subject oracle", Status: "done", ProjectID: "p1"})
	mp.AddTask(&provider.Task{ID: "task-500", Ref: "FAC-500", Title: "closed by nothing at all", Status: "done", ProjectID: "p1"})
	mp.AddTask(&provider.Task{ID: "task-321", Ref: "FAC-321", Title: "closed by override", Status: "in-review", ProjectID: "p1"})
	mp.AddTask(&provider.Task{ID: "task-222", Ref: "FAC-222", Title: "still open", Status: "in-progress", ProjectID: "p1"})
	cp := &countingProvider{MemoryProvider: mp}

	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	if _, err := BoardDone(ctx, cp, DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
		Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
	}); err != nil {
		t.Fatalf("receipt close: %v", err)
	}
	if _, err := BoardDone(ctx, cp, DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-321",
		Override: &OverrideRequest{Actor: "kampe", Reason: "duplicate of FAC-132", Evidence: mergeSHA, Policy: "duplicate-card"},
	}); err != nil {
		t.Fatalf("override close: %v", err)
	}

	before := map[string]string{}
	for _, id := range []string{testTaskID, "task-777", "task-500", "task-321", "task-222"} {
		before[id] = statusOf(t, cp, id)
	}

	findings, err := AuditDone(ctx, cp, dir, "p1")
	if err != nil {
		t.Fatalf("AuditDone: %v", err)
	}
	got := map[string]string{}
	for _, f := range findings {
		got[f.Ref] = f.Kind
	}
	if _, reported := got["FAC-132"]; reported {
		t.Fatalf("a receipt-closed card must produce no finding, got %v", got)
	}
	if got["FAC-777"] != AuditCommitHintOnly {
		t.Fatalf("FAC-777 is done on a commit-subject hint alone, got %q", got["FAC-777"])
	}
	if got["FAC-500"] != AuditNoEvidence {
		t.Fatalf("FAC-500 is done with nothing behind it, got %q", got["FAC-500"])
	}
	if got["FAC-321"] != AuditOverride {
		t.Fatalf("FAC-321 was closed by override, got %q", got["FAC-321"])
	}
	if _, reported := got["FAC-222"]; reported {
		t.Fatalf("a card that is not done must not be audited, got %v", got)
	}

	for id, want := range before {
		if now := statusOf(t, cp, id); now != want {
			t.Fatalf("audit mutated %s: %q -> %q", id, want, now)
		}
	}
	if cp.updates != 2 || cp.comments != 2 {
		t.Fatalf("audit must issue no provider writes; updates=%d comments=%d (2 each from setup)", cp.updates, cp.comments)
	}
}

func TestWriteAndLoadReceiptRoundTrip(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	if err := WriteReceipt(dir, r); err != nil {
		t.Fatalf("WriteReceipt: %v", err)
	}
	back, err := LoadReceipt(ReceiptPath(dir, "FAC-132"))
	if err != nil {
		t.Fatalf("LoadReceipt: %v", err)
	}
	if back.Digest != r.Digest {
		t.Fatalf("round trip changed the digest: %s -> %s", r.Digest, back.Digest)
	}
	if err := back.Validate(dir, "FAC-132", integratedState("FAC-132")); err != nil {
		t.Fatalf("a round-tripped receipt must still validate: %v", err)
	}
}

func TestUnrecordedTaskHasNoLifecycleState(t *testing.T) {
	store, err := lifecycle.NewEventStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	defer store.Close()
	st, err := store.CurrentState("FAC-132")
	if err != nil {
		t.Fatalf("CurrentState: %v", err)
	}
	if st != nil {
		t.Fatalf("an unrecorded task must have no state, got %+v", st)
	}
	// And that absence is what Validate refuses on.
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	if err := r.Validate(dir, "FAC-132", st); err == nil {
		t.Fatal("a task with no durable lifecycle state must not close")
	}
}
