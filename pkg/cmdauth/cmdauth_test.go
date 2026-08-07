package cmdauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests use no native/cgo build tag fixture and send no host signals.
// Every "process" in the FAC-151 reproduction is a counting Spawn, and the
// one test that starts a real process (TestOwnedSpawn*) runs a short shell
// command to completion without signalling it.

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmdauth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// countingSpawn records every process creation and writes one line to a
// marker file, so a test can assert both "how many processes" and "what the
// filesystem looks like afterwards".
type countingSpawn struct {
	mu       sync.Mutex
	calls    int
	marker   string
	exitCode int
}

func (c *countingSpawn) fn() Spawn {
	return func(ctx context.Context, dir string, argv []string) (int, error) {
		c.mu.Lock()
		c.calls++
		n := c.calls
		c.mu.Unlock()
		if c.marker != "" {
			f, err := os.OpenFile(c.marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return -1, err
			}
			fmt.Fprintf(f, "exec %d: %s\n", n, strings.Join(argv, " "))
			f.Close()
		}
		return c.exitCode, nil
	}
}

func (c *countingSpawn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func fac151Authorization(dir string, argv []string) Authorization {
	return Authorization{
		CommandID:   "fac151-guarded-test-1",
		CommandHash: CanonicalHash(dir, argv),
		MaxAttempts: 1,
		Authority:   "root",
		Lane:        "worker-a",
		SessionID:   "019fcb4f-f1c8-7bc1-8c1a-63da16634829",
		Disposition: StopOnFirstFailure,
	}
}

func request(a Authorization) Request {
	return Request{CommandID: a.CommandID, Lane: a.Lane, SessionID: a.SessionID}
}

// TestFAC151FixturePermitsExactlyOneExecution is the incident, reproduced:
// one authorization, max 1 attempt, stop-on-first-failure, and a worker that
// tries four times in one turn while editing code in between.
func TestFAC151FixturePermitsExactlyOneExecution(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	work := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executions.log")
	argv := []string{"go", "test", "-run", "TestGuarded", "./pkg/importer"}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	spawn := &countingSpawn{marker: marker, exitCode: 1} // the guarded test fails

	// Attempt 1: authorized. It runs and fails, exactly as in the incident.
	out, err := store.Run(ctx, request(auth), work, argv, spawn.fn())
	if err != nil {
		t.Fatalf("first attempt must be permitted: %v", err)
	}
	if !out.Ran || out.ExitCode != 1 || out.Grant.Attempt != 1 {
		t.Fatalf("unexpected first outcome: %+v", out)
	}

	// Attempts 2-4: the worker "improvises repairs" and reruns. Each edit is
	// simulated by touching a file in the worktree between attempts, which is
	// exactly what made the original reruns look legitimate.
	for attempt := 2; attempt <= 4; attempt++ {
		if err := os.WriteFile(filepath.Join(work, "edit.go"),
			[]byte(fmt.Sprintf("// repair %d\n", attempt)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := store.Run(ctx, request(auth), work, argv, spawn.fn())
		if err == nil {
			t.Fatalf("attempt %d must be rejected; the FAC-151 control failed open", attempt)
		}
		if !errors.Is(err, ErrStopOnFailure) {
			t.Fatalf("attempt %d rejected for the wrong reason: %v", attempt, err)
		}
	}

	if got := spawn.count(); got != 1 {
		t.Fatalf("process creations = %d, want exactly 1", got)
	}

	// Zero child-process and filesystem side effects on the rejected path:
	// the marker file records one and only one execution.
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("marker recorded %d executions:\n%s", len(lines), data)
	}

	// And the ledger explains all four attempts.
	receipts, err := store.Receipts(ctx, auth.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	got := eventSequence(receipts)
	want := []string{EventAuthorized, EventConsumed, EventFailed, EventRejected, EventRejected, EventRejected}
	if !equal(got, want) {
		t.Fatalf("receipt events = %v, want %v", got, want)
	}
}

// TestRejectedAttemptNeverReachesSpawn proves the refusal happens BEFORE
// process creation rather than being cleaned up after it.
func TestRejectedAttemptNeverReachesSpawn(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "build", "./..."}
	auth := fac151Authorization(work, argv)
	auth.Disposition = ContinueOnFailure
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}

	explode := Spawn(func(context.Context, string, []string) (int, error) {
		t.Fatal("spawn was reached on a path that must refuse before process creation")
		return 0, nil
	})

	// Budget of 1, already spent by a successful run.
	if _, err := store.Run(ctx, request(auth), work, argv, func(context.Context, string, []string) (int, error) {
		return 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Run(ctx, request(auth), work, argv, explode); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want ErrBudgetExhausted, got %v", err)
	}

	// Unknown command id.
	if _, err := store.Run(ctx, Request{CommandID: "never-issued", Lane: auth.Lane, SessionID: auth.SessionID},
		work, argv, explode); !errors.Is(err, ErrNoAuthorization) {
		t.Fatalf("want ErrNoAuthorization, got %v", err)
	}
}

// TestAuthorizedCommandCannotBeSwappedForAnother is the reason Run recomputes
// the hash instead of trusting the request: an authorization for one command
// must not be spendable on a different one.
func TestAuthorizedCommandCannotBeSwappedForAnother(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	authorized := []string{"go", "test", "./pkg/importer"}
	auth := fac151Authorization(work, authorized)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}

	spawn := &countingSpawn{}
	for _, other := range [][]string{
		{"go", "test", "./..."},              // wider scope
		{"rm", "-rf", "."},                   // different command entirely
		{"go", "test", "./pkg/importer", ""}, // extra empty arg
	} {
		if _, err := store.Run(ctx, request(auth), work, other, spawn.fn()); !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("argv %v: want ErrHashMismatch, got %v", other, err)
		}
	}
	// Same argv, different directory is also a different command.
	if _, err := store.Run(ctx, request(auth), t.TempDir(), authorized, spawn.fn()); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("different dir: want ErrHashMismatch, got %v", err)
	}
	if spawn.count() != 0 {
		t.Fatalf("hash mismatch spawned %d processes", spawn.count())
	}
	// The authorization is untouched: the real command still runs once.
	if _, err := store.Run(ctx, request(auth), work, authorized, spawn.fn()); err != nil {
		t.Fatalf("authorized command must still be runnable: %v", err)
	}
}

// TestCanonicalHashIsUnambiguous pins the length-prefixing: no regrouping of
// the same bytes may collide.
func TestCanonicalHashIsUnambiguous(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"ab", "c"},
		{"a", "bc"},
		{"abc"},
		{"a", "b", "c"},
	}
	seen := map[string]bool{}
	for _, argv := range cases {
		h := CanonicalHash("/dir", argv)
		if seen[h] {
			t.Fatalf("hash collision for %v", argv)
		}
		seen[h] = true
	}
	if CanonicalHash("a", []string{"b"}) == CanonicalHash("ab", nil) {
		t.Fatal("dir/argv boundary is ambiguous")
	}
	if CanonicalHash("d", []string{"x"}) != CanonicalHash("d", []string{"x"}) {
		t.Fatal("hash is not stable")
	}
}

// TestFailedStopTokenSurvivesReconnectAndResume: a fresh Store over the same
// file is what a reconnected/resumed session gets. The refusal must persist.
func TestFailedStopTokenSurvivesReconnectAndResume(t *testing.T) {
	ctx := context.Background()
	store, path := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	auth := fac151Authorization(work, argv)
	auth.MaxAttempts = 5 // budget remains; the disposition is what burns it
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	spawn := &countingSpawn{exitCode: 2}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); err != nil {
		t.Fatal(err)
	}

	// Reconnect: a brand-new process opens the same ledger.
	reconnected, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	if _, err := reconnected.Run(ctx, request(auth), work, argv, spawn.fn()); !errors.Is(err, ErrStopOnFailure) {
		t.Fatalf("reconnected session: want ErrStopOnFailure, got %v", err)
	}
	if spawn.count() != 1 {
		t.Fatalf("processes=%d, want 1 despite %d remaining budget", spawn.count(), auth.MaxAttempts-1)
	}
	st, err := reconnected.Get(ctx, auth.CommandID)
	if err != nil || st == nil {
		t.Fatalf("get: %v %v", st, err)
	}
	if st.Terminal != TerminalFailedStop {
		t.Fatalf("terminal=%q want %q", st.Terminal, TerminalFailedStop)
	}
}

// TestDuplicateDeliveryDoesNotRefreshBudget: a retried/duplicated prompt
// delivering the same packet must not hand back attempts.
func TestDuplicateDeliveryDoesNotRefreshBudget(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"make", "test"}
	auth := fac151Authorization(work, argv)
	auth.Disposition = ContinueOnFailure

	for i := 0; i < 3; i++ { // duplicate delivery, three times
		if _, err := store.Authorize(ctx, auth); err != nil {
			t.Fatalf("duplicate delivery %d must be idempotent: %v", i, err)
		}
	}
	spawn := &countingSpawn{exitCode: 1}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); err != nil {
		t.Fatal(err)
	}
	// Re-deliver again after the budget is spent.
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want ErrBudgetExhausted after re-delivery, got %v", err)
	}
	if spawn.count() != 1 {
		t.Fatalf("processes=%d, want 1", spawn.count())
	}
}

// TestReauthorizingSameIDOnDifferentTermsIsRefused: an ID cannot quietly
// change what it permits (the laundering path a coordinator race could open).
func TestReauthorizingSameIDOnDifferentTermsIsRefused(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "vet", "./..."}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(Authorization) Authorization{
		func(a Authorization) Authorization { a.MaxAttempts = 9; return a },
		func(a Authorization) Authorization { a.Disposition = ContinueOnFailure; return a },
		func(a Authorization) Authorization { a.CommandHash = CanonicalHash(work, []string{"sh"}); return a },
		func(a Authorization) Authorization { a.Lane = "worker-b"; return a },
		func(a Authorization) Authorization { a.Authority = "worker"; return a },
	} {
		if _, err := store.Authorize(ctx, mutate(auth)); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("want ErrIdentityConflict, got %v", err)
		}
	}
}

// TestOnlyADistinctCommandIDPermitsAnotherAttempt is the acceptance criterion
// verbatim: after a stop-on-failure burn, only a NEW root-authorized token
// with a distinct command ID reopens execution — and it retires the old one.
func TestOnlyADistinctCommandIDPermitsAnotherAttempt(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./pkg/importer"}
	first := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, first); err != nil {
		t.Fatal(err)
	}
	spawn := &countingSpawn{exitCode: 1}
	if _, err := store.Run(ctx, request(first), work, argv, spawn.fn()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Run(ctx, request(first), work, argv, spawn.fn()); !errors.Is(err, ErrStopOnFailure) {
		t.Fatalf("want ErrStopOnFailure, got %v", err)
	}

	second := first
	second.CommandID = "fac151-guarded-test-2"
	if _, err := store.Authorize(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Run(ctx, request(second), work, argv, spawn.fn()); err != nil {
		t.Fatalf("a distinct newly authorized command id must be permitted: %v", err)
	}
	if spawn.count() != 2 {
		t.Fatalf("processes=%d, want 2", spawn.count())
	}
	// The spent token stays spent; the new one does not resurrect it.
	if _, err := store.Run(ctx, request(first), work, argv, spawn.fn()); !errors.Is(err, ErrStopOnFailure) {
		t.Fatalf("old token must stay burned, got %v", err)
	}
}

// TestNewAuthorizationSupersedesOpenOneForLane: a lane never holds two live
// tokens, so it cannot fall back to an older one after a new packet arrives.
func TestNewAuthorizationSupersedesOpenOneForLane(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argvA := []string{"go", "build", "./..."}
	argvB := []string{"go", "test", "./..."}
	a := fac151Authorization(work, argvA)
	a.MaxAttempts = 3
	a.Disposition = ContinueOnFailure
	if _, err := store.Authorize(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := fac151Authorization(work, argvB)
	b.CommandID = "second"
	b.CommandHash = CanonicalHash(work, argvB)
	if _, err := store.Authorize(ctx, b); err != nil {
		t.Fatal(err)
	}
	spawn := &countingSpawn{}
	if _, err := store.Run(ctx, request(a), work, argvA, spawn.fn()); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("want ErrSuperseded, got %v", err)
	}
	if spawn.count() != 0 {
		t.Fatal("superseded token spawned a process")
	}
	st, err := store.Get(ctx, a.CommandID)
	if err != nil || st.Terminal != TerminalSuperseded {
		t.Fatalf("state=%+v err=%v", st, err)
	}
}

// TestIdentityMismatchIsRefused: a token issued to one lane/session cannot be
// presented by another.
func TestIdentityMismatchIsRefused(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	spawn := &countingSpawn{}
	for _, req := range []Request{
		{CommandID: auth.CommandID, Lane: "worker-b", SessionID: auth.SessionID},
		{CommandID: auth.CommandID, Lane: auth.Lane, SessionID: "another-session"},
		{CommandID: auth.CommandID},
	} {
		if _, err := store.Run(ctx, req, work, argv, spawn.fn()); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("req %+v: want ErrIdentityMismatch, got %v", req, err)
		}
	}
	if spawn.count() != 0 {
		t.Fatal("identity mismatch spawned a process")
	}
}

// refusingProver is a stand-in for the FAC-193 ownership check.
type refusingProver struct{ err error }

func (p refusingProver) ProveOwner(context.Context, string, string) error { return p.err }

// TestOwnerProverSeamFailsClosed pins the FAC-193 seam's contract: when a
// prover is wired and refuses, no attempt is consumed and nothing spawns.
func TestOwnerProverSeamFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	store.Prover = refusingProver{err: errors.New("pid 4242 is not this lane's exec session")}
	spawn := &countingSpawn{}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); !errors.Is(err, ErrOwnerUnproven) {
		t.Fatalf("want ErrOwnerUnproven, got %v", err)
	}
	if spawn.count() != 0 {
		t.Fatal("unproven owner spawned a process")
	}
	st, err := store.Get(ctx, auth.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if st.AttemptsUsed != 0 {
		t.Fatalf("a refused ownership proof consumed %d attempts", st.AttemptsUsed)
	}
	// With the prover satisfied the same token still works: the seam refuses,
	// it does not corrupt.
	store.Prover = refusingProver{err: nil}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); err != nil {
		t.Fatalf("proven owner must be permitted: %v", err)
	}
}

// TestConsumptionIsAtomicAcrossCompetingStores races independent Stores (each
// with its own connection to the same file, i.e. the same OS-level locking a
// separate process gets) for a single-attempt budget. Exactly one may win.
func TestConsumptionIsAtomicAcrossCompetingStores(t *testing.T) {
	ctx := context.Background()
	seed, path := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	auth := fac151Authorization(work, argv)
	if _, err := seed.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	spawn := &countingSpawn{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	granted := make(chan Grant, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			<-start
			out, err := s.Run(ctx, request(auth), work, argv, spawn.fn())
			if err != nil {
				errs <- err
				return
			}
			granted <- out.Grant
		}()
	}
	close(start)
	wg.Wait()
	close(granted)
	close(errs)

	var wins []Grant
	for g := range granted {
		wins = append(wins, g)
	}
	if len(wins) != 1 {
		t.Fatalf("%d workers were granted the single attempt: %+v", len(wins), wins)
	}
	if wins[0].Attempt != 1 {
		t.Fatalf("granted attempt %d, want 1", wins[0].Attempt)
	}
	if spawn.count() != 1 {
		t.Fatalf("process creations=%d, want 1", spawn.count())
	}
	var refusals int
	for err := range errs {
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("loser refused for the wrong reason: %v", err)
		}
		refusals++
	}
	if refusals != workers-1 {
		t.Fatalf("refusals=%d, want %d", refusals, workers-1)
	}
	// Exactly one consumed receipt exists, so the ledger agrees with reality.
	receipts, err := seed.Receipts(ctx, auth.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if n := countEvent(receipts, EventConsumed); n != 1 {
		t.Fatalf("consumed receipts=%d, want 1", n)
	}
}

// TestReceiptLedgerIsAppendOnly proves the append-only claim is enforced by
// the database, not by convention — this is what makes the counter/ledger
// cross-check in Consume worth anything.
func TestReceiptLedgerIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Run(ctx, request(auth), work, argv, (&countingSpawn{}).fn()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM command_receipts WHERE event = ?`, EventConsumed); err == nil {
		t.Fatal("deleting a consumed receipt must abort")
	}
	if _, err := store.DB().Exec(`UPDATE command_receipts SET event = 'rejected'`); err == nil {
		t.Fatal("updating a receipt must abort")
	}
}

// TestAttemptConsumptionResetIsRejected is the mutation criterion, exercised
// non-vacuously: the mutation is applied for real (the attempt counter and
// terminal flag are reset directly in the database, exactly what a "just let
// me rerun it" patch would do), and the boundary must still refuse — because
// the append-only ledger is an independent record of what was spent.
func TestAttemptConsumptionResetIsRejected(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./pkg/importer"}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	spawn := &countingSpawn{exitCode: 1}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); err != nil {
		t.Fatal(err)
	}

	// The mutation: remove attempt consumption and clear the burn.
	if _, err := store.DB().Exec(
		`UPDATE command_authorizations SET attempts_used = 0, terminal = '' WHERE command_id = ?`,
		auth.CommandID); err != nil {
		t.Fatal(err)
	}
	st, err := store.Get(ctx, auth.CommandID)
	if err != nil || st.AttemptsUsed != 0 || st.Terminal != TerminalNone {
		t.Fatalf("mutation fixture broken: %+v (%v)", st, err)
	}

	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); !errors.Is(err, ErrLedgerTampered) {
		t.Fatalf("reset consumption must fail closed, got %v", err)
	}
	if spawn.count() != 1 {
		t.Fatalf("process creations=%d after reset, want 1", spawn.count())
	}
}

// TestDurableReadbackIdentifiesEveryField covers the readback criterion:
// command ID/hash, session/lane, attempt number, result, and rejection reason
// are all recoverable from the ledger.
func TestDurableReadbackIdentifiesEveryField(t *testing.T) {
	ctx := context.Background()
	store, path := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./pkg/importer"}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	spawn := &countingSpawn{exitCode: 7}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); err == nil {
		t.Fatal("second attempt must be rejected")
	}

	// Read back from a fresh handle: this is durable state, not memory.
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	receipts, err := reader.Receipts(ctx, auth.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	byEvent := map[string]Receipt{}
	for _, r := range receipts {
		if r.CommandID != auth.CommandID {
			t.Fatalf("receipt %d has command id %q", r.Seq, r.CommandID)
		}
		if r.Lane != auth.Lane || r.SessionID != auth.SessionID {
			t.Fatalf("receipt %d lost lane/session identity: %+v", r.Seq, r)
		}
		if r.CommandHash != auth.CommandHash {
			t.Fatalf("receipt %d lost the command hash: %+v", r.Seq, r)
		}
		byEvent[r.Event] = r
	}
	consumed, ok := byEvent[EventConsumed]
	if !ok || consumed.Attempt != 1 {
		t.Fatalf("consumed receipt missing or has attempt %d: %+v", consumed.Attempt, consumed)
	}
	failed, ok := byEvent[EventFailed]
	if !ok || failed.ExitCode == nil || *failed.ExitCode != 7 {
		t.Fatalf("failure result not recoverable: %+v", failed)
	}
	rejected, ok := byEvent[EventRejected]
	if !ok || !strings.Contains(rejected.Reason, "stop-on-first-failure") {
		t.Fatalf("rejection reason not recoverable: %+v", rejected)
	}
	if byEvent[EventAuthorized].Reason == "" {
		t.Fatal("authorization terms not recoverable")
	}
}

// TestUnauthorizedAttemptIsRecorded: an execution attempt with no
// authorization at all is evidence worth keeping.
func TestUnauthorizedAttemptIsRecorded(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	req := Request{CommandID: "not-issued", Lane: "worker-a", SessionID: "s1"}
	if _, err := store.Run(ctx, req, work, argv, (&countingSpawn{}).fn()); !errors.Is(err, ErrNoAuthorization) {
		t.Fatalf("want ErrNoAuthorization, got %v", err)
	}
	receipts, err := store.Receipts(ctx, "not-issued")
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Event != EventRejected {
		t.Fatalf("unauthorized attempt left %d receipts: %+v", len(receipts), receipts)
	}
	if receipts[0].CommandHash != CanonicalHash(work, argv) {
		t.Fatal("rejection receipt does not record what was attempted")
	}
}

// TestAuthorizationValidationRejectsWildcards: no field is optional, because
// an empty field would be a wildcard.
func TestAuthorizationValidationRejectsWildcards(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	base := fac151Authorization(t.TempDir(), []string{"go", "test"})
	for name, mutate := range map[string]func(Authorization) Authorization{
		"no id":          func(a Authorization) Authorization { a.CommandID = ""; return a },
		"no hash":        func(a Authorization) Authorization { a.CommandHash = ""; return a },
		"no authority":   func(a Authorization) Authorization { a.Authority = ""; return a },
		"no lane":        func(a Authorization) Authorization { a.Lane = ""; return a },
		"no session":     func(a Authorization) Authorization { a.SessionID = ""; return a },
		"zero attempts":  func(a Authorization) Authorization { a.MaxAttempts = 0; return a },
		"neg attempts":   func(a Authorization) Authorization { a.MaxAttempts = -1; return a },
		"no disposition": func(a Authorization) Authorization { a.Disposition = ""; return a },
		"bad dispositio": func(a Authorization) Authorization { a.Disposition = "retry_forever"; return a },
	} {
		if _, err := store.Authorize(ctx, mutate(base)); err == nil {
			t.Fatalf("%s: authorization must be refused", name)
		}
	}
}

// TestContinueOnFailureSpendsOnlyItsOwnAttempt keeps the other disposition
// honest: it is a real alternative, not a synonym.
func TestContinueOnFailureSpendsOnlyItsOwnAttempt(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	auth := fac151Authorization(work, argv)
	auth.MaxAttempts = 3
	auth.Disposition = ContinueOnFailure
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	spawn := &countingSpawn{exitCode: 1}
	for i := 1; i <= 3; i++ {
		out, err := store.Run(ctx, request(auth), work, argv, spawn.fn())
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if out.Grant.Attempt != i {
			t.Fatalf("attempt number %d, want %d", out.Grant.Attempt, i)
		}
	}
	if _, err := store.Run(ctx, request(auth), work, argv, spawn.fn()); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("fourth attempt: want ErrBudgetExhausted, got %v", err)
	}
	if spawn.count() != 3 {
		t.Fatalf("process creations=%d, want 3", spawn.count())
	}
}

// TestSpawnFailureStillBurnsTheAttempt: a command that cannot start consumed
// its authorization all the same, and burns a stop-on-failure token.
func TestSpawnFailureStillBurnsTheAttempt(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"definitely-not-on-path-fac195"}
	auth := fac151Authorization(work, argv)
	auth.MaxAttempts = 4
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	out, err := store.Run(ctx, request(auth), work, argv, OwnedSpawn(nil, nil))
	if err == nil {
		t.Fatal("a command that cannot start must report an error")
	}
	if !out.Ran || out.ExitCode != -1 {
		t.Fatalf("outcome=%+v", out)
	}
	if _, err := store.Run(ctx, request(auth), work, argv, OwnedSpawn(nil, nil)); !errors.Is(err, ErrStopOnFailure) {
		t.Fatalf("want ErrStopOnFailure, got %v", err)
	}
}

// TestOwnedSpawnReportsRealExitCodes exercises the production spawn against
// real processes. No signals are sent; both commands run to completion.
func TestOwnedSpawnReportsRealExitCodes(t *testing.T) {
	ctx := context.Background()
	spawn := OwnedSpawn(nil, nil)
	dir := t.TempDir()
	code, err := spawn(ctx, dir, []string{"sh", "-c", "exit 0"})
	if err != nil || code != 0 {
		t.Fatalf("exit 0: code=%d err=%v", code, err)
	}
	code, err = spawn(ctx, dir, []string{"sh", "-c", "exit 3"})
	if err != nil || code != 3 {
		t.Fatalf("exit 3: code=%d err=%v", code, err)
	}
	// It really runs in dir.
	code, err = spawn(ctx, dir, []string{"sh", "-c", "touch ran.txt"})
	if err != nil || code != 0 {
		t.Fatalf("touch: code=%d err=%v", code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err != nil {
		t.Fatalf("command did not run in dir: %v", err)
	}
}

// TestRunRefusesEmptyCommand: no argv, no execution, no attempt spent.
func TestRunRefusesEmptyCommand(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	if _, err := store.Run(ctx, Request{CommandID: "x"}, t.TempDir(), nil, (&countingSpawn{}).fn()); err == nil {
		t.Fatal("empty argv must be refused")
	}
}

// TestSupersedeIsIdempotent keeps the coordinator race path boring.
func TestSupersedeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	work := t.TempDir()
	argv := []string{"go", "test", "./..."}
	auth := fac151Authorization(work, argv)
	if _, err := store.Authorize(ctx, auth); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := store.Supersede(ctx, auth.CommandID, "root cancelled the order"); err != nil {
			t.Fatalf("supersede %d: %v", i, err)
		}
	}
	if err := store.Supersede(ctx, "unknown", "x"); !errors.Is(err, ErrNoAuthorization) {
		t.Fatalf("want ErrNoAuthorization, got %v", err)
	}
	receipts, err := store.Receipts(ctx, auth.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if n := countEvent(receipts, EventSuperseded); n != 1 {
		t.Fatalf("superseded receipts=%d, want 1", n)
	}
}

// TestStoreClockSeam keeps timestamps deterministic for readback assertions.
func TestStoreClockSeam(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	fixed := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return fixed }
	auth := fac151Authorization(t.TempDir(), []string{"go", "test"})
	st, err := store.Authorize(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IssuedAt.Equal(fixed) {
		t.Fatalf("issued_at=%v want %v", st.IssuedAt, fixed)
	}
}

func eventSequence(receipts []Receipt) []string {
	out := make([]string, 0, len(receipts))
	for _, r := range receipts {
		out = append(out, r.Event)
	}
	return out
}

func countEvent(receipts []Receipt, event string) int {
	n := 0
	for _, r := range receipts {
		if r.Event == event {
			n++
		}
	}
	return n
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
