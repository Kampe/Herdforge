package boardfreeze

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERD_STATE_DIR", dir)
	return dir
}

func TestFreshStateIsOff(t *testing.T) {
	isolate(t)
	st, frozen, err := Active(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if frozen || st.On {
		t.Fatalf("fresh state must not be frozen: %+v", st)
	}
	if st.Generation != 0 {
		t.Fatalf("fresh generation = %d, want 0", st.Generation)
	}
}

func TestSetOnRequiresActorAndReason(t *testing.T) {
	isolate(t)
	if _, err := SetState(true, "", "reason", "", nil, time.Now()); err == nil {
		t.Fatal("empty actor must be rejected")
	}
	if _, err := SetState(true, "actor", "", "", nil, time.Now()); err == nil {
		t.Fatal("empty reason on freeze-on must be rejected")
	}
	if _, err := SetState(false, "", "", "", nil, time.Now()); err == nil {
		t.Fatal("empty actor on freeze-off must be rejected")
	}
}

func TestOnOffGenerationIsMonotonic(t *testing.T) {
	isolate(t)
	on, err := SetState(true, "op", "why", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !on.On || on.Generation != 1 {
		t.Fatalf("first on: %+v", on)
	}
	_, frozen, err := Active(time.Now())
	if err != nil || !frozen {
		t.Fatalf("must read frozen after on: frozen=%v err=%v", frozen, err)
	}

	off, err := SetState(false, "op", "", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if off.On || off.Generation != 2 {
		t.Fatalf("off: %+v", off)
	}
	_, frozen, err = Active(time.Now())
	if err != nil || frozen {
		t.Fatalf("must read unfrozen after off: frozen=%v err=%v", frozen, err)
	}

	// Re-freezing with a different reason bumps generation again, even
	// though the effective on/off value doesn't change from "off" — this
	// is what lets a caller distinguish "still the same freeze" from "a
	// newer decision was made" without inspecting reason text.
	again, err := SetState(true, "op", "why again", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if again.Generation != 3 {
		t.Fatalf("generation must keep incrementing: %+v", again)
	}
}

func TestExpiryAutoClearsWithoutMutatingState(t *testing.T) {
	isolate(t)
	past := time.Now().Add(-time.Hour)
	st, err := SetState(true, "op", "temporary hold", "", &past, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !st.On {
		t.Fatalf("persisted state must still say on: %+v", st)
	}
	_, frozen, err := Active(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if frozen {
		t.Fatal("an expired freeze must read as not-frozen")
	}
}

func TestRestartPersistsGenerationAndBlockedCount(t *testing.T) {
	dir := isolate(t)
	if _, err := SetState(true, "op", "why", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := RecordBlock(); err != nil {
		t.Fatal(err)
	}
	if err := RecordBlock(); err != nil {
		t.Fatal(err)
	}

	// Simulate a fresh process: re-derive the path exactly like a new herd
	// invocation would (via StateDir()), open a brand new Store, and read.
	path := filepath.Join(dir, "board-freeze.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Generation != 1 || st.BlockedMutations != 2 || !st.On {
		t.Fatalf("state did not survive restart: %+v", st)
	}
}

func TestConcurrentRecordBlockNeverLosesAnIncrement(t *testing.T) {
	isolate(t)
	if _, err := SetState(true, "op", "load test", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine opens its OWN Store, mimicking separate herd
			// processes racing to record a blocked mutation concurrently.
			errs <- RecordBlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, _, err := Active(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.BlockedMutations != n {
		t.Fatalf("blocked_mutations = %d, want %d (lost update under concurrency)", st.BlockedMutations, n)
	}
}

func TestFailsClosedWhenStateFileIsCorrupt(t *testing.T) {
	dir := isolate(t)
	path := filepath.Join(dir, "board-freeze.db")
	if err := os.WriteFile(path, []byte("definitely not a sqlite file"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, frozen, err := Active(time.Now())
	if err == nil {
		t.Fatal("corrupt state file must surface an error")
	}
	if !frozen {
		t.Fatal("corrupt state file must fail CLOSED (frozen=true), not open")
	}
	if st.On {
		t.Fatalf("no readable state should be trusted: %+v", st)
	}
}
