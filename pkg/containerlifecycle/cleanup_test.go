package containerlifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// recordingRemover/AbsenceChecker let tests assert exactly which
// container IDs a compensation path touched, and in what order, without
// ever shelling out to real docker.
type recorder struct {
	removeCalls []string
	absentCalls []string
	removeErr   map[string]error
	absentOK    map[string]bool
	absentErr   map[string]error
}

func newRecorder() *recorder {
	return &recorder{removeErr: map[string]error{}, absentOK: map[string]bool{}, absentErr: map[string]error{}}
}

func (r *recorder) remove(_ context.Context, id string) error {
	r.removeCalls = append(r.removeCalls, id)
	return r.removeErr[id]
}

func (r *recorder) absent(_ context.Context, id string) (bool, error) {
	r.absentCalls = append(r.absentCalls, id)
	if err, ok := r.absentErr[id]; ok {
		return false, err
	}
	ok, set := r.absentOK[id]
	if !set {
		return true, nil
	}
	return ok, nil
}

func TestEnsureCleanupSuccessPath(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	if err := EnsureCleanup(context.Background(), s, "c1", "success", rec.remove, rec.absent); err != nil {
		t.Fatalf("EnsureCleanup: %v", err)
	}
	if len(rec.removeCalls) != 1 || rec.removeCalls[0] != "c1" {
		t.Fatalf("removeCalls = %v", rec.removeCalls)
	}
	if len(rec.absentCalls) != 1 || rec.absentCalls[0] != "c1" {
		t.Fatalf("absentCalls = %v", rec.absentCalls)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateRemoved || !got.AbsenceProved {
		t.Fatalf("got = %+v", got)
	}
}

// TestEnsureCleanupRecordsExpectedTerminalState targets the gap where a
// receipt went straight from registered/started to removed with a blank
// expected_terminal_state, losing the outcome classification the
// "single compensation path" is supposed to capture.
func TestEnsureCleanupRecordsExpectedTerminalState(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	if err := EnsureCleanup(context.Background(), s, "c1", "test_failed", rec.remove, rec.absent); err != nil {
		t.Fatalf("EnsureCleanup: %v", err)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExpectedTerminalState != "test_failed" {
		t.Fatalf("expected_terminal_state = %q, want test_failed", got.ExpectedTerminalState)
	}
}

// TestEnsureCleanupPreservesExplicitAwaitingCleanupState proves
// EnsureCleanup never overwrites a real outcome a caller already
// recorded via its own MarkAwaitingCleanup call — it only fills in the
// blank when nothing more specific was set.
func TestEnsureCleanupPreservesExplicitAwaitingCleanupState(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.MarkAwaitingCleanup("c1", "success"); err != nil {
		t.Fatalf("MarkAwaitingCleanup: %v", err)
	}
	rec := newRecorder()
	// A later reconcile sweep would pass "orphaned" — it must not
	// clobber the real "success" outcome already recorded.
	if err := EnsureCleanup(context.Background(), s, "c1", "orphaned", rec.remove, rec.absent); err != nil {
		t.Fatalf("EnsureCleanup: %v", err)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExpectedTerminalState != "success" {
		t.Fatalf("expected_terminal_state = %q, want preserved 'success'", got.ExpectedTerminalState)
	}
}

func TestEnsureCleanupFailurePathQuarantinesWhenStillPresent(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	rec.removeErr["c1"] = errors.New("docker daemon unreachable")
	rec.absentOK["c1"] = false // remove failed AND the container is confirmed still there
	if err := EnsureCleanup(context.Background(), s, "c1", "success", rec.remove, rec.absent); err == nil {
		t.Fatal("expected error from failed remove + confirmed presence")
	}
	if len(rec.absentCalls) != 1 {
		t.Fatalf("absence must still be checked after a failed remove: absentCalls=%v", rec.absentCalls)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateQuarantined {
		t.Fatalf("state = %s, want quarantined", got.State)
	}
}

// TestEnsureCleanupSucceedsWhenRemoveErrorsButAbsenceConfirmed proves the
// independent absence check — not docker rm's own error text — is the
// authority for whether a container is actually gone. A "no such
// container" style failure from remove() must not need to be
// pattern-matched by EnsureCleanup to still record success, because the
// absence check settles it either way.
func TestEnsureCleanupSucceedsWhenRemoveErrorsButAbsenceConfirmed(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	rec.removeErr["c1"] = errors.New("Error: No such container: c1")
	// absentOK left unset -> defaults to true (confirmed gone).
	if err := EnsureCleanup(context.Background(), s, "c1", "success", rec.remove, rec.absent); err != nil {
		t.Fatalf("EnsureCleanup: %v", err)
	}
	if len(rec.absentCalls) != 1 {
		t.Fatalf("absence must be checked even though remove errored: absentCalls=%v", rec.absentCalls)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateRemoved || !got.AbsenceProved {
		t.Fatalf("got = %+v, want removed with absence proved despite remove() erroring", got)
	}
}

func TestEnsureCleanupRefusesUnregisteredContainer(t *testing.T) {
	s := newTestStore(t)
	rec := newRecorder()
	if err := EnsureCleanup(context.Background(), s, "unregistered", "success", rec.remove, rec.absent); !errors.Is(err, ErrUnknownContainer) {
		t.Fatalf("err = %v, want ErrUnknownContainer", err)
	}
	if len(rec.removeCalls) != 0 || len(rec.absentCalls) != 0 {
		t.Fatalf("must never touch docker for an unregistered id: remove=%v absent=%v", rec.removeCalls, rec.absentCalls)
	}
}

func TestEnsureCleanupQuarantinesWhenAbsenceCheckErrors(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	rec.absentErr["c1"] = errors.New("docker inspect timed out")
	if err := EnsureCleanup(context.Background(), s, "c1", "success", rec.remove, rec.absent); err == nil {
		t.Fatal("expected error when absence check itself errors")
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateQuarantined {
		t.Fatalf("state = %s, want quarantined", got.State)
	}
}

func TestEnsureCleanupIsIdempotentOnAlreadyTerminalReceipt(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	if err := EnsureCleanup(context.Background(), s, "c1", "success", rec.remove, rec.absent); err != nil {
		t.Fatalf("first EnsureCleanup: %v", err)
	}
	// Second call (e.g. both a timeout path and a later reconcile sweep
	// racing on the same container) must not re-invoke docker.
	if err := EnsureCleanup(context.Background(), s, "c1", "success", rec.remove, rec.absent); err != nil {
		t.Fatalf("second EnsureCleanup: %v", err)
	}
	if len(rec.removeCalls) != 1 {
		t.Fatalf("remove called %d times, want exactly once", len(rec.removeCalls))
	}
}

func TestEnsureCleanupWorksWithFreshTeardownContextAfterOriginalCancelled(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Simulate a timed-out/cancelled run: its own context is dead, but
	// the deferred cleanup call is made with a fresh teardown context,
	// the same pattern the hermetic runner uses for its own teardown.
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if runCtx.Err() == nil {
		t.Fatal("test setup: run context should already be cancelled")
	}
	teardownCtx := context.Background()
	rec := newRecorder()
	if err := EnsureCleanup(teardownCtx, s, "c1", "timeout", rec.remove, rec.absent); err != nil {
		t.Fatalf("EnsureCleanup with fresh teardown context: %v", err)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateRemoved {
		t.Fatalf("state = %s, want removed", got.State)
	}
}

func TestReconcileReclaimsOnlyDeadGenerations(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "dead", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register dead: %v", err)
	}
	if _, err := s.Register(Receipt{ContainerID: "alive", TaskRef: "FAC-200", Generation: "g2"}); err != nil {
		t.Fatalf("Register alive: %v", err)
	}
	rec := newRecorder()
	live := func(taskRef, generation string) bool { return generation == "g2" }
	report, err := Reconcile(context.Background(), s, live, rec.remove, rec.absent)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Reclaimed) != 1 || report.Reclaimed[0] != "dead" {
		t.Fatalf("Reclaimed = %v, want [dead]", report.Reclaimed)
	}
	if len(report.Skipped) != 1 || report.Skipped[0] != "alive" {
		t.Fatalf("Skipped = %v, want [alive]", report.Skipped)
	}
	if !contains(rec.removeCalls, "dead") || contains(rec.removeCalls, "alive") {
		t.Fatalf("removeCalls = %v, want only dead touched", rec.removeCalls)
	}
}

func TestReconcileReportIsSortedByContainerID(t *testing.T) {
	s := newTestStore(t)
	// Register in an order that does NOT match ID-sorted order, so a
	// report that merely preserved receipt-insertion order would fail
	// this assertion.
	for _, id := range []string{"zebra", "apple", "mango"} {
		if _, err := s.Register(Receipt{ContainerID: id, TaskRef: "FAC-200", Generation: "g1"}); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
	}
	rec := newRecorder()
	live := func(string, string) bool { return false }
	report, err := Reconcile(context.Background(), s, live, rec.remove, rec.absent)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{"apple", "mango", "zebra"}
	if fmt.Sprint(report.Reclaimed) != fmt.Sprint(want) {
		t.Fatalf("Reclaimed = %v, want sorted %v", report.Reclaimed, want)
	}
}

func TestReconcileIsIdempotentAcrossSweeps(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "orphan", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	live := func(string, string) bool { return false }

	first, err := Reconcile(context.Background(), s, live, rec.remove, rec.absent)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if len(first.Reclaimed) != 1 {
		t.Fatalf("first sweep Reclaimed = %v, want one", first.Reclaimed)
	}

	second, err := Reconcile(context.Background(), s, live, rec.remove, rec.absent)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(second.Reclaimed) != 0 || len(second.Skipped) != 0 || len(second.Quarantined) != 0 {
		t.Fatalf("second sweep = %+v, want fully empty (already terminal)", second)
	}
	if len(rec.removeCalls) != 1 {
		t.Fatalf("remove invoked %d times across two sweeps, want exactly once", len(rec.removeCalls))
	}
}

func TestReconcileNeverTouchesContainersWithoutAReceipt(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "owned", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := newRecorder()
	live := func(string, string) bool { return false }
	if _, err := Reconcile(context.Background(), s, live, rec.remove, rec.absent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// A container Docker knows about but this store never registered
	// (e.g. a legacy FAC-174 leftover) must never appear in any call
	// Reconcile makes — it only iterates its own receipts.
	if contains(rec.removeCalls, "legacy-unowned") {
		t.Fatalf("removeCalls touched an unregistered id: %v", rec.removeCalls)
	}
	sort.Strings(rec.removeCalls)
	if fmt.Sprint(rec.removeCalls) != "[owned]" {
		t.Fatalf("removeCalls = %v, want exactly [owned]", rec.removeCalls)
	}
}

func TestStaleGenerationLiveTracksRecency(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	live := StaleGenerationLive(s, 10*time.Minute, func() time.Time { return time.Now().UTC() })
	if !live("FAC-200", "g1") {
		t.Fatal("a receipt just registered should be considered live")
	}
	staleClock := func() time.Time { return time.Now().UTC().Add(time.Hour) }
	liveLater := StaleGenerationLive(s, 10*time.Minute, staleClock)
	if liveLater("FAC-200", "g1") {
		t.Fatal("a receipt untouched for an hour should be considered stale under a 10m window")
	}
	if liveLater("FAC-200", "unknown-generation") {
		t.Fatal("a generation with no matching receipt at all must not be reported live")
	}
}

// backdateUpdatedAt directly rewrites a receipt's updated_at, bypassing
// the public API (which always stamps real wall-clock time). Tests use
// this to simulate "registered a long time ago" without needing an
// injectable clock inside Store itself.
func backdateUpdatedAt(t *testing.T, s *Store, containerID string, when time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE container_receipts SET updated_at = ? WHERE container_id = ?`, when, containerID); err != nil {
		t.Fatalf("backdate %s: %v", containerID, err)
	}
}

// TestReconcileWithStaleGenerationLiveReclaimsAllSiblingsInOneSweep
// reproduces the exact regression a naive StaleGenerationLive
// implementation hits: reclaiming c-a bumps its own updated_at to now
// via MarkRemoved, and if that timestamp were allowed to count as
// evidence for its OWN generation, checking c-b right afterward in the
// same sweep would see "recent activity in g1" and wrongly skip it.
// Unlike TestReconcileReclaimsOnlyDeadGenerations (which injects a
// hand-written live func and can't catch this), this test drives
// Reconcile with the real StaleGenerationLive helper across multiple
// siblings sharing one generation, so it exercises the exact code path
// `herd containers reconcile` uses in production. All three receipts are
// backdated to look genuinely old (well past staleAfter) BEFORE the
// sweep starts, and the check clock is real wall-clock time — so a
// reclaimed sibling's freshly-written removed_at (which Store always
// stamps with the real clock, not a test-injected one) sits right next
// to "now", the exact condition that fools an implementation which
// doesn't exclude terminal states from its evidence.
func TestReconcileWithStaleGenerationLiveReclaimsAllSiblingsInOneSweep(t *testing.T) {
	s := newTestStore(t)
	longAgo := time.Now().UTC().Add(-time.Hour)
	for _, id := range []string{"c-a", "c-b", "c-c"} {
		if _, err := s.Register(Receipt{ContainerID: id, TaskRef: "FAC-200", Generation: "g1"}); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
		backdateUpdatedAt(t, s, id, longAgo)
	}
	live := StaleGenerationLive(s, 10*time.Minute, func() time.Time { return time.Now().UTC() })
	rec := newRecorder()
	report, err := Reconcile(context.Background(), s, live, rec.remove, rec.absent)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{"c-a", "c-b", "c-c"}
	if fmt.Sprint(report.Reclaimed) != fmt.Sprint(want) {
		t.Fatalf("Reclaimed = %v, want all three siblings reclaimed in one sweep: %v", report.Reclaimed, want)
	}
	if len(report.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want none — a sibling's own cleanup must never protect the rest of a dead generation", report.Skipped)
	}
}

// TestStaleGenerationLiveIgnoresTerminalReceiptsAsEvidence is the
// narrower unit-level version of the sibling-poisoning regression above:
// a receipt that is already removed/quarantined must never make its
// generation look live, even if its own updated_at is very recent.
func TestStaleGenerationLiveIgnoresTerminalReceiptsAsEvidence(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "removed-recently", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.MarkRemoved("removed-recently", true); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	// The only receipt for this generation is now terminal (removed) and
	// was touched "just now" — but a terminal state must never count as
	// evidence of a live generation, or a dead generation would look
	// live forever the moment anything in it gets cleaned up.
	live := StaleGenerationLive(s, time.Hour, func() time.Time { return time.Now().UTC() })
	if live("FAC-200", "g1") {
		t.Fatal("a recently-removed receipt must not make its generation look live")
	}
}

// TestRegisterConcurrentSameIdentityYieldsExactlyOneRow exercises the
// atomic check-then-act path: concurrent Register calls for the same
// identity must never race into two rows or a spurious constraint error.
func TestRegisterConcurrentSameIdentityYieldsExactlyOneRow(t *testing.T) {
	s := newTestStore(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Register: %v", i, err)
		}
	}
	all, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("concurrent identical Register calls produced %d rows, want 1", len(all))
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
