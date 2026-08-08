package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-140: a reviewer FAIL must reach the authoring worker and must never
// reach the merge gate. Observed live 2026-08-02 on FAC-121/PR #43: the
// reviewer posted four material findings and went idle, the worker sat idle
// holding nothing, and the coordinator kept offering the FAILed card up for
// approval.

func inReviewTask(ref, id string, p provider.Priority) *provider.Task {
	return &provider.Task{ID: id, Ref: ref, Status: "in-review", Priority: p,
		Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"" + ref + "\",\"task_id\":\"" + id + "\",\"edges\":[]}\n```\n"}
}

func rejection(ref, sha string) Rejection {
	return Rejection{
		Ref: ref, SHA: sha, Reviewer: "review-" + strings.ToLower(ref) + "-openai",
		Artifact: ".herd/" + strings.ToLower(ref) + "-verdict.md",
		Findings: "1. receipt.go:65 reports prompt consumption that never happened.\n" +
			"2. dispatch.go:149-160 discards outbox and compensation errors.",
	}
}

// The core inversion: an in-review card carrying a FAIL is not approvable.
func TestForgeStep_FailedCandidateRejectsInsteadOfApproving(t *testing.T) {
	e := forgeEngine(t, inReviewTask("FAC-121", "1", provider.PriorityHigh))
	r := rejection("FAC-121", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	a, err := e.ForgeStep(context.Background(), LaneState{Busy: 0, Max: 3},
		map[string]bool{}, map[string]bool{}, map[string]Rejection{"FAC-121": r})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ActionReject || a.Ref != "FAC-121" {
		t.Fatalf("a FAILed in-review card must reject, not %s %s", a.Kind, a.Ref)
	}
	if a.Rejection == nil || a.Rejection.SHA != r.SHA || a.Rejection.Findings != r.Findings {
		t.Fatalf("the action must carry the reviewer's own findings and candidate SHA, got %+v", a.Rejection)
	}
}

// A rejection on one card must not stall the merge gate for a clean one.
func TestForgeStep_CleanInReviewStillApprovesAlongsideARejection(t *testing.T) {
	e := forgeEngine(t,
		inReviewTask("FAC-121", "1", provider.PriorityUrgent),
		inReviewTask("FAC-122", "2", provider.PriorityLow),
	)
	a, err := e.ForgeStep(context.Background(), LaneState{Busy: 0, Max: 3},
		map[string]bool{}, map[string]bool{},
		map[string]Rejection{"FAC-121": rejection("FAC-121", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")})
	if err != nil {
		t.Fatal(err)
	}
	// Urgent FAC-121 outranks FAC-122 but is rejected, so the clean low-priority
	// card is the one that may be approved.
	if a.Kind != ActionApprove || a.Ref != "FAC-122" {
		t.Fatalf("want approve FAC-122, got %s %s", a.Kind, a.Ref)
	}
}

// Idempotent per (ref, SHA): the FAIL stays in the ledger until a fresh
// candidate earns a fresh PASS, so a 15s tick would otherwise re-prompt the
// worker with the same rejection forever.
func TestForgeLoop_RejectionRoutesOncePerCandidateSHA(t *testing.T) {
	e := forgeEngine(t, inReviewTask("FAC-121", "1", provider.PriorityHigh))
	r := rejection("FAC-121", "cccccccccccccccccccccccccccccccccccccccc")
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{},
		rejections: map[string]Rejection{"FAC-121": r}}

	if err := e.ForgeLoop(context.Background(), d, fastLoop(4)); err != nil {
		t.Fatal(err)
	}
	if len(d.actions) != 1 || d.actions[0] != "reject:FAC-121" {
		t.Fatalf("want exactly one rejection delivery across 4 ticks, got %v", d.actions)
	}
	if len(d.delivered) != 1 || d.delivered[0].Findings != r.Findings {
		t.Fatalf("the worker must receive the reviewer's exact findings, got %+v", d.delivered)
	}
	// And it must never have been offered to the merge gate.
	for _, a := range d.actions {
		if strings.HasPrefix(a, "approve:") {
			t.Fatalf("a FAILed card reached the merge gate: %v", d.actions)
		}
	}
}

// resharpeningDriver swaps in a rejection on a NEW candidate SHA after the
// first delivery — the shape of a worker that repaired, re-published, and got
// FAILed again.
type resharpeningDriver struct {
	fakeDriver
	next Rejection
}

func (f *resharpeningDriver) Rejections(ctx context.Context) (map[string]Rejection, error) {
	if len(f.delivered) > 0 {
		return map[string]Rejection{f.next.Ref: f.next}, nil
	}
	return f.fakeDriver.Rejections(ctx)
}

func TestForgeLoop_FreshCandidateSHARoutesAFreshRejection(t *testing.T) {
	e := forgeEngine(t, inReviewTask("FAC-121", "1", provider.PriorityHigh))
	first := rejection("FAC-121", "1111111111111111111111111111111111111111")
	second := rejection("FAC-121", "2222222222222222222222222222222222222222")
	d := &resharpeningDriver{
		fakeDriver: fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{},
			rejections: map[string]Rejection{"FAC-121": first}},
		next: second,
	}

	if err := e.ForgeLoop(context.Background(), d, fastLoop(4)); err != nil {
		t.Fatal(err)
	}
	if len(d.delivered) != 2 {
		t.Fatalf("a FAIL on a fresh SHA is a new rejection: want 2 deliveries, got %d (%v)", len(d.delivered), d.actions)
	}
	if d.delivered[0].SHA != first.SHA || d.delivered[1].SHA != second.SHA {
		t.Fatalf("deliveries must track the candidate SHA, got %s then %s", d.delivered[0].SHA, d.delivered[1].SHA)
	}
}

// A delivery that cannot be proven is a FAILED transition: it retries and it
// reaches the exit status. Recording it as routed would strand the worker
// exactly as the original defect did.
func TestForgeLoop_UnprovenRejectionDeliveryRetriesAndFailsTheRun(t *testing.T) {
	e := forgeEngine(t, inReviewTask("FAC-121", "1", provider.PriorityHigh))
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{},
		rejections: map[string]Rejection{"FAC-121": rejection("FAC-121", "dddddddddddddddddddddddddddddddddddddddd")},
		rejectErr: func(string) error {
			return errors.New("agent \"task-fac-121\" never confirmed prompt-correlated consumption")
		}}

	err := e.ForgeLoop(context.Background(), d, fastLoop(3))
	if err == nil {
		t.Fatal("an undeliverable rejection exited 0")
	}
	if !strings.Contains(err.Error(), "reject FAC-121 failed") {
		t.Fatalf("exit error must name the failed transition, got %v", err)
	}
	if len(d.actions) != 3 {
		t.Fatalf("an unproven delivery must be retried every tick, got %v", d.actions)
	}
	if len(d.delivered) != 0 {
		t.Fatalf("an unproven delivery must not count as routed: %+v", d.delivered)
	}
}

// Unreadable review state is UNKNOWN, not "nothing was rejected" — the second
// reading re-arms the merge gate on a FAILed candidate.
func TestForgeLoop_UnknownRejectionStateBlocksApprove(t *testing.T) {
	e := forgeEngine(t, inReviewTask("FAC-121", "1", provider.PriorityHigh))
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{},
		rejectionErr: errors.New("read review ledger: permission denied")}

	err := e.ForgeLoop(context.Background(), d, fastLoop(1))
	if err == nil {
		t.Fatal("unreadable review state exited 0")
	}
	if !strings.Contains(err.Error(), "review_state_unknown") {
		t.Fatalf("want a review_state_unknown transition, got %v", err)
	}
	if len(d.actions) != 0 {
		t.Fatalf("acted on unknown review state: %v", d.actions)
	}
}

// A rejected card is not a drained board: --stop-empty must not exit while a
// worker still owes a repair.
func TestForgeLoop_RejectedCardIsNotADrainedBoard(t *testing.T) {
	e := forgeEngine(t, inReviewTask("FAC-121", "1", provider.PriorityHigh))
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{},
		rejections: map[string]Rejection{"FAC-121": rejection("FAC-121", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")}}

	done := make(chan error, 1)
	go func() {
		done <- e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, StopEmpty: true, MaxTicks: 5})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("routing a rejection is not a failed run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop hung on a rejected card")
	}
	if len(d.actions) != 1 || d.actions[0] != "reject:FAC-121" {
		t.Fatalf("want one delivery and no board move, got %v", d.actions)
	}
	if log := strings.Join(d.logged, "\n"); strings.Contains(log, "board clear") {
		t.Fatalf("the loop claimed a drained board while FAC-121 awaited repair: %v", d.logged)
	}
}
