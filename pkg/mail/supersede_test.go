package mail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	liveBinding   = "tb-live-session"
	oldBinding    = "tb-rebooted-old-session"
	supersedeBody = "retask: current harvest task 2849"
)

func supersedeFixture(t *testing.T) (*Mailbox, string, string) {
	t.Helper()
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	ctx := context.Background()
	for _, body := range []string{"review PR820", "harvest task 2753"} {
		if _, err := box.QueueRoutine(ctx, "forge-orchestrator-1", "worker-p1BB", liveBinding, body); err != nil {
			t.Fatalf("seed queued envelope: %v", err)
		}
	}
	return box, "forge-orchestrator-1", "worker-p1BB"
}

func TestSupersedeRetiresSameIssuerPendingAndQueuesReplacementOnce(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)
	ctx := context.Background()

	out, err := box.SupersedePendingRoutine(ctx, sender, recipient, liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(out.SupersededIDs) != 2 || out.Idempotent {
		t.Fatalf("outcome = %+v, want both stale envelopes superseded once", out)
	}

	pending, err := box.PendingQueued(recipient)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != out.EnvelopeID {
		t.Fatalf("pending = %d envelopes, want exactly the replacement %s", len(pending), out.EnvelopeID)
	}
	if pending[0].Body != supersedeBody {
		t.Fatalf("replacement body drifted: %q", pending[0].Body)
	}
	if pending[0].Binding != liveBinding {
		t.Fatalf("replacement lost its target binding: %q", pending[0].Binding)
	}

	// The disposition and its reason are durable history; the stale envelope
	// lines themselves must still be present (append-only evidence).
	st, err := loadAck(box.MailFile)
	if err != nil {
		t.Fatalf("ack state: %v", err)
	}
	if len(st.Superseded[recipient]) != 2 {
		t.Fatalf("supersession records = %d, want 2", len(st.Superseded[recipient]))
	}
	for _, id := range out.SupersededIDs {
		rec := st.Superseded[recipient][id]
		if rec.Reason != SupersedePendingReason || rec.ReplacementID != out.EnvelopeID || rec.Issuer != sender {
			t.Fatalf("record for %s = %+v", id, rec)
		}
	}
	raw, err := os.ReadFile(box.MailFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "review PR820") != 1 {
		t.Fatal("the stale envelope line was deleted; history must be preserved")
	}

	// Idempotent re-run: the same supersession cannot duplicate the
	// replacement or re-supersede consumed work.
	again, err := box.SupersedePendingRoutine(ctx, sender, recipient, liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if !again.Idempotent || len(again.SupersededIDs) != 0 || again.EnvelopeID != out.EnvelopeID {
		t.Fatalf("re-run outcome = %+v, want idempotent convergence", again)
	}
	pending, err = box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending after re-run = %d, want exactly the one replacement", len(pending))
	}
}

func TestSupersedeLeavesForeignIssuerAndNonQueueMailAlone(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)
	ctx := context.Background()
	if _, err := box.QueueRoutine(ctx, "forge-orchestrator-OTHER", recipient, liveBinding, "another coordinator's queued work"); err != nil {
		t.Fatalf("seed foreign queued: %v", err)
	}
	ctrl := &Envelope{ID: "ctl-1", Sender: sender, Recipient: recipient, Subject: ControlSubjectPrefix + " stop worker-p1BB", Body: "urgent stop", Timestamp: time.Now().UTC()}
	if err := box.AppendEnvelopeContext(ctx, ctrl); err != nil {
		t.Fatalf("seed control: %v", err)
	}
	ordinary := &Envelope{ID: "ord-1", Sender: sender, Recipient: recipient, Subject: "review feedback", Body: "ordinary report", Timestamp: time.Now().UTC()}
	if err := box.AppendEnvelopeContext(ctx, ordinary); err != nil {
		t.Fatalf("seed ordinary: %v", err)
	}

	out, err := box.SupersedePendingRoutine(ctx, sender, recipient, liveBinding, "replacement", "")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(out.SupersededIDs) != 2 {
		t.Fatalf("superseded = %v, want only the two same-issuer queued envelopes", out.SupersededIDs)
	}
	pending, err := box.PendingRoutine(recipient)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, env := range pending {
		seen[env.ID] = true
	}
	if seen["ctl-1"] {
		t.Fatal("authenticated control mail must keep its native consumer; it must never surface on the routine drain")
	}
	if !seen["ord-1"] {
		t.Fatal("ordinary report mail is routine-drain-eligible by design and must be untouched by supersession")
	}
	handled, err := box.Handled(recipient, "ord-1")
	if err != nil || handled {
		t.Fatalf("ordinary mail must not be retired by supersession: handled=%v err=%v", handled, err)
	}
	if !seen[out.EnvelopeID] {
		t.Fatalf("replacement %s must be the eligible pending payload", out.EnvelopeID)
	}
	foreignPending, err := box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	foreignAlive := false
	for _, env := range foreignPending {
		if env.Sender == "forge-orchestrator-OTHER" {
			foreignAlive = true
		}
	}
	if !foreignAlive {
		t.Fatal("a foreign issuer's queued work was retired; supersession must be same-issuer only")
	}
}

// TestSupersedeCannotRetireAnotherSessionOrUnboundWork is the live incident:
// a host reboots, the lane comes back under the SAME NAME, and the new
// session must not be able to retire the old session's queued work. Legacy
// envelopes carrying no binding at all are equally ambiguous and equally
// protected.
func TestSupersedeCannotRetireAnotherSessionOrUnboundWork(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	ctx := context.Background()
	sender, recipient := "forge-orchestrator-1", "worker-p1BB"

	if _, err := box.QueueRoutine(ctx, sender, recipient, oldBinding, "work owned by the pre-reboot session"); err != nil {
		t.Fatalf("seed old session: %v", err)
	}
	if _, err := box.QueueRoutine(ctx, sender, recipient, "", "legacy unbound work"); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	out, err := box.SupersedePendingRoutine(ctx, sender, recipient, liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(out.SupersededIDs) != 0 {
		t.Fatalf("superseded %v; a different session and an unbound envelope must both be preserved", out.SupersededIDs)
	}
	pending, err := box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want both preserved envelopes plus the new replacement", len(pending))
	}

	// And the identity is not merely advisory: an unresolvable binding is a
	// hard refusal, never a name-only retirement.
	if _, err := box.SupersedePendingRoutine(ctx, sender, recipient, "", supersedeBody, ""); err == nil {
		t.Fatal("an unbound supersession must fail; a recipient name is not proof of a session")
	}
}

// TestSupersedeCrashBetweenAppendAndCommitKeepsOldWorkEligible is the crash
// contract. The replacement is durable first and the disposition commits
// second, so an interruption in between leaves every old envelope
// deliverable, and re-running converges on exactly one replacement.
func TestSupersedeCrashBetweenAppendAndCommitKeepsOldWorkEligible(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)

	// Inject the crash precisely at the disposition commit: the replacement
	// append has already happened durably when this fires.
	orig := writeFileAtomicFn
	writeFileAtomicFn = func(path string, data []byte, perm os.FileMode) error {
		if strings.HasSuffix(path, ".handled.json") {
			return os.ErrPermission
		}
		return orig(path, data, perm)
	}
	_, err := box.SupersedePendingRoutine(context.Background(), sender, recipient, liveBinding, supersedeBody, "")
	writeFileAtomicFn = orig
	if err == nil {
		t.Fatal("a disposition that did not persist must return nonzero")
	}

	pending, err := box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want both old envelopes still eligible plus the durable replacement; nothing may be lost", len(pending))
	}
	st, err := loadAck(box.MailFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Superseded[recipient]) != 0 || len(st.Handled[recipient]) != 0 {
		t.Fatalf("an uncommitted supersession left a disposition behind: %+v", st)
	}

	// Recovery is re-running the identical command, not manual surgery.
	out, err := box.SupersedePendingRoutine(context.Background(), sender, recipient, liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("re-run after crash: %v", err)
	}
	if len(out.SupersededIDs) != 2 || !out.Idempotent {
		t.Fatalf("re-run = %+v, want the two stale envelopes retired against the already-durable replacement", out)
	}
	pending, err = box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != out.EnvelopeID {
		t.Fatalf("after recovery pending = %d, want exactly one replacement", len(pending))
	}
}

// TestReaderIgnoresSupersessionWhoseReplacementIsMissing proves the
// ineligibility rule is enforced where it is READ, not merely argued from
// write ordering: a disposition naming a replacement that is not in the
// mailbox describes a supersession that never completed, and the old work
// stays deliverable.
func TestReaderIgnoresSupersessionWhoseReplacementIsMissing(t *testing.T) {
	box, _, recipient := supersedeFixture(t)
	pending, err := box.PendingQueued(recipient)
	if err != nil || len(pending) != 2 {
		t.Fatalf("fixture: %d pending, err %v", len(pending), err)
	}
	victim := pending[0].ID

	st, err := loadAck(box.MailFile)
	if err != nil {
		t.Fatal(err)
	}
	applySupersedeMarks(st, recipient, []string{victim}, "forge-orchestrator-1", "queued-never-written", "torn")
	if err := saveAck(box.MailFile, st); err != nil {
		t.Fatal(err)
	}

	after, err := box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("pending = %d, want the victim still eligible: its replacement never landed", len(after))
	}

	// An ordinary acknowledgement carries no supersession record and must
	// still retire its envelope normally.
	if err := box.MarkHandled(recipient, victim); err != nil {
		t.Fatal(err)
	}
	delete(st.Superseded[recipient], victim)
	if err := saveAck(box.MailFile, st); err != nil {
		t.Fatal(err)
	}
	after, err = box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("pending = %d, want a plain acknowledgement to still retire its envelope", len(after))
	}
}

func TestSupersedeFailurePersistsNothingAndKeepsOldWorkEligible(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)

	// Empty payload: a clear pre-mutation refusal.
	if _, err := box.SupersedePendingRoutine(context.Background(), sender, recipient, liveBinding, "   ", ""); err == nil {
		t.Fatal("empty replacement payload must fail clearly")
	}
	if pending, _ := box.PendingQueued(recipient); len(pending) != 2 {
		t.Fatalf("empty-payload refusal disturbed the queue: %d pending", len(pending))
	}

	// Replacement-append failure: nothing is retired, because nothing is
	// retired until the replacement is durable.
	if err := os.Chmod(box.MailFile, 0o444); err != nil {
		t.Fatalf("make mailbox read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(box.MailFile, 0o644) })
	if _, err := box.SupersedePendingRoutine(context.Background(), sender, recipient, liveBinding, "replacement", ""); err == nil {
		t.Fatal("failed replacement persistence must return nonzero")
	}
	if err := os.Chmod(box.MailFile, 0o644); err != nil {
		t.Fatal(err)
	}
	pending, err := box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("old work must survive a failed supersession eligible: %d pending", len(pending))
	}
	st, err := loadAck(box.MailFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Superseded[recipient]) != 0 || len(st.Handled[recipient]) != 0 {
		t.Fatalf("a failed supersession left a durable disposition behind: %+v", st)
	}
}

// TestSupersedeRefusesRepeatOfAnAlreadyConsumedPayload closes the silent-drop
// hole: the queued identity is stable, so re-sending a body that was already
// delivered and acknowledged cannot append anything. Reporting success there
// would lose a genuinely new assignment, so it is an explicit failure.
func TestSupersedeRefusesRepeatOfAnAlreadyConsumedPayload(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)
	ctx := context.Background()

	out, err := box.SupersedePendingRoutine(ctx, sender, recipient, liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if err := box.MarkHandled(recipient, out.EnvelopeID); err != nil {
		t.Fatalf("consume replacement: %v", err)
	}

	_, err = box.SupersedePendingRoutine(ctx, sender, recipient, liveBinding, supersedeBody, "")
	if err == nil {
		t.Fatal("repeating an already-consumed payload must fail loudly, not report a phantom success")
	}
	if !strings.Contains(err.Error(), "already delivered") {
		t.Fatalf("error must name the cause, got: %v", err)
	}
}

// TestSupersedeWithNothingPendingStillQueuesTheReplacement pins the rule that
// a supplied assignment is never discarded: the operator asked for this
// payload to be delivered, and "nothing stale was pending" is not a reason to
// throw it away.
func TestSupersedeWithNothingPendingStillQueuesTheReplacement(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	out, err := box.SupersedePendingRoutine(context.Background(), "forge-orchestrator-1", "worker-p1BB", liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("supersede on an empty queue: %v", err)
	}
	if len(out.SupersededIDs) != 0 || out.EnvelopeID == "" {
		t.Fatalf("outcome = %+v, want zero victims and a queued replacement", out)
	}
	pending, err := box.PendingQueued("worker-p1BB")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != supersedeBody {
		t.Fatalf("pending = %+v, want the operator's real assignment queued", pending)
	}
}

// TestSupersedeRefusesAPartiallyUnreadableQueue keeps the selection fail-closed:
// a mailbox it cannot fully parse must not be selectively retired.
func TestSupersedeRefusesAPartiallyUnreadableQueue(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)
	f, err := os.OpenFile(box.MailFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := box.SupersedePendingRoutine(context.Background(), sender, recipient, liveBinding, supersedeBody, ""); err == nil {
		t.Fatal("a partially unreadable queue must refuse retirement, not guess")
	}
	st, err := loadAck(box.MailFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Superseded[recipient]) != 0 {
		t.Fatalf("refused supersession still recorded dispositions: %v", st.Superseded[recipient])
	}
}

// TestConcurrentAcknowledgementSurvivesSupersession covers the shared-state
// hazard: a second Mailbox instance acknowledging one envelope must not have
// its mark erased by a supersession running against the same file, and vice
// versa. Both writers take the same cross-process data flock.
func TestConcurrentAcknowledgementSurvivesSupersession(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)
	ctx := context.Background()
	other := NewMailbox(box.MailFile) // a separate instance, as another process would have

	ordinary := &Envelope{ID: "ord-concurrent", Sender: sender, Recipient: recipient, Subject: "review feedback", Body: "report", Timestamp: time.Now().UTC()}
	if err := box.AppendEnvelopeContext(ctx, ordinary); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- other.MarkHandled(recipient, "ord-concurrent") }()
	out, err := box.SupersedePendingRoutine(ctx, sender, recipient, liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("concurrent acknowledgement: %v", err)
	}

	handled, err := other.Handled(recipient, "ord-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("the concurrent acknowledgement was clobbered; disposition writers must serialize on the shared lock")
	}
	st, err := loadAck(box.MailFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Superseded[recipient]) != len(out.SupersededIDs) {
		t.Fatalf("supersession records = %d, want %d", len(st.Superseded[recipient]), len(out.SupersededIDs))
	}
}

// TestMarkHandledTakesTheSharedMailboxLock is the synchronization guard.
// Acknowledgement is a read-modify-write of state shared with every other
// process holding this mailbox; under the per-instance mutex alone, two
// processes could each load the same state and one mark would silently
// vanish, re-delivering settled work. Proven at the mailbox critical-section
// seam so it cannot pass by scheduling luck.
func TestMarkHandledTakesTheSharedMailboxLock(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	if _, err := box.QueueRoutine(context.Background(), "issuer", "worker", liveBinding, "payload"); err != nil {
		t.Fatal(err)
	}

	entered := 0
	rec := func(int64) { entered++ }
	recordCSTicket.Store(&rec)
	defer recordCSTicket.Store(nil)

	before := entered
	if err := box.MarkHandled("worker", "some-envelope"); err != nil {
		t.Fatal(err)
	}
	if entered == before {
		t.Fatal("MarkHandled never entered the shared mailbox critical section; concurrent acknowledgements from another process can clobber each other")
	}
}

// TestDispositionStateIsFsyncedNotJustRenamed keeps the durability honest: an
// atomic-looking rename whose data was never flushed does not survive the
// crash it exists to survive.
func TestDispositionStateIsFsyncedNotJustRenamed(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))

	syncs := 0
	origFile := fileSyncFn
	fileSyncFn = func(f *os.File) error {
		syncs++
		return origFile(f)
	}
	dirSyncs := 0
	origDir := syncDirFn
	syncDirFn = func(path string) error {
		if strings.Contains(path, ".handled.json") {
			dirSyncs++
		}
		return origDir(path)
	}
	defer func() { fileSyncFn, syncDirFn = origFile, origDir }()

	before, beforeDir := syncs, dirSyncs
	if err := box.MarkHandled("worker", "env-1"); err != nil {
		t.Fatal(err)
	}
	if syncs == before {
		t.Fatal("the disposition file was renamed into place without ever being fsynced")
	}
	if dirSyncs == beforeDir {
		t.Fatal("the disposition rename was not followed by a directory fsync; the new directory entry can be lost")
	}
}

// TestSupersedeRefusesTheSharedAnonymousIssuer closes the fallback hole at the
// mail layer too: unbound coordinators all queue as the same default sender,
// so equality on it proves nothing and mail queued under it is permanently
// preserved.
func TestSupersedeRefusesTheSharedAnonymousIssuer(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	ctx := context.Background()
	if _, err := box.QueueRoutine(ctx, AnonymousIssuer, "worker-p1BB", liveBinding, "anonymous legacy work"); err != nil {
		t.Fatal(err)
	}

	if _, err := box.SupersedePendingRoutine(ctx, AnonymousIssuer, "worker-p1BB", liveBinding, supersedeBody, ""); err == nil {
		t.Fatal("the shared unbound sender must never authorize a supersession")
	}
	pending, err := box.PendingQueued("worker-p1BB")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "anonymous legacy work" {
		t.Fatalf("anonymous pending must survive untouched: %+v", pending)
	}

	// A bound issuer cannot reach into it either.
	out, err := box.SupersedePendingRoutine(ctx, "forge-orchestrator-1", "worker-p1BB", liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("bound supersede: %v", err)
	}
	if len(out.SupersededIDs) != 0 {
		t.Fatalf("a bound issuer retired anonymous work: %v", out.SupersededIDs)
	}
}

// TestReaderGateValidatesTheReplacementIdentityNotJustTheID: a present
// envelope whose id happens to match the record is not a committed
// supersession. It must be the same queued class, recipient, issuer and target
// binding, or the retired work stays deliverable.
func TestReaderGateValidatesTheReplacementIdentityNotJustTheID(t *testing.T) {
	sender, recipient := "forge-orchestrator-1", "worker-p1BB"
	victim := &Envelope{ID: "queued-victim", Sender: sender, Recipient: recipient, Subject: QueuedDeliverySubject, Binding: liveBinding, Body: "stale"}
	rec := SupersessionRecord{ReplacementID: "queued-repl", Issuer: sender, Reason: "r"}
	good := &Envelope{ID: "queued-repl", Sender: sender, Recipient: recipient, Subject: QueuedDeliverySubject, Binding: liveBinding, Body: "fresh"}

	if !supersessionHonoured(rec, victim, good) {
		t.Fatal("a fully matching replacement must commit the supersession")
	}
	for name, bad := range map[string]*Envelope{
		"missing entirely":    nil,
		"ordinary report":     {ID: "queued-repl", Sender: sender, Recipient: recipient, Subject: "review feedback", Binding: liveBinding},
		"control envelope":    {ID: "queued-repl", Sender: sender, Recipient: recipient, Subject: ControlSubjectPrefix + " stop", Binding: liveBinding},
		"different recipient": {ID: "queued-repl", Sender: sender, Recipient: "someone-else", Subject: QueuedDeliverySubject, Binding: liveBinding},
		"different issuer":    {ID: "queued-repl", Sender: "forge-orchestrator-OTHER", Recipient: recipient, Subject: QueuedDeliverySubject, Binding: liveBinding},
		"different session":   {ID: "queued-repl", Sender: sender, Recipient: recipient, Subject: QueuedDeliverySubject, Binding: oldBinding},
		"unbound replacement": {ID: "queued-repl", Sender: sender, Recipient: recipient, Subject: QueuedDeliverySubject, Binding: ""},
		"identity mismatch":   {ID: "queued-something-else", Sender: sender, Recipient: recipient, Subject: QueuedDeliverySubject, Binding: liveBinding},
	} {
		if supersessionHonoured(rec, victim, bad) {
			t.Fatalf("%s was accepted as a committed replacement", name)
		}
	}
}

// TestEveryDispositionReaderHonoursTheGate proves the rule lives at the shared
// chokepoint: Handled itself reports an uncommitted supersession as still
// pending, so every caller inherits it rather than each re-implementing it.
func TestEveryDispositionReaderHonoursTheGate(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)
	pending, err := box.PendingQueued(recipient)
	if err != nil || len(pending) != 2 {
		t.Fatalf("fixture: %d pending, err %v", len(pending), err)
	}
	victim := pending[0].ID

	st, err := loadAck(box.MailFile)
	if err != nil {
		t.Fatal(err)
	}
	applySupersedeMarks(st, recipient, []string{victim}, sender, "queued-never-written", "torn")
	if err := saveAck(box.MailFile, st); err != nil {
		t.Fatal(err)
	}

	handled, err := box.Handled(recipient, victim)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("Handled honoured a supersession whose replacement never landed; every read path that asks it would skip deliverable work")
	}
	after, err := box.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("listing = %d, want the victim still eligible", len(after))
	}
}

// TestInterruptedSupersessionSurvivesRestart: the recovery guarantee has to
// hold for a NEW process reading the same files, not just the instance that
// was interrupted. No in-memory state, no stale saved state to restore.
func TestInterruptedSupersessionSurvivesRestart(t *testing.T) {
	box, sender, recipient := supersedeFixture(t)

	orig := writeFileAtomicFn
	writeFileAtomicFn = func(path string, data []byte, perm os.FileMode) error {
		if strings.HasSuffix(path, ".handled.json") {
			return os.ErrPermission
		}
		return orig(path, data, perm)
	}
	_, err := box.SupersedePendingRoutine(context.Background(), sender, recipient, liveBinding, supersedeBody, "")
	writeFileAtomicFn = orig
	if err == nil {
		t.Fatal("an uncommitted disposition must return nonzero")
	}

	// Restart: a cold instance with no seen-set and no cached ack state.
	restarted := NewMailbox(box.MailFile)
	pending, err := restarted.PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("after restart pending = %d, want both old envelopes plus the durable replacement", len(pending))
	}
	out, err := restarted.SupersedePendingRoutine(context.Background(), sender, recipient, liveBinding, supersedeBody, "")
	if err != nil {
		t.Fatalf("recovery on a restarted process: %v", err)
	}
	if len(out.SupersededIDs) != 2 || !out.Idempotent {
		t.Fatalf("recovery = %+v, want the stale pair retired against the already-durable replacement", out)
	}

	// And once committed, a further cold instance sees exactly the replacement.
	final, err := NewMailbox(box.MailFile).PendingQueued(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 1 || final[0].ID != out.EnvelopeID {
		t.Fatalf("after recovery a cold reader sees %d pending, want exactly the replacement", len(final))
	}
}
