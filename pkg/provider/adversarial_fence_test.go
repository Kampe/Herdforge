package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestAdversarial_Gen2PreemptsGen1_PresentEquivalent: gen2 advances fence while
// board effect still matches gen1 expectation (both InProgress). Gen1 must
// fence-reject BEFORE MarkApplied on ambiguous-Present (hold inb04ouq).
func TestAdversarial_Gen2PreemptsGen1_PresentEquivalent(t *testing.T) {
	var backendCalls atomic.Int32
	prev := kaneoRunCLIEnv
	t.Cleanup(func() { kaneoRunCLIEnv = prev })
	kaneoRunCLIEnv = func(ctx context.Context, extraEnv []string, name string, args ...string) (*CLIResult, error) {
		backendCalls.Add(1)
		return &CLIResult{}, nil
	}

	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	mp.AddTask(testTask("g1", "FAC-G", "to-do"))
	kp := NewKaneoProvider("", "p", true)
	enableAtomicFence(kp)
	kp.RequireCASMeta = true
	dual := &cliBoardDual{kp: kp, mp: mp}

	// Gen1: crash after remote InProgress (applied fails).
	crash := &crashMarkStore{FenceStore: store}
	crash.failAfter.Store(0)
	kp.Receiver = NewAuthBroker(crash).BindRevisionReader(dual.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	casA, _ := NewFencedCAS(crash, dual)
	ctx := context.Background()
	rev, _ := casA.ReadRevision(ctx, "g1")
	_, err := casA.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "g1", rev, 1, "op-gen1", func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "g1", StatusInProgress)
	})
	if err == nil {
		t.Fatal("want crash after remote")
	}
	got, _ := mp.GetTask(ctx, "g1")
	if NormalizeStatus(got.Status) != StatusInProgress {
		t.Fatalf("gen1 board=%s", got.Status)
	}

	// Gen2: advance fence only (board stays InProgress — PRESENT-equivalent).
	kp.Receiver = NewAuthBroker(store).BindRevisionReader(dual.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	cas2, _ := NewFencedCAS(store, dual)
	if err := cas2.AdvanceFence(ctx, "g1", 2); err != nil {
		t.Fatal(err)
	}
	// Optional gen2 op that keeps status InProgress.
	rev2, _ := cas2.ReadRevision(ctx, "g1")
	_, err = cas2.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "g1", rev2, 2, "op-gen2-keep", func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "g1", StatusInProgress)
	})
	if err != nil {
		t.Fatalf("gen2: %v", err)
	}

	// Gen1 Present-equivalent retry must be fence-rejected (not MarkApplied success).
	before := backendCalls.Load()
	cas1, _ := NewFencedCAS(store, dual)
	rev3, _ := cas1.ReadRevision(ctx, "g1")
	_, err = cas1.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "g1", rev3, 1, "op-gen1", func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "g1", StatusInProgress)
	})
	if err == nil || !errors.Is(err, claim.ErrProviderFenceRejected) {
		t.Fatalf("want gen1 fence reject on Present-equivalent, got %v", err)
	}
	if backendCalls.Load() != before {
		t.Fatalf("gen1 must not hit backend: %d→%d", before, backendCalls.Load())
	}
	// Receipt must not become applied under gen1 after gen2 high-water.
	rec, _ := store.LookupApplied(ctx, "op-gen1")
	if rec != nil && !rec.Ambiguous {
		t.Fatal("stale Present path must not MarkApplied gen1 after gen2 high-water")
	}
}

// TestAdversarial_AppliedReceiptMonotonic: MarkAmbiguous cannot erase applied.
func TestAdversarial_AppliedReceiptMonotonic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.db")
	store, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rec := OpReceipt{OpID: "mono-1", TaskID: "t1", FenceToken: 1, Revision: "rev-A", ExpectedStatus: StatusDone}
	if err := store.MarkApplied(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// Outer CAS post-read failure path would try MarkAmbiguous — must no-op.
	if err := store.MarkAmbiguous(ctx, OpReceipt{OpID: "mono-1", TaskID: "t1", FenceToken: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LookupApplied(ctx, "mono-1")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Ambiguous {
		t.Fatal("applied must stay non-ambiguous")
	}
	if got.Revision != "rev-A" {
		t.Fatalf("revision evidence lost: %q", got.Revision)
	}
	if got.ExpectedStatus != StatusDone {
		t.Fatalf("status evidence lost: %q", got.ExpectedStatus)
	}

	mem := NewMemoryFenceStore()
	_ = mem.MarkApplied(ctx, rec)
	_ = mem.MarkAmbiguous(ctx, OpReceipt{OpID: "mono-1", TaskID: "t1", FenceToken: 1})
	g2, _ := mem.LookupApplied(ctx, "mono-1")
	if g2.Ambiguous || g2.Revision != "rev-A" {
		t.Fatalf("memory monotonic fail: %+v", g2)
	}
}

// TestAdversarial_ListLiveComments_HTTPUnknown: production path when activity
// endpoint fails — never inject EffectUnknown enum.
func TestAdversarial_ListLiveComments_HTTPUnknown(t *testing.T) {
	var statusPosts atomic.Int32
	var commentPosts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/activity/") {
			http.Error(w, `{"error":"timeout"}`, http.StatusGatewayTimeout)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comment") {
			commentPosts.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.Method == http.MethodPut {
			statusPosts.Add(1)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/task/") {
			jsonEncodeTask(w, "u1", "to-do")
			return
		}
		http.Error(w, "no", 404)
	}))
	defer srv.Close()

	store := NewMemoryFenceStore()
	kp := NewKaneoProvider(srv.URL, "p", false)
	enableAtomicFence(kp)
	kp.Receiver = NewAuthBroker(store).BindRevisionReader(kp.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	kp.RequireCASMeta = true

	// Seed ambiguous comment op (crash after remote would look like this if
	// list is down — force ambiguous + attempt recovery via addCommentOnce).
	_ = store.MarkAmbiguous(context.Background(), OpReceipt{
		OpID: "cmt-unk", TaskID: "u1", FenceToken: 1, ExpectedComment: "body",
	})
	_, _ = store.Advance(context.Background(), "u1", 1)

	before := commentPosts.Load()
	ctx := WithCASMeta(context.Background(), 1, "cmt-unk")
	ctx = WithCASExpectation(ctx, "", "body")
	err := kp.addCommentOnce(ctx, "u1", "body")
	if err == nil || !errors.Is(err, claim.ErrProviderAmbiguous) {
		t.Fatalf("want UNKNOWN ambiguous from live list failure, got %v", err)
	}
	if commentPosts.Load() != before {
		t.Fatalf("UNKNOWN must not re-post comments: %d→%d", before, commentPosts.Load())
	}
	// Prove ListLiveComments itself errors (not enum injection).
	_, lerr := kp.ListLiveComments(context.Background(), "u1")
	if lerr == nil {
		t.Fatal("ListLiveComments must fail on 504 activity")
	}
	_ = statusPosts.Load() // status path unused in this comment-only case
}

// TestAdversarial_CommentOpBoundIdentity: two ops, same free-text body, distinct
// markers — prior comment must not satisfy the second op.
func TestAdversarial_CommentOpBoundIdentity(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{ID: "id1", Ref: "FAC-ID", Title: "t", Status: "to-do", ProjectID: "p", UpdatedAt: now, CreatedAt: now})
	srv := board.serve()
	defer srv.Close()

	kp := NewKaneoProvider(srv.URL, "p", false)
	enableAtomicFence(kp)
	store := NewMemoryFenceStore()
	kp.Receiver = NewAuthBroker(store).BindRevisionReader(kp.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }
	kp.RequireCASMeta = true

	ctx1 := WithCASMeta(context.Background(), 1, "op-aaa")
	ctx1 = WithCASExpectation(ctx1, "", "same body")
	if err := kp.addCommentOnce(ctx1, "id1", "same body"); err != nil {
		t.Fatal(err)
	}
	// Second distinct op with identical free text.
	ctx2 := WithCASMeta(context.Background(), 1, "op-bbb")
	ctx2 = WithCASExpectation(ctx2, "", "same body")
	// Ambiguous recovery for op-bbb: live has op-aaa only → ABSENT → re-post.
	_ = store.MarkAmbiguous(context.Background(), OpReceipt{
		OpID: "op-bbb", TaskID: "id1", FenceToken: 1, ExpectedComment: "same body",
	})
	if err := kp.addCommentOnce(ctx2, "id1", "same body"); err != nil {
		t.Fatalf("op-bbb: %v", err)
	}
	live, err := kp.ListLiveComments(context.Background(), "id1")
	if err != nil {
		t.Fatal(err)
	}
	var hasA, hasB bool
	for _, c := range live {
		if MatchCommentOp(c, "same body", "op-aaa") {
			hasA = true
		}
		if MatchCommentOp(c, "same body", "op-bbb") {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Fatalf("want distinct op markers on board: %v", live)
	}
	// Substring alone must not match: "same body" without tag.
	if MatchCommentOp("same body", "same body", "op-aaa") {
		t.Fatal("untagged body must not match op-bound identity")
	}
}

// TestAdversarial_CrashBeforeRemote_StatusRecovery: in_progress, ABSENT, re-mutate.
func TestAdversarial_CrashBeforeRemote_StatusRecovery(t *testing.T) {
	var backendCalls atomic.Int32
	prev := kaneoRunCLIEnv
	t.Cleanup(func() { kaneoRunCLIEnv = prev })
	kaneoRunCLIEnv = func(ctx context.Context, extraEnv []string, name string, args ...string) (*CLIResult, error) {
		backendCalls.Add(1)
		return &CLIResult{}, nil
	}
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	mp.AddTask(testTask("cb1", "FAC-CB", "to-do"))
	kp := NewKaneoProvider("", "p", true)
	enableAtomicFence(kp)
	kp.RequireCASMeta = true
	dual := &cliBoardDual{kp: kp, mp: mp}
	kp.Receiver = NewAuthBroker(store).BindRevisionReader(dual.GetTask)
	if b, ok := kp.Receiver.(*AuthBroker); ok { b.ServerOpDedupe = true }

	_ = store.MarkAmbiguous(context.Background(), OpReceipt{
		OpID: "op-before", TaskID: "cb1", FenceToken: 1, ExpectedStatus: StatusInProgress,
	})
	_, _ = store.Advance(context.Background(), "cb1", 1)

	cas, _ := NewFencedCAS(store, dual)
	ctx := context.Background()
	rev, _ := cas.ReadRevision(ctx, "cb1")
	_, err := cas.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "cb1", rev, 1, "op-before", func(ctx context.Context) error {
		return dual.UpdateStatus(ctx, "cb1", StatusInProgress)
	})
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if backendCalls.Load() != 1 {
		t.Fatalf("backendCalls=%d", backendCalls.Load())
	}
}

// TestAdversarial_CrossTaskSameOpBinding: SQLite BEGIN IMMEDIATE binding race.
func TestAdversarial_CrossTaskSameOpBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fences.db")
	store, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- store.MarkAmbiguous(context.Background(), OpReceipt{
			OpID: "shared-op", TaskID: "task-A", FenceToken: 1,
		})
	}()
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		errs <- store.MarkAmbiguous(context.Background(), OpReceipt{
			OpID: "shared-op", TaskID: "task-B", FenceToken: 1,
		})
	}()
	wg.Wait()
	close(errs)
	var okN, errN int
	for e := range errs {
		if e == nil {
			okN++
		} else {
			errN++
		}
	}
	if okN != 1 || errN != 1 {
		t.Fatalf("want 1 ok + 1 bind error, got ok=%d err=%d", okN, errN)
	}
}

func jsonEncodeTask(w http.ResponseWriter, id, status string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"` + id + `","ref":"R","title":"t","status":"` + status + `","priority":"medium","projectId":"p","labels":[],"createdAt":"2020-01-01T00:00:00Z","updatedAt":"2020-01-01T00:00:00Z"}`))
}
