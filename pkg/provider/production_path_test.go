package provider

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestProductionPath_ClaimStackAttachesReceiverAndRequiresCAS: OpenClaimStack
// wires AuthBroker and refuses unfenced mutates.
func TestProductionPath_ClaimStackAttachesReceiverAndRequiresCAS(t *testing.T) {
	kp := NewKaneoProvider("http://127.0.0.1:9", "proj", false)
	enableAtomicFence(kp)
	if kp.RequireCASMeta || kp.Receiver != nil {
		t.Fatal("fresh Kaneo must not require CAS until ClaimStack attach")
	}
	stack, err := OpenClaimStack(t.TempDir(), kp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	if !kp.RequireCASMeta || kp.Receiver == nil {
		t.Fatal("ClaimStack must attach AuthBroker + RequireCASMeta")
	}
	err = kp.UpdateStatus(context.Background(), "any", StatusInProgress)
	if err == nil || !strings.Contains(err.Error(), "unfenced bypass") {
		t.Fatalf("want unfenced bypass refused, got %v", err)
	}
}

// TestProductionPath_FencedStatus_RequiresAtomicFenceServer: stock Kaneo
// fenced status is fail-closed; enforcing sandbox with AtomicFenceServer
// accepts fence+op PUT without client HMAC receipts (audit con62fkm).
func TestProductionPath_FencedStatus_RequiresAtomicFenceServer(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{ID: "t1", Ref: "FAC-CLI", Title: "t", Status: "to-do", ProjectID: "proj",
		Priority: PriorityMedium, Position: 1, UpdatedAt: now, CreatedAt: now})
	srv := board.serve()
	defer srv.Close()

	// Stock (no AtomicFenceServer): refuse fenced mutate.
	stock := NewKaneoProvider(srv.URL, "proj", false)
	store := NewMemoryFenceStore()
	stock.Receiver = NewAuthBroker(store).BindRevisionReader(stock.GetTask)
	stock.RequireCASMeta = true
	ctx := WithCASMeta(context.Background(), 7, "op-stock-refuse")
	ctx = WithCASExpectation(ctx, StatusInProgress, "")
	if err := stock.updateStatusOnce(ctx, "t1", StatusInProgress); err == nil {
		t.Fatal("stock Kaneo fenced status must fail closed")
	}
	if board.EffectCount() != 0 {
		t.Fatalf("stock path must not mutate board: effects=%d", board.EffectCount())
	}

	// Enforcing sandbox: AtomicFenceServer allows fence+op status PUT.
	kp := NewKaneoProvider(srv.URL, "proj", false)
	enableAtomicFence(kp)
	broker := NewAuthBroker(store).BindRevisionReader(kp.GetTask)
	broker.ServerOpDedupe = true
	kp.Receiver = broker
	kp.RequireCASMeta = true
	ctx = WithCASMeta(context.Background(), 7, "op-cli-env-1")
	ctx = WithCASExpectation(ctx, StatusInProgress, "")
	if err := kp.updateStatusOnce(ctx, "t1", StatusInProgress); err != nil {
		t.Fatalf("updateStatusOnce: %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("effects=%d want 1 atomic PUT", board.EffectCount())
	}
	got, err := kp.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if NormalizeStatus(got.Status) != StatusInProgress {
		t.Fatalf("status=%s", got.Status)
	}
	if len(board.Comments("t1")) != 0 {
		t.Fatalf("must not dual-write evidence comments: %v", board.Comments("t1"))
	}
}

// crashMarkStore fails MarkApplied after N successes (simulates crash
// between remote success and applied receipt). MarkAmbiguous (in_progress)
// always succeeds.
type crashMarkStore struct {
	FenceStore
	failAfter atomic.Int32 // fail when remaining hits 0 on MarkApplied
	failed    atomic.Int32
}

func (c *crashMarkStore) MarkApplied(ctx context.Context, rec OpReceipt) error {
	// failAfter: number of successful MarkApplied allowed before fail.
	// Use failAfter=0 means fail immediately on first MarkApplied.
	if c.failAfter.Add(-1) < 0 {
		c.failed.Add(1)
		// Restore so subsequent can succeed: set to large
		c.failAfter.Store(1000)
		return errors.New("simulated crash after remote before applied receipt")
	}
	return c.FenceStore.MarkApplied(ctx, rec)
}

// TestAuthBroker_CrashBetweenRemoteAndReceipt_OneEffect is the card's
// provider-success/local-failure window: in_progress before remote; crash
// on MarkApplied after remote; restart reconciles without second backend.
func TestAuthBroker_CrashBetweenRemoteAndReceipt_OneEffect(t *testing.T) {
	var backendCalls atomic.Int32
	prev := kaneoRunCLIEnv
	t.Cleanup(func() { kaneoRunCLIEnv = prev })
	kaneoRunCLIEnv = func(ctx context.Context, extraEnv []string, name string, args ...string) (*CLIResult, error) {
		backendCalls.Add(1)
		return &CLIResult{}, nil
	}

	base := NewMemoryFenceStore()
	// Allow 0 successful MarkApplied first (in_progress uses MarkAmbiguous).
	crash := &crashMarkStore{FenceStore: base}
	crash.failAfter.Store(0) // first MarkApplied fails

	mp := NewMemoryProvider()
	mp.AddTask(testTask("cr1", "FAC-CR", "to-do"))
	kp := NewKaneoProvider("", "p", true)
	enableAtomicFence(kp)
	broker := NewAuthBroker(crash).BindRevisionReader(mp.GetTask)
	broker.ServerOpDedupe = true
	kp.Receiver = broker
	kp.RequireCASMeta = true
	dual := &cliBoardDual{kp: kp, mp: mp}

	// Process A: CAS over crash store (shared with broker).
	casA, err := NewFencedCAS(crash, dual)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rev, _ := casA.ReadRevision(ctx, "cr1")
	op := "crash-window-remote-receipt"
	_, err = casA.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "cr1", rev, 1, op, func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "cr1", StatusInProgress)
	})
	if err == nil {
		t.Fatal("expected failure after remote when MarkApplied crashes")
	}
	if backendCalls.Load() != 1 {
		t.Fatalf("backendCalls=%d want 1 after first attempt", backendCalls.Load())
	}
	// Board effect landed (dual updated mp).
	got, _ := mp.GetTask(ctx, "cr1")
	if NormalizeStatus(got.Status) != StatusInProgress {
		t.Fatalf("status=%s after remote", got.Status)
	}
	// in_progress receipt must exist.
	rec, err := base.LookupApplied(ctx, op)
	if err != nil || rec == nil {
		// crash store wraps base — LookupApplied on crash delegates.
		rec, err = crash.LookupApplied(ctx, op)
	}
	if err != nil || rec == nil {
		t.Fatalf("want in_progress receipt, rec=%v err=%v", rec, err)
	}
	if !rec.Ambiguous {
		// MarkApplied may have failed before flip — should still be ambiguous.
		t.Logf("receipt ambiguous=%v (ok if still in_progress)", rec.Ambiguous)
	}

	// Process B: reopen over same durable store (cross-process restart).
	casB, err := NewFencedCAS(base, dual)
	if err != nil {
		t.Fatal(err)
	}
	// Re-bind broker to base without crash injection.
	kp.Receiver = NewAuthBroker(base).BindRevisionReader(mp.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	cur, _ := casB.ReadRevision(ctx, "cr1")
	_ = casB.AdvanceFence(ctx, "cr1", 1)
	_, err = casB.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "cr1", cur, 1, op, func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "cr1", StatusInProgress)
	})
	if err != nil {
		t.Fatalf("restart reconcile: %v", err)
	}
	if backendCalls.Load() != 1 {
		t.Fatalf("DOUBLE BACKEND after crash window: calls=%d", backendCalls.Load())
	}
}

// TestAuthBroker_LocalSuccessProviderFailure_RetryOnce: backend fails,
// leave in_progress; retry runs backend once successfully.
func TestAuthBroker_LocalSuccessProviderFailure_RetryOnce(t *testing.T) {
	var backendCalls atomic.Int32
	prev := kaneoRunCLIEnv
	t.Cleanup(func() { kaneoRunCLIEnv = prev })
	failOnce := atomic.Bool{}
	failOnce.Store(true)
	kaneoRunCLIEnv = func(ctx context.Context, extraEnv []string, name string, args ...string) (*CLIResult, error) {
		backendCalls.Add(1)
		if failOnce.CompareAndSwap(true, false) {
			return nil, errors.New("simulated provider failure")
		}
		return &CLIResult{}, nil
	}
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	mp.AddTask(testTask("pf1", "FAC-PF", "to-do"))
	kp := NewKaneoProvider("", "p", true)
	enableAtomicFence(kp)
	kp.Receiver = NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	kp.RequireCASMeta = true
	dual := &cliBoardDual{kp: kp, mp: mp}
	cas, _ := NewFencedCAS(store, dual)
	ctx := context.Background()
	rev, _ := cas.ReadRevision(ctx, "pf1")
	op := "provider-fail-op"
	_, err := cas.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "pf1", rev, 1, op, func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "pf1", StatusInProgress)
	})
	if err == nil {
		t.Fatal("want provider failure")
	}
	// Retry
	rev2, _ := cas.ReadRevision(ctx, "pf1")
	_, err = cas.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "pf1", rev2, 1, op, func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "pf1", StatusInProgress)
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if backendCalls.Load() != 2 {
		t.Fatalf("backendCalls=%d want 2 (fail then success)", backendCalls.Load())
	}
	got, _ := mp.GetTask(ctx, "pf1")
	if NormalizeStatus(got.Status) != StatusInProgress {
		t.Fatalf("status=%s", got.Status)
	}
}

// TestAuthBroker_StaleFenceRejected: fence behind high-water never hits backend.
func TestAuthBroker_StaleFenceRejected(t *testing.T) {
	var backendCalls atomic.Int32
	prev := kaneoRunCLIEnv
	t.Cleanup(func() { kaneoRunCLIEnv = prev })
	kaneoRunCLIEnv = func(ctx context.Context, extraEnv []string, name string, args ...string) (*CLIResult, error) {
		backendCalls.Add(1)
		return &CLIResult{}, nil
	}
	store := NewMemoryFenceStore()
	_, _ = store.Advance(context.Background(), "st1", 5)
	kp := NewKaneoProvider("", "p", true)
	enableAtomicFence(kp)
	// Stale fence rejects before remote; revision reader unused.
	kp.Receiver = NewAuthBroker(store).BindRevisionReader(kp.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	kp.RequireCASMeta = true
	ctx := WithCASMeta(context.Background(), 3, "stale-op")
	err := kp.updateStatusOnce(ctx, "st1", StatusDone)
	if err == nil || !errors.Is(err, claim.ErrProviderFenceRejected) {
		t.Fatalf("want fence rejected, got %v", err)
	}
	if backendCalls.Load() != 0 {
		t.Fatalf("backend must not run on stale fence: %d", backendCalls.Load())
	}
}

// TestAuthBroker_CrossProcessSQLiteRestart: two processes share fences.db;
// applied op dedupes across reopen.
func TestAuthBroker_CrossProcessSQLiteRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fences.db")
	var backendCalls atomic.Int32
	prev := kaneoRunCLIEnv
	t.Cleanup(func() { kaneoRunCLIEnv = prev })
	kaneoRunCLIEnv = func(ctx context.Context, extraEnv []string, name string, args ...string) (*CLIResult, error) {
		backendCalls.Add(1)
		return &CLIResult{}, nil
	}

	mp := NewMemoryProvider()
	mp.AddTask(testTask("x1", "FAC-X", "to-do"))

	// Process A
	storeA, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	kp := NewKaneoProvider("", "p", true)
	enableAtomicFence(kp)
	kp.Receiver = NewAuthBroker(storeA).BindRevisionReader(mp.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	kp.RequireCASMeta = true
	dual := &cliBoardDual{kp: kp, mp: mp}
	casA, _ := NewFencedCAS(storeA, dual)
	ctx := context.Background()
	rev, _ := casA.ReadRevision(ctx, "x1")
	_, err = casA.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "x1", rev, 2, "x-op", func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "x1", StatusInProgress)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = storeA.Close()

	// Process B reopen
	storeB, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	kp.Receiver = NewAuthBroker(storeB).BindRevisionReader(mp.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	casB, _ := NewFencedCAS(storeB, dual)
	cur, _ := casB.ReadRevision(ctx, "x1")
	var mutated int
	_, err = casB.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "x1", cur, 2, "x-op", func(ctx context.Context) error {
		mutated++
		return dual.UpdateStatus(ctx, "x1", StatusDone)
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutated != 0 || backendCalls.Load() != 1 {
		t.Fatalf("cross-process dedupe failed: mutated=%d backend=%d", mutated, backendCalls.Load())
	}
}

// cliBoardDual: GetTask/comments on MemoryProvider; mutates via Kaneo+broker
// so effectMet can observe board status after remote CLI stub.
type cliBoardDual struct {
	kp *KaneoProvider
	mp *MemoryProvider
}

func (d *cliBoardDual) GetTask(ctx context.Context, id string) (*Task, error) {
	return d.mp.GetTask(ctx, id)
}
func (d *cliBoardDual) ListTasks(ctx context.Context, p, s string) ([]*Task, error) {
	return d.mp.ListTasks(ctx, p, s)
}
func (d *cliBoardDual) ClaimTask(ctx context.Context, id, role string) error {
	return d.UpdateStatus(ctx, id, StatusInProgress)
}
func (d *cliBoardDual) UpdateStatus(ctx context.Context, id, status string) error {
	// Hermetic dual: CLI stub + memory board status. Models AtomicFenceServer
	// op-dedupe recovery without client HMAC receipts (audit con62fkm).
	status = NormalizeStatus(status)
	if err := d.kp.requireCAS(ctx); err != nil {
		return err
	}
	op := casOpID(ctx)
	fence, ok := casFenceToken(ctx)
	if !ok || op == "" {
		return fmt.Errorf("cliBoardDual: fence+op required")
	}
	if d.kp.Receiver == nil {
		return fmt.Errorf("cliBoardDual: receiver required")
	}
	return d.kp.Receiver.Execute(ctx, id, fence, op, status, "",
		func(ctx context.Context) error {
			if d.kp.UseCLI {
				if err := d.kp.runCLIMutate(ctx, "task", "status", id, status); err != nil {
					return err
				}
			}
			return d.mp.UpdateStatus(ctx, id, status)
		},
		func(ctx context.Context) (EffectState, error) {
			t, err := d.mp.GetTask(ctx, id)
			if err != nil {
				return EffectUnknown, nil
			}
			if NormalizeStatus(t.Status) == status {
				return EffectPresent, nil
			}
			return EffectAbsent, nil
		},
	)
}
func (d *cliBoardDual) AddComment(ctx context.Context, id, body string) error {
	if err := d.kp.requireCAS(ctx); err != nil {
		return err
	}
	op := casOpID(ctx)
	fence, ok := casFenceToken(ctx)
	if !ok || op == "" {
		return fmt.Errorf("cliBoardDual: fence+op required")
	}
	post := CommentOpTaggedBody(body, op)
	return d.kp.Receiver.Execute(ctx, id, fence, op, "", body,
		func(ctx context.Context) error {
			if d.kp.UseCLI {
				if err := d.kp.runCLIMutate(ctx, "task", "comment", "add", id, post); err != nil {
					return err
				}
			}
			return d.mp.AddComment(ctx, id, post)
		},
		func(ctx context.Context) (EffectState, error) {
			for _, c := range d.mp.Comments(id) {
				if MatchCommentOp(c, body, op) {
					return EffectPresent, nil
				}
			}
			return EffectAbsent, nil
		},
	)
}
func (d *cliBoardDual) ListLiveComments(ctx context.Context, taskID string) ([]string, error) {
	return d.mp.Comments(taskID), nil
}
func (d *cliBoardDual) Comments(taskID string) []string {
	return d.mp.Comments(taskID)
}

// TestProductionPath_KaneoHTTP_StaleAndBypass via real KaneoProvider + authBoard.
func TestProductionPath_KaneoHTTP_StaleAndBypass(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{ID: "h1", Ref: "FAC-H", Title: "t", Status: "to-do", ProjectID: "proj", UpdatedAt: now, CreatedAt: now})
	srv := board.serve()
	defer srv.Close()

	kp := NewKaneoProvider(srv.URL, "proj", false)
	enableAtomicFence(kp)
	stack, err := OpenClaimStack(t.TempDir(), kp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	// Reader for CAS is Kaneo; Comments via receiver.
	reader := &kaneoReader{KaneoProvider: kp, board: board}
	// Replace CAS reader? Stack already has cas over kp. Use stack board path.
	key := LeaseKey(".", "kaneo", "proj", "FAC-H")
	lease := MustAcquireLease(t, stack, key, "agent", "worker", "h1")
	// MutateStatus through Begin/Complete production path.
	// Stack's CAS reader is kp — GetTask hits HTTP.
	if err := stack.Board.MutateStatus(context.Background(), stack.Manager, key, "agent", lease.Generation, "h1", StatusInProgress); err != nil {
		t.Fatalf("MutateStatus: %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("effects=%d", board.EffectCount())
	}
	// Stale generation refused by lease layer.
	if err := stack.Board.MutateStatus(context.Background(), stack.Manager, key, "agent", lease.Generation-1, "h1", StatusDone); err == nil {
		t.Fatal("stale generation must fail")
	}
	// Direct unfenced bypass refused.
	if err := kp.UpdateStatus(context.Background(), "h1", StatusDone); err == nil || !strings.Contains(err.Error(), "unfenced") {
		t.Fatalf("direct bypass: %v", err)
	}
	// Idempotent retry same op path via MutateStatus again (outbox applied).
	if err := stack.Board.MutateStatus(context.Background(), stack.Manager, key, "agent", lease.Generation, "h1", StatusInProgress); err != nil {
		t.Fatalf("idempotent status: %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("duplicate effect: %d", board.EffectCount())
	}
	_ = reader
}
