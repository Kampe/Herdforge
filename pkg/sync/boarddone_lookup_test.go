package sync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// listTasksCountingProvider wraps MemoryProvider to count ListTasks invocations
// and optionally fail ListTasks on demand.
type listTasksCountingProvider struct {
	*provider.MemoryProvider
	listTasksCount int64
	failListTasks  bool
}

func (p *listTasksCountingProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	atomic.AddInt64(&p.listTasksCount, 1)
	if p.failListTasks {
		return nil, errors.New("ListTasks provider_timeout after 30s")
	}
	return p.MemoryProvider.ListTasks(ctx, projectID, status)
}

func TestBoardDone_ExactReceiptTaskLookup_NoListTasks(t *testing.T) {
	dir, baseSHA, realMergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()

	// 1. Acceptance Criterion 1: Hermetic public native approval fixture with authentic receipt:
	// ListTasks deliberately errors and counts calls; exact GetTask returns matching identity,
	// native completion succeeds, ListTasks is never called.
	t.Run("receipt-bound closure uses exact GetTask and never calls ListTasks", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "fix exact lookup",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		counting := &listTasksCountingProvider{
			MemoryProvider: mp,
			failListTasks:  true, // ListTasks is configured to fail closed if called!
		}

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		bindLiveRevision(t, receipt, mp, "task-exact-123")

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		res, err := BoardDone(ctx, counting, req)
		if err != nil {
			t.Fatalf("expected receipt-bound BoardDone to succeed without ListTasks, got: %v", err)
		}
		if res == nil || res.TaskID != "task-exact-123" {
			t.Fatalf("unexpected DoneResult: %+v", res)
		}
		if atomic.LoadInt64(&counting.listTasksCount) != 0 {
			t.Fatalf("expected 0 ListTasks calls, got %d", counting.listTasksCount)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got.Status != "done" {
			t.Fatalf("card status must be 'done', got %q", got.Status)
		}
	})

	// 2. Acceptance Criterion 2: Wrong task id/ref/project and HTTP200 error-shaped exact read refuse before claim/mint/mutation; record zero writes.
	t.Run("mismatched task id from GetTask refuses before mutation", func(t *testing.T) {
		mockP := &customGetTaskProvider{
			MemoryProvider: provider.NewMemoryProvider(),
			getTaskFunc: func(ctx context.Context, id string) (*provider.Task, error) {
				return &provider.Task{ID: "different-id", Ref: "FAC-783", Status: "in-review", ProjectID: "p1"}, nil
			},
		}
		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		receipt.Seal()

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mockP, req)
		if err == nil || !strings.Contains(err.Error(), "receipt task id task-exact-123 does not match board task id different-id") {
			t.Fatalf("expected task id mismatch refusal, got %v", err)
		}
	})

	t.Run("mismatched ref refuses before mutation", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-999", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		receipt.Seal()

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mp, req)
		if err == nil || (!strings.Contains(err.Error(), "board task ref FAC-999 does not match") && !strings.Contains(err.Error(), "does not match")) {
			t.Fatalf("expected ref mismatch refusal, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got.Status != "in-review" {
			t.Fatalf("zero writes expected: status changed to %q", got.Status)
		}
	})

	t.Run("mismatched project id refuses before mutation", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "other-project", Description: testAcceptanceDescription,
		})

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		receipt.Seal()

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mp, req)
		if err == nil || (!strings.Contains(err.Error(), "board task project other-project does not match") && !strings.Contains(err.Error(), "does not match")) {
			t.Fatalf("expected project id mismatch refusal, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got.Status != "in-review" {
			t.Fatalf("zero writes expected: status changed to %q", got.Status)
		}
	})

	t.Run("HTTP 200 error-shaped exact read refuses before mutation", func(t *testing.T) {
		mockP := &customGetTaskProvider{
			MemoryProvider: provider.NewMemoryProvider(),
			getTaskFunc: func(ctx context.Context, id string) (*provider.Task, error) {
				// HTTP 200 response carrying error body deserialized as empty/invalid task
				return &provider.Task{ID: ""}, nil
			},
		}
		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		receipt.Seal()

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mockP, req)
		if err == nil || !strings.Contains(err.Error(), "error-shaped response") {
			t.Fatalf("expected error-shaped read refusal, got %v", err)
		}
	})

	t.Run("missing provider_revision in full receipt refuses before mutation", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		receipt.ProviderRevision = "" // Stripped provider revision
		receipt.Seal()

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mp, req)
		if err == nil || !strings.Contains(err.Error(), "receipt is missing provider_revision") {
			t.Fatalf("expected missing provider_revision refusal, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got.Status != "in-review" {
			t.Fatalf("zero writes expected: status changed to %q", got.Status)
		}
	})

	// 3. Acceptance Criterion 3: Unauthenticated caller without receipt cannot bypass receipt evidence
	t.Run("unauthenticated caller without receipt falls back to resolveTaskByRef and fails without evidence", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   nil, // No receipt!
		}

		_, err := BoardDone(ctx, mp, req)
		if err == nil || !errors.Is(err, ErrNoEvidence) {
			t.Fatalf("expected ErrNoEvidence for unauthenticated request without receipt, got %v", err)
		}
	})

	// 4. Acceptance Criterion 4: Focused race tests
	t.Run("concurrent receipt-bound closures are race free", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-race-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})
		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-race-123"
		bindLiveRevision(t, receipt, mp, "task-race-123")

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = BoardDone(ctx, mp, req)
			}()
		}
		wg.Wait()
		got, _ := mp.GetTask(ctx, "task-race-123")
		if got.Status != "done" {
			t.Fatalf("concurrent BoardDone must leave task done, got %q", got.Status)
		}
	})
}

type customGetTaskProvider struct {
	*provider.MemoryProvider
	getTaskFunc  func(ctx context.Context, id string) (*provider.Task, error)
	getTaskCalls int64
}

func (c *customGetTaskProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	atomic.AddInt64(&c.getTaskCalls, 1)
	if c.getTaskFunc != nil {
		return c.getTaskFunc(ctx, id)
	}
	return c.MemoryProvider.GetTask(ctx, id)
}

// reducedReceiptWithoutTaskID mints a sealed reduced-provenance receipt — the
// mode whose signed authority deliberately omits task identity and provider
// revision (FAC-629), used here to pin the reduced compatibility rule.
func reducedReceiptWithoutTaskID(t *testing.T, dir, ref, mergeSHA, baseSHA string) *CompletionReceipt {
	t.Helper()
	r := validReceipt(t, dir, ref, mergeSHA, baseSHA)
	r.TaskID = ""
	r.ProviderRevision = ""
	r.LeaseGeneration = 0
	r.AcceptanceDigest = ""
	r.AcceptanceEvidence = ""
	r.ProvenanceMode = ProvenanceReduced
	r.PullRequest = 783
	r.Seal()
	return r
}

// --- FAC-783 finding 1: a full receipt's provider revision is bound to the
// exact live task read, not merely required to be non-empty ---

func TestBoardDone_ReceiptRevisionBoundToLiveTask(t *testing.T) {
	dir, baseSHA, realMergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()

	t.Run("stale revision refuses with zero writes", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		receipt.ProviderRevision = "provider-rev-1" // not the live revision
		receipt.Seal()

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mp, req)
		if err == nil || !strings.Contains(err.Error(), "does not match live task revision") {
			t.Fatalf("stale receipt revision must refuse the close, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got.Status != "in-review" {
			t.Fatalf("zero writes expected: status changed to %q", got.Status)
		}
	})

	t.Run("receipt bound to the live revision closes", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		bindLiveRevision(t, receipt, mp, "task-exact-123")

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		if _, err := BoardDone(ctx, mp, req); err != nil {
			t.Fatalf("receipt bound to the live revision must close, got: %v", err)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got.Status != "done" {
			t.Fatalf("card status must be 'done', got %q", got.Status)
		}
	})
}

// --- FAC-783 finding 2: authentication precedes any use of the receipt's
// locator on the provider ---

func TestBoardDone_AuthenticatesReceiptBeforeLocatorUse(t *testing.T) {
	dir, baseSHA, realMergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()

	t.Run("tampered receipt triggers zero provider reads", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})
		mockP := &customGetTaskProvider{MemoryProvider: mp}

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		receipt.RiskTier = "R0" // edited AFTER sealing: no reseal
		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mockP, req)
		if err == nil || !strings.Contains(err.Error(), "digest does not match") {
			t.Fatalf("tampered receipt must be refused on its digest, got %v", err)
		}
		if atomic.LoadInt64(&mockP.getTaskCalls) != 0 {
			t.Fatalf("unauthenticated receipt must not reach the provider: %d GetTask calls", mockP.getTaskCalls)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got.Status != "in-review" {
			t.Fatalf("zero writes expected: status changed to %q", got.Status)
		}
	})
}

// --- FAC-783 finding 3: a full receipt without its required task identity
// rejects before any provider call; only reduced receipts may use the
// legacy ref resolver ---

func TestResolveDoneTask_FullReceiptIdentityRejectsBeforeProviderCalls(t *testing.T) {
	dir, baseSHA, realMergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()

	t.Run("full receipt without task id refuses ResolveDoneTask without provider calls", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})
		mockP := &customGetTaskProvider{MemoryProvider: mp}
		counting := &listTasksCountingProvider{
			MemoryProvider: mp,
			failListTasks:  true,
		}

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "" // stripped signed identity
		receipt.Seal()

		_, err := ResolveDoneTask(ctx, counting, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-783", Receipt: receipt,
		})
		if err == nil || !strings.Contains(err.Error(), "receipt is missing task_id") {
			t.Fatalf("full receipt without task id must reject, got %v", err)
		}
		if atomic.LoadInt64(&counting.listTasksCount) != 0 {
			t.Fatalf("missing identity must never reach ListTasks: %d calls", counting.listTasksCount)
		}
		if atomic.LoadInt64(&mockP.getTaskCalls) != 0 {
			t.Fatalf("missing identity must never reach GetTask: %d calls", mockP.getTaskCalls)
		}
	})

	t.Run("full receipt without task id refuses BoardDone with zero provider calls", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})
		counting := &listTasksCountingProvider{
			MemoryProvider: mp,
			failListTasks:  true,
		}

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = ""
		receipt.Seal()

		_, err := BoardDone(ctx, counting, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-783", Receipt: receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		})
		if err == nil || !strings.Contains(err.Error(), "task_id") {
			t.Fatalf("full receipt without task id must refuse the close, got %v", err)
		}
		if atomic.LoadInt64(&counting.listTasksCount) != 0 {
			t.Fatalf("invalid receipt must not trigger a provider-wide lookup: %d ListTasks calls", counting.listTasksCount)
		}
	})

	t.Run("reduced receipt without task id resolves by ref", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "live-board-task", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1",
			Description: "## Acceptance criteria\n\n- [x] independently reviewed work landed",
		})

		receipt := reducedReceiptWithoutTaskID(t, dir, "FAC-783", realMergeSHA, baseSHA)
		task, err := ResolveDoneTask(ctx, mp, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-783", Receipt: receipt,
		})
		if err != nil {
			t.Fatalf("reduced receipt must resolve through the explicit ref rule, got: %v", err)
		}
		if task == nil || task.ID != "live-board-task" {
			t.Fatalf("unexpected resolution: %+v", task)
		}
	})
}

// --- FAC-783 finding 4: the exact receipt path requires the provider record
// to carry its canonical ref and project identity ---

func TestResolveDoneTask_ExactReadRequiresCompleteProviderIdentity(t *testing.T) {
	dir, baseSHA, realMergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()

	t.Run("partial provider record refuses before mutation", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mockP := &customGetTaskProvider{
			MemoryProvider: mp,
			getTaskFunc: func(ctx context.Context, id string) (*provider.Task, error) {
				// A record that echoes only the requested id plus an acceptance
				// block: no canonical ref, no project identity.
				return &provider.Task{ID: id, Status: "in-review", Description: testAcceptanceDescription}, nil
			},
		}

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		bindLiveRevision(t, receipt, mockP, "task-exact-123")

		req := DoneRequest{
			RepoDir:   dir,
			ProjectID: "p1",
			Ref:       "FAC-783",
			Receipt:   receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		}

		_, err := BoardDone(ctx, mockP, req)
		if err == nil || !strings.Contains(err.Error(), "carries no canonical ref") {
			t.Fatalf("partial provider record must refuse the close, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "task-exact-123")
		if got != nil && got.Status != "in-review" {
			t.Fatalf("zero writes expected: status changed to %q", got.Status)
		}
	})

	t.Run("exact read with empty ref refuses before mutation", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		bindLiveRevision(t, receipt, mp, "task-exact-123")

		_, err := BoardDone(ctx, mp, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-783", Receipt: receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		})
		if err == nil || !strings.Contains(err.Error(), "carries no canonical ref") {
			t.Fatalf("board task without canonical ref must refuse, got %v", err)
		}
	})

	t.Run("exact lookup without a requested project refuses", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{
			ID: "task-exact-123", Ref: "FAC-783", Title: "t",
			Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription,
		})

		receipt := validReceipt(t, dir, "FAC-783", realMergeSHA, baseSHA)
		receipt.TaskID = "task-exact-123"
		bindLiveRevision(t, receipt, mp, "task-exact-123")

		_, err := BoardDone(ctx, mp, DoneRequest{
			RepoDir: dir, ProjectID: "", Ref: "FAC-783", Receipt: receipt,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-783")},
		})
		if err == nil || !strings.Contains(err.Error(), "requires the requested project identity") {
			t.Fatalf("exact receipt lookup without project binding must refuse, got %v", err)
		}
	})
}

// TestBoardDone_TerminalCardRequiresReceiptBoundDoneLog is the FAC-783
// reviewer-c7102a76 regression: a done card accepts a full receipt only from
// receipt-bound durable evidence — the done log's record of THIS receipt's
// digest — never from generic done status.
func TestBoardDone_TerminalCardRequiresReceiptBoundDoneLog(t *testing.T) {
	ctx := context.Background()

	t.Run("sealed stale full receipt on a done card with no matching log record refuses with zero mutation", func(t *testing.T) {
		dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
		cp := newReceiptBoard(t, "FAC-132", testTaskID)
		r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
		bindLiveRevision(t, r, cp, testTaskID)
		// The card reached done through some other authority after the
		// receipt was minted: done status, no done-log record anywhere.
		if err := cp.UpdateStatus(ctx, testTaskID, "done"); err != nil {
			t.Fatal(err)
		}
		baseline := cp.updates
		req := DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
		}
		if _, err := BoardDone(ctx, cp, req); err == nil || !strings.Contains(err.Error(), "cannot be accepted on stale evidence") {
			t.Fatalf("a validly sealed stale receipt with no matching done-log digest must refuse on a done card, got %v", err)
		}
		if got := statusOf(t, cp, testTaskID); got != "done" {
			t.Fatalf("status = %q, want unchanged done", got)
		}
		if cp.updates != baseline || cp.comments != 0 {
			t.Fatalf("zero mutation on refusal: updates=%d baseline=%d comments=%d", cp.updates, baseline, cp.comments)
		}
		log, err := ReadDoneLog(dir)
		if err != nil || len(log) != 0 {
			t.Fatalf("the refusal must append no done-log record, got %+v err %v", log, err)
		}
	})

	t.Run("a different validly sealed receipt digest never passes as the terminal card's evidence", func(t *testing.T) {
		dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
		cp := newReceiptBoard(t, "FAC-132", testTaskID)
		rA := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
		bindLiveRevision(t, rA, cp, testTaskID)
		if _, err := BoardDone(ctx, cp, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: rA,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
		}); err != nil {
			t.Fatalf("receipt A close: %v", err)
		}
		if rA.Digest == "" {
			t.Fatal("receipt A must carry a digest")
		}
		// Receipt B is validly sealed for the same candidate, merge, and card,
		// but carries a different verification proof — hence a different
		// digest — and this card's done log never recorded it.
		rB := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
		rB.VerificationDigest = "verification-digest-2"
		rB.Seal()
		if rB.Digest == rA.Digest {
			t.Fatal("receipt B must hash differently from receipt A")
		}
		req := DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: rB,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
		}
		// First delivery: recorded-digest idempotence short-circuits only for
		// A's digest, so B refuses as stale evidence on a terminal card.
		if _, err := BoardDone(ctx, cp, req); err == nil || !strings.Contains(err.Error(), "cannot be accepted on stale evidence") {
			t.Fatalf("a different sealed digest must refuse on a done card, got %v", err)
		}
		if cp.updates != 1 {
			t.Fatalf("zero mutation from the refusal: exactly receipt A's write should stand, updates=%d", cp.updates)
		}
		// A itself remains idempotent via the digest short-circuit.
		res, err := BoardDone(ctx, cp, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: rA,
			Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
		})
		if err != nil || !res.Idempotent {
			t.Fatalf("receipt A replay must stay idempotent, got %+v err %v", res, err)
		}
		if cp.updates != 1 {
			t.Fatalf("idempotent replay must not write again, updates=%d", cp.updates)
		}
	})
}

// TestResolveDoneTask_TerminalCardMatchingRevisionStillRequiresDoneLog is the
// FAC-783 reviewer-dc7d0755 regression: a sealed full receipt whose provider
// revision matches the live DONE revision is still refused without a
// matching done-log record — a matching revision is not proof this receipt
// effected the done transition.
func TestResolveDoneTask_TerminalCardMatchingRevisionStillRequiresDoneLog(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()
	cp := newReceiptBoard(t, "FAC-132", testTaskID)
	// Drive the card terminal through another authority, THEN bind the
	// receipt to the live done revision: the revision now matches, and no
	// done-log record exists anywhere.
	if err := cp.UpdateStatus(ctx, testTaskID, "done"); err != nil {
		t.Fatal(err)
	}
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	bindLiveRevision(t, r, cp, testTaskID)
	live, err := cp.GetTask(ctx, testTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if string(provider.EncodeRevision(live)) != r.ProviderRevision {
		t.Fatalf("fixture must bind the matching revision: receipt %q live %q", r.ProviderRevision, provider.EncodeRevision(live))
	}
	req := DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
		Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
	}
	if _, err := ResolveDoneTask(ctx, cp, req); err == nil ||
		!strings.Contains(err.Error(), "cannot be accepted on stale evidence") {
		t.Fatalf("a revision-matching sealed receipt on a terminal card must still require the done-log record, got %v", err)
	}
}

func TestBoardDone_TerminalMatchingRevisionWithoutLogRecordRefusesWithoutMutating(t *testing.T) {
	dir, baseSHA, mergeSHA, _, _ := receiptRepo(t)
	ctx := context.Background()
	cp := newReceiptBoard(t, "FAC-132", testTaskID)
	if err := cp.UpdateStatus(ctx, testTaskID, "done"); err != nil {
		t.Fatal(err)
	}
	baseline := cp.updates
	r := validReceipt(t, dir, "FAC-132", mergeSHA, baseSHA)
	bindLiveRevision(t, r, cp, testTaskID)
	req := DoneRequest{
		RepoDir: dir, ProjectID: "p1", Ref: "FAC-132", Receipt: r,
		Lifecycle: fakeLifecycle{st: integratedState("FAC-132")},
	}
	if _, err := BoardDone(ctx, cp, req); err == nil ||
		!strings.Contains(err.Error(), "cannot be accepted on stale evidence") {
		t.Fatalf("unfenced close must refuse a revision-matching receipt with no done-log record, got %v", err)
	}
	if got := statusOf(t, cp, testTaskID); got != "done" {
		t.Fatalf("status = %q, want unchanged done", got)
	}
	if cp.updates != baseline || cp.comments != 0 {
		t.Fatalf("zero provider write on refusal: updates=%d baseline=%d comments=%d", cp.updates, baseline, cp.comments)
	}
	log, err := ReadDoneLog(dir)
	if err != nil || len(log) != 0 {
		t.Fatalf("the refusal must not fabricate a done-log record, got %+v err %v", log, err)
	}
}
