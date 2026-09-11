package herdr

import (
	"context"
	"testing"
)

// TestSupersedeAndQueueRoutineResolvesLiveTargetAndRetiresSameIssuer proves
// the retasking seam end to end against the live-fleet resolution seam: only
// the exact live target's pending mail from the same issuer is retired, the
// replacement queues once, and nothing is written to the pane.
func TestSupersedeAndQueueRoutineResolvesLiveTargetAndRetiresSameIssuer(t *testing.T) {
	recorder, box := installBusyWorker(t, "working")
	t.Setenv("HERD_LANE", "forge-orchestrator-1")
	ctx := context.Background()
	binding := TargetBinding(AgentEntry{Name: "worker", Workspace: "wK", TerminalID: "term_live_1"})
	if binding == "" {
		t.Fatal("a live row with a terminal id must produce a binding")
	}
	if _, err := box.QueueRoutine(ctx, "forge-orchestrator-1", "worker", binding, "stale review PR820"); err != nil {
		t.Fatalf("seed stale queue: %v", err)
	}
	if _, err := box.QueueRoutine(ctx, "forge-orchestrator-1", "worker", binding, "stale harvest 2753"); err != nil {
		t.Fatalf("seed stale queue: %v", err)
	}
	if _, err := box.QueueRoutine(ctx, "forge-orchestrator-OTHER", "worker", binding, "foreign queued work"); err != nil {
		t.Fatalf("seed foreign queue: %v", err)
	}

	result, err := SupersedeAndQueueRoutine(ctx, "worker", "", "retask: current harvest task 2849")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(result.SupersededIDs) != 2 {
		t.Fatalf("superseded = %v, want only the two same-issuer stale prompts", result.SupersededIDs)
	}
	if _, prompted, keys, signals := recorder.snapshot(); prompted != 0 || len(keys) != 0 || signals != 0 {
		t.Fatalf("supersession touched the pane: prompted=%d keys=%v signals=%d", prompted, keys, signals)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want replacement + the foreign issuer's envelope", len(pending))
	}
	found := map[string]bool{}
	for _, env := range pending {
		found[env.ID] = true
	}
	if !found[result.EnvelopeID] {
		t.Fatalf("replacement %s missing from pending", result.EnvelopeID)
	}

	// A second attempt converges instead of double-queueing.
	again, err := SupersedeAndQueueRoutine(ctx, "worker", "", "retask: current harvest task 2849")
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if !again.Idempotent || again.EnvelopeID != result.EnvelopeID {
		t.Fatalf("re-run = %+v, want idempotent convergence on %s", again, result.EnvelopeID)
	}
}

// TestSupersedeAndQueueRoutineQueuesReplacementWithNothingPending pins the
// no-discard rule at the command boundary: the operator supplied a real
// assignment, so an empty stale queue still delivers it.
func TestSupersedeAndQueueRoutineQueuesReplacementWithNothingPending(t *testing.T) {
	_, box := installBusyWorker(t, "working")
	t.Setenv("HERD_LANE", "forge-orchestrator-1")
	result, err := SupersedeAndQueueRoutine(context.Background(), "worker", "", "replacement")
	if err != nil {
		t.Fatalf("supersede with nothing pending: %v", err)
	}
	if len(result.SupersededIDs) != 0 || result.EnvelopeID == "" {
		t.Fatalf("result = %+v, want zero victims and a queued replacement", result)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "replacement" {
		t.Fatalf("pending = %+v, want the operator's assignment queued, never discarded", pending)
	}
}

// TestSupersedeAndQueueRoutineRefusesAnUnidentifiableTarget is the reboot
// guard at the resolution boundary: without a terminal generation there is no
// proof of WHICH session answers to this name, so nothing may be retired.
func TestSupersedeAndQueueRoutineRefusesAnUnidentifiableTarget(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	rec.terminal = ""
	t.Setenv("HERD_LANE", "forge-orchestrator-1")
	ctx := context.Background()
	if _, err := box.QueueRoutine(ctx, "forge-orchestrator-1", "worker", "tb-some-old-session", "stale work"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := SupersedeAndQueueRoutine(ctx, "worker", "", "replacement"); err == nil {
		t.Fatal("an unidentifiable target must refuse supersession, not retire on a name")
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "stale work" {
		t.Fatalf("a refused supersession must change nothing: %+v", pending)
	}
}

// TestBusyQueueBindsTheLiveSessionSoARelaunchCannotRetireIt is the incident in
// miniature: the busy-send path binds the session it queued to, and the same
// lane relaunched under a new terminal generation cannot retire that work.
func TestBusyQueueBindsTheLiveSessionSoARelaunchCannotRetireIt(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	t.Setenv("HERD_LANE", "forge-orchestrator-1")
	if _, err := Send("worker", "work for the pre-reboot session", false, 0); err != nil {
		t.Fatalf("busy send: %v", err)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil || len(pending) != 1 {
		t.Fatalf("seed: %d pending, err %v", len(pending), err)
	}
	if pending[0].Binding == "" {
		t.Fatal("a queued payload must record the live target it was addressed to")
	}

	// The host reboots; the lane comes back under the same name with a new
	// terminal generation.
	rec.terminal = "term_live_2_after_reboot"
	result, err := SupersedeAndQueueRoutine(context.Background(), "worker", "", "current correction")
	if err != nil {
		t.Fatalf("supersede from the new session: %v", err)
	}
	if len(result.SupersededIDs) != 0 {
		t.Fatalf("a relaunched session retired the previous session's work: %v", result.SupersededIDs)
	}
	after, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("pending = %d, want the preserved old-session envelope plus the new correction", len(after))
	}
}
