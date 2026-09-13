package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderCacheSubprocessHelper(t *testing.T) {
	if os.Getenv("HERD_CACHE_SUBPROCESS_HELPER") != "1" {
		return
	}
	if held := os.Getenv("HERD_CACHE_HOLD_PROVIDER"); held != "" {
		if err := withProviderFileLock(held, currentProviderAccountKey(held), func() error {
			if ready := os.Getenv("HERD_CACHE_HOLDER_READY"); ready != "" {
				if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
					return err
				}
			}
			release := os.Getenv("HERD_CACHE_HOLDER_RELEASE")
			if release == "" {
				// Legacy contract, unchanged for the callers that rely on the
				// holder simply going away on its own.
				time.Sleep(time.Second)
				return nil
			}
			// Release handshake. The lock is held until the PARENT says it is
			// done, so the hold covers the other process's whole attempt by
			// construction rather than by outlasting it. A fixed interval
			// could expire between the parent seeing the ready file and the
			// other process reaching the lock, which is the scheduling
			// assumption this replaces.
			//
			// The fail-safe is not the mechanism, only a leak bound: if the
			// parent dies without releasing, this returns an error instead of
			// holding the lock forever.
			deadline := time.Now().Add(holderReleaseFailSafe)
			for {
				if _, err := os.Stat(release); err == nil {
					return nil
				}
				if !time.Now().Before(deadline) {
					return fmt.Errorf("holder was never released within %s", holderReleaseFailSafe)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	url := os.Getenv("HERD_CACHE_FIXTURE_URL")
	var polls atomic.Int32
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){"codex": func() (ProviderUsage, error) {
		polls.Add(1)
		resp, err := http.Get(url)
		if err != nil {
			return ProviderUsage{}, err
		}
		_ = resp.Body.Close()
		if os.Getenv("HERD_CACHE_429_HELPER") == "1" {
			return ProviderUsage{}, pollErrf("rate-limited", "HTTP 429 retry-after=15")
		}
		return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Remaining: 80, WindowSeconds: 18000},
		}}, nil
	}})
	defer restore()
	_, err := FetchProviderForce("codex", false)
	if os.Getenv("HERD_CACHE_429_HELPER") == "1" {
		if err == nil || pollErrorCode(err) != "rate-limited" {
			t.Fatalf("expected persisted rate-limit result, got %v", err)
		}
		return
	}
	// Report the outcome instead of demanding one. Under cross-process
	// single-flight a caller legitimately ends in one of three states, and
	// which one it reaches depends on the scheduler, not on the contract:
	//
	//   served    -- it won the lock and performed THE one poll
	//   cached    -- it took the lock after the winner and was served from
	//                the cache the winner had just written, polling nothing
	//   contended -- the winner held the lock for the caller's whole bounded
	//                wait, so it returned ErrCacheLockBusy, polling nothing
	//
	// All three preserve single-flight. Anything else is a real failure and is
	// still reported as one below.
	outcome := ""
	switch {
	case err == nil && polls.Load() == 1:
		outcome = singleFlightServed
	case err == nil && polls.Load() == 0:
		outcome = singleFlightCached
	case errors.Is(err, ErrCacheLockBusy) && polls.Load() == 0:
		outcome = singleFlightContended
	}
	if outcome == "" {
		t.Fatalf("%s%s polls=%d err=%v", singleFlightOutcomePrefix, "unexpected", polls.Load(), err)
	}
	fmt.Printf("%s%s polls=%d\n", singleFlightOutcomePrefix, outcome, polls.Load())
}

// The helper and its parent exchange outcomes through one stable line rather
// than through exit codes, so a scheduler-dependent-but-legal result cannot be
// mistaken for a failure, and an illegal one cannot hide behind exit 0.
const (
	singleFlightOutcomePrefix = "SINGLEFLIGHT-OUTCOME: "
	singleFlightServed        = "served"
	singleFlightCached        = "cached"
	singleFlightContended     = "contended"

	// Every owned subprocess is bounded. These are LEAK bounds, not the
	// mechanism any assertion depends on: the handshakes below decide the
	// outcome, and a budget being reached is reported as a failure of this
	// test rather than absorbed into the suite-wide timeout.
	helperProcessBudget     = 60 * time.Second
	helperResultBudget      = 45 * time.Second
	helperReapBudget        = 10 * time.Second
	holderReleaseFailSafe   = 90 * time.Second
	holderReadyBudget       = 30 * time.Second
	helperWaitDelayOnKill   = 5 * time.Second
	helperReadyPollInterval = 5 * time.Millisecond
)

// ownedHelper is one subprocess this test owns end to end.
//
// It exists because the previous shape leaked in two ways that a whole-suite
// timeout hid: CombinedOutput and the result channel were read without any
// deadline, so a t.Cleanup that killed processes could never run while the
// test was blocked on them, and Kill alone does not reap -- the temp directory
// could be removed while a child still had it open.
//
// Start is synchronous so cmd.Process is set before anything else can observe
// it, Wait is called exactly once by one goroutine, and every accessor waits on
// that single completion instead of calling Wait again.
// Completion is signalled by CLOSING finished, never by sending on it: a
// channel that carries the result can be drained only once, so cleanup would
// then block for its whole reap budget on every already-finished helper. waitErr
// is written before the close and read only after it, which is what makes the
// unsynchronised field safe.
type ownedHelper struct {
	name     string
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	output   *bytes.Buffer
	finished chan struct{}
	waitErr  error
	stopped  sync.Once
}

// startOwnedHelper launches a helper under a finite context and registers a
// cleanup that cancels it and WAITS for it to be reaped.
func startOwnedHelper(t *testing.T, name string, env []string) *ownedHelper {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), helperProcessBudget)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderCacheSubprocessHelper$", "-test.v")
	cmd.Env = env
	// Bound the join as well as the process: without this, Wait can outlive
	// the kill while an inherited pipe stays open.
	cmd.WaitDelay = helperWaitDelayOnKill
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	h := &ownedHelper{name: name, cmd: cmd, cancel: cancel, output: &buf, finished: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s helper: %v", name, err)
	}
	go func() {
		h.waitErr = cmd.Wait()
		close(h.finished)
	}()
	t.Cleanup(h.stop)
	return h
}

// wait blocks for the helper's completion within a finite budget and returns
// its combined output. Exceeding the budget is an ERROR naming the timeout, not
// a result: a child that had to be killed proves nothing about the guard under
// test.
func (h *ownedHelper) wait(budget time.Duration) (string, error) {
	select {
	case <-h.finished:
		// Safe to read the buffer and the error only after the single Wait
		// has returned, which the close above establishes.
		text := strings.TrimSpace(h.output.String())
		if h.waitErr != nil {
			return text, fmt.Errorf("%s helper failed: %w", h.name, h.waitErr)
		}
		return text, nil
	case <-time.After(budget):
		h.stop()
		return "", fmt.Errorf("%s helper did not finish within %s; killed and reaped, result discarded", h.name, budget)
	}
}

// exited reports whether the helper has already finished, without consuming its
// completion, so a caller can distinguish "still holding" from "died early".
func (h *ownedHelper) exited() bool {
	select {
	case <-h.finished:
		return true
	default:
		return false
	}
}

// stop cancels the process and waits for it to be reaped. Idempotent, and safe
// to call from cleanup after wait has already consumed the completion.
func (h *ownedHelper) stop() {
	h.stopped.Do(func() {
		h.cancel()
		select {
		case <-h.finished:
		case <-time.After(helperReapBudget):
		}
	})
}

// singleFlightOutcome extracts the reported outcome from one helper's output.
// An absent or unreadable line is a failure, never an assumed pass: a helper
// that printed nothing proves nothing.
func singleFlightOutcome(output string) (string, error) {
	found := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, singleFlightOutcomePrefix) {
			continue
		}
		value := strings.TrimPrefix(trimmed, singleFlightOutcomePrefix)
		if name, _, ok := strings.Cut(value, " "); ok {
			value = name
		}
		if found != "" && found != value {
			return "", fmt.Errorf("helper reported two different outcomes, %q then %q", found, value)
		}
		found = value
	}
	switch found {
	case singleFlightServed, singleFlightCached, singleFlightContended:
		return found, nil
	case "":
		return "", fmt.Errorf("helper reported no outcome line")
	default:
		return "", fmt.Errorf("helper reported an unrecognised outcome %q", found)
	}
}

// TestProviderBackoff429SurfacesStalePriorReadingNotAbsence is the FAC-786
// regression: a usage-endpoint 429 on a provider that previously had a good
// reading must degrade to an explicit stale reading (routing can still see
// the provider and its last known state), not vanish from the snapshot
// entirely and read as a fully unknown/absent provider. It also asserts the
// backoff bounds the poller to exactly one live call across repeated
// fetchProviderCached calls within the backoff window (no retry storm).
func TestProviderBackoff429SurfacesStalePriorReadingNotAbsence(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"stale-429-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls int32
	rateLimited := false
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){"codex": func() (ProviderUsage, error) {
		atomic.AddInt32(&calls, 1)
		if rateLimited {
			return ProviderUsage{}, pollErrf("rate-limited", "HTTP 429; retry-after=300")
		}
		return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Remaining: 80, WindowSeconds: 18000},
		}}, nil
	}})
	defer restore()
	InvalidateSnapshotCache()
	defer InvalidateSnapshotCache()

	// First call: healthy reading, persisted to disk.
	snap, err := fetchProviderCached("codex", false)
	if err != nil || snap == nil || len(snap.Providers["codex"].Resources) == 0 {
		t.Fatalf("expected a healthy first reading, got snap=%+v err=%v", snap, err)
	}

	// ttl=0 already forces the next call to re-poll upstream (no
	// InvalidateSnapshotCache: that helper drops non-backoff records
	// entirely, which would erase the prior good reading this test needs).
	rateLimited = true

	snap, err = fetchProviderCached("codex", false)
	if err == nil || pollErrorCode(err) != "rate-limited" {
		t.Fatalf("expected a rate-limited error on the 429 call, got snap=%+v err=%v", snap, err)
	}
	if snap == nil {
		t.Fatal("429 with a prior good reading must surface a stale snapshot, not nil")
	}
	provider, ok := snap.Providers["codex"]
	if !ok {
		t.Fatal("429 with a prior good reading dropped the provider entirely instead of degrading to stale")
	}
	if !provider.Stale {
		t.Fatal("provider surfaced during backoff must be marked Stale, never presented as a fresh reading")
	}
	if len(provider.Resources) == 0 {
		t.Fatal("stale provider lost its last known resource data")
	}

	// A second call still inside the backoff window must not hit the poller
	// again -- this is the bounded/single-flight guarantee.
	if _, err := fetchProviderCached("codex", false); err == nil || pollErrorCode(err) != "rate-limited" {
		t.Fatalf("expected the persisted backoff to still apply, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 live polls (1 success + 1 429), got %d -- backoff did not bound repeated calls", got)
	}
}

// TestFetchSnapshotCachedBoundsRepeatedCallsAcrossRouteInvocations is the
// FAC-786 regression for `herd route`: it used the uncached usage.FetchSnapshot,
// which hits every provider directly with no persisted backoff, so repeated
// route calls during a 429 hammered the provider endpoint unbounded and lost
// the prior stale reading each time. usage.FetchSnapshotCached shares the same
// per-provider backoff and stale fallback as the launch path.
func TestFetchSnapshotCachedBoundsRepeatedCallsAcrossRouteInvocations(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"route-429-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls int32
	rateLimited := false
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){"codex": func() (ProviderUsage, error) {
		atomic.AddInt32(&calls, 1)
		if rateLimited {
			return ProviderUsage{}, pollErrf("rate-limited", "HTTP 429; retry-after=300")
		}
		return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Remaining: 80, WindowSeconds: 18000},
		}}, nil
	}})
	defer restore()
	InvalidateSnapshotCache()
	defer InvalidateSnapshotCache()

	// First route call: healthy, cached to disk.
	if snap, _, err := FetchSnapshotCached(); err != nil || len(snap.Providers["codex"].Resources) == 0 {
		t.Fatalf("expected a healthy first snapshot, got snap=%+v err=%v", snap, err)
	}

	rateLimited = true

	// Three more "herd route" invocations in a row while the provider is
	// rate-limited must not each re-poll: the persisted backoff must bound
	// them to at most one additional live call, and the stale reading must
	// still be visible rather than the provider disappearing.
	for i := 0; i < 3; i++ {
		snap, _, err := FetchSnapshotCached()
		if snap == nil {
			t.Fatalf("route call %d: lost the snapshot entirely under backoff", i)
		}
		provider, ok := snap.Providers["codex"]
		if !ok {
			t.Fatalf("route call %d: provider dropped from snapshot under backoff (err=%v)", i, err)
		}
		if !provider.Stale {
			t.Fatalf("route call %d: provider surfaced under backoff must be marked stale", i)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 live polls (1 success + 1 429) across 4 route calls, got %d -- backoff did not bound repeated calls", got)
	}
}

func TestProviderCacheAcrossProcessesShares429Backoff(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	cachePath := filepath.Join(dir, "quota.json")
	t.Setenv("HERD_QUOTA_CACHE_PATH", cachePath)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"429-subprocess"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	env := append(os.Environ(), "HERD_CACHE_SUBPROCESS_HELPER=1", "HERD_CACHE_429_HELPER=1", "HERD_CACHE_FIXTURE_URL="+server.URL, "HERD_QUOTA_CACHE_PATH="+cachePath, "HOME="+home, "CODEX_HOME="+home)
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProviderCacheSubprocessHelper$", "-test.v")
		cmd.Env = env
		go func() {
			output, err := cmd.CombinedOutput()
			if err != nil {
				results <- fmt.Errorf("429 helper failed: %w: %s", err, strings.TrimSpace(string(output)))
				return
			}
			results <- nil
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("concurrent 429 callers made %d upstream requests, want 1", requests.Load())
	}
	var cached cachedSnapshot
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatal(err)
	}
	if !cached.Providers["codex"].BackoffUntil.After(time.Now()) {
		t.Fatal("concurrent 429 did not persist a future cooldown")
	}
	// FAC-818: the bound here is ONE backoff window, not the old flat 15s
	// default (the floor is now the success TTL, and the helper's
	// retry-after=15 no longer undercuts it). What this still proves is the
	// single-flight property it was written for: the waiting caller must not
	// write a second record and push the deadline out to a second window.
	if cached.Providers["codex"].BackoffUntil.After(time.Now().Add(2 * defaultSnapshotTTL)) {
		t.Fatal("cooldown deadline was extended by the waiting caller")
	}
	if got := cached.Providers["codex"].FailureStreak; got != 1 {
		t.Fatalf("concurrent 429 callers recorded a failure streak of %d, want 1: the waiting caller escalated a failure it never observed", got)
	}
}

func TestProviderLocksAreScopedAndBoundedAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	cachePath := filepath.Join(dir, "quota.json")
	t.Setenv("HERD_QUOTA_CACHE_PATH", cachePath)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"lock-scope"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "codex-holder.ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProviderCacheSubprocessHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "HERD_CACHE_SUBPROCESS_HELPER=1", "HERD_CACHE_HOLD_PROVIDER=codex", "HERD_CACHE_HOLDER_READY="+ready, "HERD_QUOTA_CACHE_PATH="+cachePath, "HOME="+home, "CODEX_HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait for the child to acquire the provider lock, rather than measuring
	// unrelated provider latency against process compilation/startup time.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("provider-lock holder did not complete deterministic ready handshake")
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := 0
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){"gemini": func() (ProviderUsage, error) {
		calls++
		return ProviderUsage{DisplayName: "Gemini", Resources: map[string]ResourceUsage{"weekly": {Remaining: 50}}}, nil
	}})
	started := time.Now()
	_, err := FetchProviderForce("gemini", false)
	elapsed := time.Since(started)
	restore()
	if err != nil || calls != 1 {
		t.Fatalf("unrelated provider was blocked by Codex holder: err=%v calls=%d", err, calls)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("unrelated provider waited on global holder: %v", elapsed)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder helper failed: %v", err)
	}
}

func TestInvalidatePreservesBackoffAndDoesNotTouchFixtureSentinel(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	sentinel := filepath.Join(home, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	account := &AccountIdentity{Key: "sentinel-account", Provenance: "fixture"}
	if err := mergeSnapshotFile(&UsageSnapshot{Errors: map[string]string{"codex": "rate-limited: HTTP 429; retry-after=60"}}, &cachedProviderRecord{AccountKey: account.Key, BackoffUntil: time.Now().Add(time.Minute), Error: "rate-limited: HTTP 429; retry-after=60"}); err != nil {
		t.Fatal(err)
	}
	if err := InvalidateSnapshotCacheWithError(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("fixture invalidation touched HOME sentinel: %v", err)
	}
	var cached cachedSnapshot
	body, err := os.ReadFile(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatal(err)
	}
	if !cached.Providers["codex"].BackoffUntil.After(time.Now()) {
		t.Fatal("invalidation erased persisted rate-limit cooldown")
	}
}

func TestProviderCacheAcrossProcessesSingleFlight(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"subprocess-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	baseEnv := append(os.Environ(),
		"HERD_CACHE_SUBPROCESS_HELPER=1",
		"HERD_CACHE_FIXTURE_URL="+server.URL,
		"HERD_QUOTA_CACHE_PATH="+filepath.Join(dir, "quota.json"),
		"HOME="+home,
		"CODEX_HOME="+home,
		"HERD_QUOTA_CACHE_SECONDS=45",
	)
	// Each helper is owned: started synchronously so its process handle exists
	// before anything observes it, waited on exactly once, and cancelled and
	// REAPED by cleanup before the temp directories are removed.
	helpers := []*ownedHelper{
		startOwnedHelper(t, "racer-a", baseEnv),
		startOwnedHelper(t, "racer-b", baseEnv),
	}

	// Collect BOTH results, each within its own finite budget, before
	// asserting anything. Returning early on the first failure would leave the
	// second helper's outcome unread and its process competing with the next
	// test for the same cache path. A helper that exceeds its budget is killed
	// and its result DISCARDED -- a child that had to be killed is not evidence
	// about the guard under test.
	outcomes := make([]string, 0, len(helpers))
	var failures []error
	for _, h := range helpers {
		text, err := h.wait(helperResultBudget)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		outcome, parseErr := singleFlightOutcome(text)
		if parseErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w: %s", h.name, parseErr, text))
			continue
		}
		outcomes = append(outcomes, outcome)
	}
	if len(failures) > 0 {
		t.Fatalf("helper(s) failed: %v", errors.Join(failures...))
	}

	// The single-flight contract, asserted on what was OBSERVED rather than on
	// which process happened to be scheduled first: exactly one live poll
	// reached the provider, exactly one caller performed it, and the other
	// caller polled nothing -- whether it read the winner's cache or waited its
	// bounded budget out. Neither outcome is a retry, a skip, or a longer
	// timeout; they are the two legal ways to lose the race.
	if got := requests.Load(); got != 1 {
		t.Fatalf("cross-process single-flight allowed %d fixture requests, want exactly 1 (outcomes %v)", got, outcomes)
	}
	served := 0
	for _, outcome := range outcomes {
		if outcome == singleFlightServed {
			served++
		}
	}
	if served != 1 {
		t.Fatalf("expected exactly one caller to perform the poll, got %d (outcomes %v)", served, outcomes)
	}
	for _, outcome := range outcomes {
		switch outcome {
		case singleFlightServed, singleFlightCached, singleFlightContended:
		default:
			t.Fatalf("unrecognised single-flight outcome %q (outcomes %v)", outcome, outcomes)
		}
	}
}

// FAC-679: the cache exists because a live quota fetch ran before EVERY review
// launch and took 29-272 seconds while the launch itself was 1.4s of work. But a
// STALE quota read routes work to an exhausted surface, which is worse than
// being slow -- so the TTL is short and staleness is reported, never hidden.
func TestSnapshotTTLIsShortAndOverridable(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "")
	if got := snapshotTTL(); got != 45*time.Second {
		t.Errorf("default TTL = %v, want 45s: long enough to collapse one dispatch beat, short enough that quota cannot go stale unnoticed", got)
	}
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "5")
	if got := snapshotTTL(); got != 5*time.Second {
		t.Errorf("override = %v, want 5s", got)
	}
	// Zero disables reuse entirely, so an operator who wants every decision live
	// can have that without editing code.
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	if got := snapshotTTL(); got != 0 {
		t.Errorf("zero must disable reuse, got %v", got)
	}
	// A malformed value falls back to the safe default rather than to zero-or-
	// forever, either of which would be a surprise.
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "not-a-number")
	if got := snapshotTTL(); got != 45*time.Second {
		t.Errorf("malformed override must fall back to the default, got %v", got)
	}
}

// A held reading is reused only while it is young, and its AGE is returned so a
// caller can report what it acted on instead of implying the number was live.
func TestCachedSnapshotReportsItsAge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(t.TempDir(), "quota.json"))
	InvalidateSnapshotCache()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"cache-age-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	account := codexAccountIdentity()
	quotaCache.Lock()
	quotaCache.snap = &UsageSnapshot{Providers: map[string]ProviderUsage{"codex": {
		Account: account, ObservedAt: time.Now().Add(-2 * time.Second),
		Resources: map[string]ResourceUsage{"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Remaining: 90}},
	}}}
	quotaCache.fetchedAt = time.Now().Add(-2 * time.Second)
	quotaCache.Unlock()

	snap, age, err := FetchSnapshotCached()
	if err != nil || snap == nil {
		t.Fatalf("a young reading must be reused: %v", err)
	}
	if age < time.Second {
		t.Errorf("the age must be reported so a caller can say what it used, got %v", age)
	}
}

func TestProviderCachedReusesFreshNativeObservation(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(t.TempDir(), "quota.json"))
	InvalidateSnapshotCache()
	calls := 0
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"cache-provider-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := nativePollers["codex"]
	nativePollers["codex"] = func() (ProviderUsage, error) {
		calls++
		return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{
			"weekly": {Kind: "consumption", Unit: "percent", Limit: 100, Remaining: 80, WindowSeconds: 604800},
		}}, nil
	}
	t.Cleanup(func() { nativePollers["codex"] = old; InvalidateSnapshotCache() })
	if _, err := FetchProviderForce("codex", false); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchProviderForce("codex", false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fresh provider observation was polled %d times, want singleflight/cache reuse", calls)
	}
}

func TestProviderCacheRetainsProviderAgeAndSeparatesAccounts(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	InvalidateSnapshotCache()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"account-one"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeUUID := "claude-cache-account"
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"`+claudeUUID+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeAccount := identity("claude", claudeUUID, "claude-profile:test")
	var codexCalls, claudeCalls int
	old := nativePollers
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){
		"claude": func() (ProviderUsage, error) {
			claudeCalls++
			return ProviderUsage{DisplayName: "Claude", Account: claudeAccount, ObservedAt: time.Now().Add(-2 * time.Second), Resources: map[string]ResourceUsage{"weekly": {Kind: "consumption", Unit: "percent", Remaining: 80}}}, nil
		},
		"codex": func() (ProviderUsage, error) {
			codexCalls++
			return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{"weekly": {Kind: "consumption", Unit: "percent", Remaining: 70}}}, nil
		},
	})
	t.Cleanup(func() { restore(); nativePollers = old; InvalidateSnapshotCache() })
	if _, err := FetchProviderForce("claude", false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	var before cachedSnapshot
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	claudeObserved := before.Providers["claude"].ObservedAt
	if _, err := FetchProviderForce("codex", false); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "quota.json"))
	var after cachedSnapshot
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Providers["claude"]; !ok {
		t.Fatal("scoped Codex success evicted the retained Claude record")
	}
	if !after.Providers["claude"].ObservedAt.Equal(claudeObserved) {
		t.Fatal("scoped success reset the retained provider observation age")
	}
	if _, err := FetchProviderForce("claude", false); err != nil {
		t.Fatal(err)
	}
	if claudeCalls != 1 || codexCalls != 1 {
		t.Fatalf("alternating providers repolled unexpectedly: claude=%d codex=%d", claudeCalls, codexCalls)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"account-two"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchProviderForce("codex", false); err != nil {
		t.Fatal(err)
	}
	if codexCalls != 2 {
		t.Fatalf("credential/account switch reused old Codex cache: calls=%d", codexCalls)
	}
}

func TestProviderCacheRejectsClockSkewAndPersists429Backoff(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	InvalidateSnapshotCache()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"backoff-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	account := codexAccountIdentity()
	future := cachedSnapshot{Providers: map[string]cachedProviderRecord{"codex": {ObservedAt: time.Now().Add(time.Hour), AccountKey: account.Key, Provider: ProviderUsage{Account: account}}}}
	body, _ := json.Marshal(future)
	if err := os.WriteFile(filepath.Join(dir, "quota.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Fatal("future provider observation was accepted")
	}
	calls := 0
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){"codex": func() (ProviderUsage, error) {
		calls++
		return ProviderUsage{}, pollErrf("rate-limited", "HTTP 429 retry-after=60")
	}})
	defer restore()
	if _, err := FetchProviderForce("codex", false); err == nil || pollErrorCode(err) != "rate-limited" {
		t.Fatalf("first 429 error = %v, want rate-limited", err)
	}
	if _, err := FetchProviderForce("codex", true); err == nil || pollErrorCode(err) != "rate-limited" {
		t.Fatalf("forced request bypassed persisted 429 backoff: %v", err)
	}
	if calls != 1 {
		t.Fatalf("persisted 429 backoff allowed %d upstream polls, want 1", calls)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cached cachedSnapshot
	if err := json.Unmarshal(raw, &cached); err != nil {
		t.Fatal(err)
	}
	record := cached.Providers["codex"]
	if !record.BackoffUntil.After(time.Now()) {
		t.Fatal("429 backoff was not persisted")
	}
	record.BackoffUntil = time.Now().Add(20 * time.Millisecond)
	cached.Providers["codex"] = record
	raw, _ = json.Marshal(cached)
	if err := os.WriteFile(filepath.Join(dir, "quota.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := FetchProviderForce("codex", false); err == nil || pollErrorCode(err) != "rate-limited" {
		t.Fatalf("expired 429 backoff did not surface the new provider response: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expired backoff did not permit exactly one new poll: calls=%d", calls)
	}
}

func TestProviderCacheKeepsFreshPositiveReadingDuring429Backoff(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"fresh-429-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){"codex": func() (ProviderUsage, error) {
		calls++
		if calls == 1 {
			return ProviderUsage{Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{"primary": {Unit: "percent", Used: 20, Remaining: 80, Limit: 100, WindowSeconds: Window5h}}}, nil
		}
		return ProviderUsage{}, pollErrf("rate-limited", "HTTP 429 retry-after=60")
	}})
	defer restore()
	quotaCache.Lock()
	quotaCache.snap, quotaCache.fetchedAt = nil, time.Time{}
	quotaCache.Unlock()
	if _, err := FetchProviderForce("codex", false); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchProviderForce("codex", true); err == nil || pollErrorCode(err) != "rate-limited" {
		t.Fatalf("forced refresh should expose typed 429, got %v", err)
	}
	quotaCache.Lock()
	quotaCache.snap, quotaCache.fetchedAt = nil, time.Time{}
	quotaCache.Unlock()
	snap, err := FetchProviderForce("codex", false)
	if err != nil || snap == nil || calls != 2 {
		t.Fatalf("fresh positive quota was lost during 429 backoff: snap=%+v err=%v calls=%d", snap, err, calls)
	}
	if got := snap.Providers["codex"].Resources["primary"].Remaining; got != 80 {
		t.Fatalf("fresh positive quota changed during 429 backoff: %v", got)
	}
}

func TestRateLimitBackoffHonorsLongAndHTTPDateRetryAfter(t *testing.T) {
	if got := rateLimitBackoff(pollErrf("rate-limited", "HTTP 429 retry-after=601"), 1); got != 601*time.Second {
		t.Fatalf("long Retry-After was truncated: got %v", got)
	}
	// FAC-818: the backoff now floors at the success TTL, so a Retry-After
	// SHORTER than the floor can no longer prove the header was read at all.
	// Use a date beyond the floor, and assert it against the floor, so this
	// stays a real test of Retry-After parsing rather than a tautology.
	date := time.Now().Add(defaultSnapshotTTL + 90*time.Second).UTC().Format(http.TimeFormat)
	got := rateLimitBackoff(pollErrf("rate-limited", "%s", "HTTP 429 retry-after="+date), 1)
	if got <= defaultSnapshotTTL {
		t.Fatalf("HTTP-date Retry-After was not honored: got %v, want more than the %v floor", got, defaultSnapshotTTL)
	}
	if got > defaultSnapshotTTL+91*time.Second {
		t.Fatalf("HTTP-date Retry-After was inflated: got %v", got)
	}
}

func TestCacheConcurrentAtomicWritesRetainBothProviders(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	write := func(provider string) error {
		return withSnapshotFileLock(func() error {
			return mergeSnapshotFile(&UsageSnapshot{Providers: map[string]ProviderUsage{
				provider: {DisplayName: provider, Account: &AccountIdentity{Key: provider + "-account", Provenance: "fixture"}, ObservedAt: time.Now().UTC(), Resources: map[string]ResourceUsage{"weekly": {Remaining: 50}}},
			}}, nil)
		})
	}
	results := make(chan error, 2)
	go func() { results <- write("claude") }()
	go func() { results <- write("codex") }()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cached cachedSnapshot
	if err := json.Unmarshal(raw, &cached); err != nil {
		t.Fatalf("concurrent writes left invalid JSON: %v", err)
	}
	if len(cached.Providers) != 2 {
		t.Fatalf("concurrent writes lost a provider: got %d records", len(cached.Providers))
	}
}

func TestMergeSnapshotMigratesEmptyAndLegacyCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	for _, raw := range []string{"{}", `{"providers":null}`} {
		if err := os.WriteFile(filepath.Join(dir, "quota.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := mergeSnapshotFile(&UsageSnapshot{Providers: map[string]ProviderUsage{
			"codex": {Account: &AccountIdentity{Key: "fixture", Provenance: "fixture"}, ObservedAt: time.Now().UTC()},
		}}, nil); err != nil {
			t.Fatalf("empty cache migration failed for %s: %v", raw, err)
		}
		var got cachedSnapshot
		body, _ := os.ReadFile(filepath.Join(dir, "quota.json"))
		if err := json.Unmarshal(body, &got); err != nil || got.Providers == nil {
			t.Fatalf("migration did not write a nonnil provider map for %s", raw)
		}
	}
	observed := time.Now().Add(-2 * time.Second).UTC()
	legacy := &UsageSnapshot{Providers: map[string]ProviderUsage{
		"codex": {Account: &AccountIdentity{Key: "legacy", Provenance: "fixture"}, ObservedAt: observed},
	}}
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: observed, Snapshot: legacy})
	if err := os.WriteFile(filepath.Join(dir, "quota.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeSnapshotFile(&UsageSnapshot{Providers: map[string]ProviderUsage{"claude": {ObservedAt: time.Now().UTC()}}}, nil); err != nil {
		t.Fatal(err)
	}
	var migrated cachedSnapshot
	body, _ = os.ReadFile(filepath.Join(dir, "quota.json"))
	if err := json.Unmarshal(body, &migrated); err != nil {
		t.Fatal(err)
	}
	if !migrated.Providers["codex"].ObservedAt.Equal(observed) || migrated.Providers["codex"].AccountKey != "legacy" {
		t.Fatal("legacy provider age or account binding was not preserved")
	}
}

func TestScopedThenAllUsesFreshProviderWithoutRepolling(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	InvalidateSnapshotCache()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"scoped-all"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var codexCalls, claudeCalls int
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){
		"codex": func() (ProviderUsage, error) {
			codexCalls++
			return ProviderUsage{Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{"weekly": {Remaining: 50}}}, nil
		},
		"claude": func() (ProviderUsage, error) {
			claudeCalls++
			return ProviderUsage{Account: identity("claude", "scoped-claude", "claude-profile:fixture"), Resources: map[string]ResourceUsage{"weekly": {Remaining: 50}}}, nil
		},
	})
	defer restore()
	if _, err := FetchProviderForce("codex", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := FetchSnapshotCachedForce(false); err != nil {
		t.Fatal(err)
	}
	if codexCalls != 1 || claudeCalls != 1 {
		t.Fatalf("scoped-then-all repolled or skipped providers: codex=%d claude=%d", codexCalls, claudeCalls)
	}
}

// Invalidation forces the next decision to refetch, so anything that materially
// changes quota is not followed by a decision acting on the number it just
// invalidated.
func TestInvalidateForcesARefetch(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(t.TempDir(), "quota.json"))
	quotaCache.Lock()
	quotaCache.snap = &UsageSnapshot{}
	quotaCache.fetchedAt = time.Now()
	quotaCache.Unlock()

	InvalidateSnapshotCache()

	quotaCache.Lock()
	held := quotaCache.snap
	quotaCache.Unlock()
	if held != nil {
		t.Fatal("invalidation must drop the held reading")
	}
}

// FAC-679 (second pass): the in-process cache alone did nothing for the case that
// reported the problem. Every `herd review` is its OWN process, so a memory cache
// collapses fetches within one launch and helps not at all across launches --
// which is exactly where the 29-272 seconds were spent. Caught by measuring two
// consecutive launches and seeing no improvement the cache could account for.
//
// FAC-786: a persisted reading is only reusable when its account identity is
// provable, so this test pins a known account into an isolated HOME and binds
// the cached reading to the same account.
func TestPersistedReadingSurvivesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "q.json"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(key, "")
	}
	const accountUUID = "11111111-2222-3333-4444-555555555555"
	if err := os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"oauthAccount":{"accountUuid":"`+accountUUID+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	acc := &AccountIdentity{
		Key:        opaqueAccountKey("claude", accountUUID),
		Provenance: "claude-profile:api.anthropic.com/api/oauth/profile:account.uuid",
	}
	writeSnapshotFile(&UsageSnapshot{Providers: map[string]ProviderUsage{
		"claude": {DisplayName: "Claude", Account: acc, Stale: false,
			Resources: map[string]ResourceUsage{
				"weekly": {Kind: "consumption", Unit: "percent", Used: 10, Remaining: 90, Limit: 100, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800},
			}},
	}})
	snap, age, ok := readSnapshotFile(45 * time.Second)
	if !ok || snap == nil {
		t.Fatal("a profile-bound Claude reading must be reusable for the matching current account")
	}
	_ = age
	_ = acc
}

// Aged-out, corrupt and missing readings all fetch live. A cache is an
// optimisation and must never be a source of truth it cannot prove.
func TestUnusableReadingsFallThroughToLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.json")
	t.Setenv("HERD_QUOTA_CACHE_PATH", path)

	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a missing reading must not be usable")
	}
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a corrupt reading must not be usable")
	}
	// Aged out.
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: time.Now().Add(-time.Hour), Snapshot: &UsageSnapshot{}})
	os.WriteFile(path, body, 0o600)
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a reading older than the TTL must not be usable")
	}
	// A clock that moved backwards yields a negative age; that is unusable, not
	// infinitely fresh.
	body, _ = json.Marshal(cachedSnapshot{FetchedAt: time.Now().Add(time.Hour), Snapshot: &UsageSnapshot{}})
	os.WriteFile(path, body, 0o600)
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a future timestamp must be treated as unusable, never as fresh")
	}
}

// TestProviderCacheContendedLockIsBusyNotAPoll pins the losing half of
// single-flight deterministically, without racing the scheduler.
//
// A holder subprocess takes the provider lock and announces it through a ready
// file, so the contender does not start until the lock is provably held. The
// holder then keeps it for a full second while the contender's wait is bounded
// at 300ms, which makes the outcome determined rather than timing-dependent:
// the contender MUST come back busy. What matters is what it does about it --
// it must report the busy lock as such and must not poll the provider behind
// the holder's back.
func TestProviderCacheContendedLockIsBusyNotAPoll(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"contended-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	ready := filepath.Join(dir, "holder.ready")
	baseEnv := append(os.Environ(),
		"HERD_CACHE_SUBPROCESS_HELPER=1",
		"HERD_CACHE_FIXTURE_URL="+server.URL,
		"HERD_QUOTA_CACHE_PATH="+filepath.Join(dir, "quota.json"),
		"HOME="+home,
		"CODEX_HOME="+home,
		"HERD_QUOTA_CACHE_SECONDS=45",
	)
	release := filepath.Join(dir, "holder.release")
	holder := startOwnedHelper(t, "holder", append(append([]string{}, baseEnv...),
		"HERD_CACHE_HOLD_PROVIDER=codex",
		"HERD_CACHE_HOLDER_READY="+ready,
		"HERD_CACHE_HOLDER_RELEASE="+release,
	))
	// The release file is written on EVERY exit path, including a Fatal
	// below, so the holder is never left waiting on its fail-safe. Cleanup
	// registered after the helper runs BEFORE the helper's own cleanup, so the
	// release lands before the reap wait rather than after it.
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("release\n"), 0o600) })

	// Bounded wait for the announcement, polled rather than slept: the
	// contender must not start before the lock is provably held, and a holder
	// that never announces is a failure rather than a silent pass.
	deadline := time.Now().Add(holderReadyBudget)
	held := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			held = true
			break
		}
		if holder.exited() {
			text, err := holder.wait(helperReapBudget)
			t.Fatalf("holder exited before announcing the lock: %v: %s", err, text)
		}
		time.Sleep(helperReadyPollInterval)
	}
	if !held {
		t.Fatalf("holder never announced that it held the provider lock within %s", holderReadyBudget)
	}

	// The holder now stays locked until THIS test releases it, which happens
	// only after the contender's result is in hand. The hold therefore spans
	// the contender's entire attempt by construction. A fixed hold interval
	// could not promise that: it can expire between the ready file appearing
	// and the contender reaching the lock, and the contender would then
	// legitimately succeed while this test called it a failure.
	contender := startOwnedHelper(t, "contender", baseEnv)
	text, waitErr := contender.wait(helperResultBudget)
	if waitErr != nil {
		t.Fatalf("contender failed instead of reporting a busy lock: %v: %s", waitErr, text)
	}
	outcome, parseErr := singleFlightOutcome(text)
	if parseErr != nil {
		t.Fatalf("%v: %s", parseErr, text)
	}
	if outcome != singleFlightContended {
		t.Fatalf("contender reported %q while the lock was held for its whole attempt, want %q",
			outcome, singleFlightContended)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("a contender that could not take the lock still polled the provider %d time(s), want 0", got)
	}

	// Release explicitly and confirm the holder exits cleanly. A holder that
	// hit its fail-safe reports an error here rather than passing silently.
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	if holderText, err := holder.wait(helperResultBudget); err != nil {
		t.Fatalf("holder failed after release: %v: %s", err, holderText)
	}
}
