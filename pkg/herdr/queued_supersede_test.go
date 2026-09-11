package herdr

import (
	"context"
	"strings"
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
	binding := TargetBinding(liveAgent())
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

// TestSupersedeAndQueueRoutineRefusesAnUnidentifiableTarget is the guard at
// the resolution boundary. A pane generation alone is not an agent: the same
// terminal can host a restarted harness that has reported no session yet, so
// EITHER identity component missing means nothing may be retired.
func TestSupersedeAndQueueRoutineRefusesAnUnidentifiableTarget(t *testing.T) {
	for name, blank := range map[string]func(*interruptRecorder){
		"no terminal generation": func(r *interruptRecorder) { r.terminal = "" },
		"harness session absent": func(r *interruptRecorder) { r.session = "" },
	} {
		t.Run(name, func(t *testing.T) {
			rec, box := installBusyWorker(t, "working")
			blank(rec)
			t.Setenv("HERD_LANE", "forge-orchestrator-1")
			ctx := context.Background()
			if _, err := box.QueueRoutine(ctx, "forge-orchestrator-1", "worker", "tb-some-old-session", "stale work"); err != nil {
				t.Fatalf("seed: %v", err)
			}

			if _, err := SupersedeAndQueueRoutine(ctx, "worker", "", "replacement"); err == nil {
				t.Fatal("an unidentifiable target must refuse supersession, not retire on a partial identity")
			}
			pending, err := box.PendingQueued("worker")
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].Body != "stale work" {
				t.Fatalf("a refused supersession must change nothing: %+v", pending)
			}
		})
	}
}

// TestSameTerminalNewHarnessSessionCannotRetirePreviousWork is the exact case
// root called out: the pane never changed, but the harness inside it
// restarted. Same terminal generation, different agent, none of the old work.
func TestSameTerminalNewHarnessSessionCannotRetirePreviousWork(t *testing.T) {
	rec, box := installBusyWorker(t, "working")
	t.Setenv("HERD_LANE", "forge-orchestrator-1")
	if _, err := Send("worker", "work owned by the previous harness session", false, 0); err != nil {
		t.Fatalf("busy send: %v", err)
	}

	// The pane is untouched; only the harness restarted.
	rec.session = "ses_restarted_harness"
	result, err := SupersedeAndQueueRoutine(context.Background(), "worker", "", "current correction")
	if err != nil {
		t.Fatalf("supersede from the restarted harness: %v", err)
	}
	if len(result.SupersededIDs) != 0 {
		t.Fatalf("a restarted harness in the same terminal retired the previous session's work: %v", result.SupersededIDs)
	}
	after, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("pending = %d, want the preserved envelope plus the new correction", len(after))
	}
}

// TestSupersedeRequiresABoundCoordinatorIssuer closes the fallback hole: an
// invocation with no lane identity queues as the shared anonymous sender,
// which every unbound coordinator also uses, so it can never authorize
// retiring queued work — including work queued anonymously.
func TestSupersedeRequiresABoundCoordinatorIssuer(t *testing.T) {
	_, box := installBusyWorker(t, "working")
	t.Setenv("HERD_LANE", "")
	ctx := context.Background()

	// Ordinary FIFO send stays compatible while unbound.
	if _, err := Send("worker", "anonymous legacy assignment", false, 0); err != nil {
		t.Fatalf("anonymous FIFO send must keep working: %v", err)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil || len(pending) != 1 {
		t.Fatalf("seed: %d pending, err %v", len(pending), err)
	}
	if pending[0].Sender != "herd-send" {
		t.Fatalf("unbound send must queue as the shared anonymous sender, got %q", pending[0].Sender)
	}

	_, err = SupersedeAndQueueRoutine(ctx, "worker", "", "replacement")
	if err == nil {
		t.Fatal("an unbound invocation must refuse supersession; the fallback sender is not an issuer identity")
	}
	// The refusal must come from the COMMAND boundary, where the invocation's
	// own identity is known, not only from the mailbox's last-ditch rejection
	// of the shared sender. A caller that silently substitutes the anonymous
	// default here would still be stopped, but only by accident of depth.
	if !strings.Contains(err.Error(), "no bound coordinator identity") {
		t.Fatalf("expected the bound-issuer refusal at the command boundary, got: %v", err)
	}
	after, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Body != "anonymous legacy assignment" {
		t.Fatalf("anonymous legacy pending must be preserved untouched: %+v", after)
	}

	// And a bound coordinator cannot reach across into that anonymous work.
	t.Setenv("HERD_LANE", "forge-orchestrator-1")
	result, err := SupersedeAndQueueRoutine(ctx, "worker", "", "bound replacement")
	if err != nil {
		t.Fatalf("bound supersede: %v", err)
	}
	if len(result.SupersededIDs) != 0 {
		t.Fatalf("a bound issuer retired anonymous work it does not own: %v", result.SupersededIDs)
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
