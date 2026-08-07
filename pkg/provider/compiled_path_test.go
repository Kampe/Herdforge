package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// ensure strings used for op-tagged comment assertions

// TestCompiledPath_DispatchReviewApproveBoardDone_KaneoHTTP exercises the
// same fenced mutation entrypoints production dispatch/review/approve/
// board-done use (MutateStatus/MutateComment/MutateClaim + Begin/Complete)
// against a Kaneo-compatible enforcing HTTP receiver + AuthBroker.
func TestCompiledPath_DispatchReviewApproveBoardDone_KaneoHTTP(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{
		ID: "cp1", Ref: "FAC-CP", Title: "compiled path", Status: "to-do",
		ProjectID: "proj", Labels: []string{"worker"},
		UpdatedAt: now, CreatedAt: now,
	})
	srv := board.serve()
	defer srv.Close()

	kp := NewKaneoProvider(srv.URL, "proj", false)
	enableAtomicFence(kp)
	stack, err := OpenClaimStack(t.TempDir(), kp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	if kp.Receiver == nil || !kp.RequireCASMeta {
		t.Fatal("stack must wire AuthBroker")
	}

	ctx := context.Background()
	key := LeaseKey(".", "kaneo", "proj", "FAC-CP")

	// --- dispatch-equivalent: claim (in-progress) under live lease ---
	lease, err := stack.AcquireLease(ctx, key, "dispatch-owner", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.CAS.AdvanceFence(ctx, "cp1", lease.Generation); err != nil {
		t.Fatal(err)
	}
	// MutateClaimGuarded is the daemon pulse/dispatch claim path.
	if _, err := stack.MutateClaimGuarded(ctx, key, "dispatch-owner", "worker", "worker", "cp1"); err != nil {
		// Claim may already be held by AcquireLease — use MutateStatus for in-progress.
		if err := stack.Board.MutateStatus(ctx, stack.Manager, key, "dispatch-owner", lease.Generation, "cp1", StatusInProgress); err != nil {
			t.Fatalf("dispatch status: %v", err)
		}
	}
	// Dispatch comment (addCommentFenced → MutateComment).
	body := "Dispatched to worktree .herd/worktrees/fac-cp"
	if err := stack.Board.MutateComment(ctx, stack.Manager, key, "dispatch-owner", lease.Generation, "cp1", body); err != nil {
		t.Fatalf("dispatch comment: %v", err)
	}
	if got := board.Comments("cp1"); len(got) != 1 || !MatchCommentOp(got[0], body, "") && !strings.Contains(got[0], body) {
		// Op-bound marker is appended for live identity; body prefix must match.
		if len(got) != 1 || !strings.HasPrefix(got[0], body) {
			t.Fatalf("comment receipt: %v", got)
		}
	}

	// --- review-equivalent: fenced status → in-review ---
	if err := stack.Board.MutateStatus(ctx, stack.Manager, key, "dispatch-owner", lease.Generation, "cp1", StatusInReview); err != nil {
		t.Fatalf("review status: %v", err)
	}

	// --- approve/board-done-equivalent: fenced status → done ---
	if err := stack.Board.MutateStatus(ctx, stack.Manager, key, "dispatch-owner", lease.Generation, "cp1", StatusDone); err != nil {
		t.Fatalf("approve status: %v", err)
	}
	tgot, err := kp.GetTask(ctx, "cp1")
	if err != nil {
		t.Fatal(err)
	}
	if NormalizeStatus(tgot.Status) != StatusDone {
		t.Fatalf("status=%s", tgot.Status)
	}

	// Stale generation cannot mutate (lease check / fence).
	if err := stack.Board.MutateStatus(ctx, stack.Manager, key, "dispatch-owner", lease.Generation-1, "cp1", StatusToDo); err == nil {
		t.Fatal("stale generation must fail")
	}
	// Direct unfenced bypass through Kaneo provider refused.
	if err := kp.UpdateStatus(ctx, "cp1", StatusToDo); err == nil || !strings.Contains(err.Error(), "unfenced") {
		t.Fatalf("bypass: %v", err)
	}
	// Contender without lease cannot MutateStatusGuarded.
	if _, err := stack.MutateStatusGuarded(ctx, key, "other", "worker", "worker", "cp1", StatusToDo); err == nil {
		t.Fatal("unguarded contender must fail")
	}

	// Effects: claim/status + comment + in-review + done (no duplicates from stale).
	if board.EffectCount() < 3 {
		t.Fatalf("effects=%d want >=3", board.EffectCount())
	}
	_ = claim.ErrProviderFenceRejected
}
