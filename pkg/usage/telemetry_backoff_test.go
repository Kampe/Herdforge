package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// coldStartEnv points the cache and credential lookup at test-owned paths and
// returns the counted poller's call count.
func coldStartEnv(t *testing.T, poll func() (ProviderUsage, error)) *int32 {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"telemetry-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int32
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){
		"codex": func() (ProviderUsage, error) {
			atomic.AddInt32(&calls, 1)
			return poll()
		},
	})
	t.Cleanup(restore)
	t.Cleanup(func() { InvalidateSnapshotCache() })
	InvalidateSnapshotCache()
	return &calls
}

// TestColdStart429KeepsTelemetryIdentity is the FAC-818 root regression. A
// provider whose FIRST poll of a cache generation is rate-limited has no
// banked reading, so staleBackoffSnapshot has nothing to surface and the
// provider vanished from ComputeAll entirely -- byte-identical to a provider
// nobody ever heard of. "The usage endpoint is throttling us" and "this
// provider does not exist" must not be the same observation.
func TestColdStart429KeepsTelemetryIdentity(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	coldStartEnv(t, func() (ProviderUsage, error) {
		return ProviderUsage{}, pollErrf("rate-limited", "codex usage: HTTP 429; retry-after=0")
	})

	snap, _, _ := FetchSnapshotCached()
	if snap == nil {
		t.Fatal("cold-start 429 produced no snapshot at all")
	}
	if detail := snap.Errors["codex"]; detail == "" {
		t.Fatal("cold-start 429 lost the classified poll error from the snapshot")
	}

	computed := NewQuotaEngine().ComputeAll(snap)
	st, ok := computed["codex"]
	if !ok {
		t.Fatal("cold-start 429 dropped the provider from ComputeAll; telemetry throttling is indistinguishable from an unknown provider")
	}
	if st.Cause == "" {
		t.Fatal("cold-start telemetry failure carries no machine-readable cause")
	}
	if st.Available {
		t.Fatal("a provider with no reading must never be reported available")
	}
	// Never invent quota: no reading means no numbers.
	if st.Used != 0 || st.Remaining != 0 || st.Class == BurnExhausted {
		t.Fatalf("telemetry-unavailable state fabricated quota numbers: %+v", st)
	}
	// Routing gate semantics must be unchanged: "we could not read quota" is
	// still no-quota-data, so the read path keeps passing the provider through
	// exactly as it did when the provider was absent.
	if st.Reason != reasonNoQuotaData {
		t.Fatalf("telemetry-unavailable changed the routing gate reason to %q; the read path would start refusing the provider", st.Reason)
	}
}

// TestRateLimitBackoffFloorsAtTTLAndEscalates pins the exact durations. The
// live defect: an Anthropic 429 carrying "retry-after=0" fell through to a
// flat 15s default while the success TTL is 45s, so a rate-limited provider
// was re-polled three times MORE often than a healthy one.
func TestRateLimitBackoffFloorsAtTTLAndEscalates(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "")
	zero := pollErrf("rate-limited", "claude usage: HTTP 429; retry-after=0")
	absent := pollErrf("rate-limited", "claude usage: HTTP 429; retry-after=unspecified")
	for _, tc := range []struct {
		name   string
		err    error
		streak int
		want   time.Duration
	}{
		{"zero-header first failure floors at the success TTL", zero, 1, defaultSnapshotTTL},
		{"absent header first failure floors at the success TTL", absent, 1, defaultSnapshotTTL},
		{"second failure doubles", zero, 2, 2 * defaultSnapshotTTL},
		{"third failure doubles again", zero, 3, 4 * defaultSnapshotTTL},
		{"escalation is capped", zero, 40, maxRateLimitBackoff},
	} {
		if got := rateLimitBackoff(tc.err, tc.streak); got != tc.want {
			t.Errorf("%s: rateLimitBackoff(streak=%d) = %v, want %v", tc.name, tc.streak, got, tc.want)
		}
	}
	// A greater valid Retry-After is authoritative and wins over the floor.
	long := pollErrf("rate-limited", "claude usage: HTTP 429; retry-after=600")
	if got := rateLimitBackoff(long, 1); got != 600*time.Second {
		t.Errorf("a greater valid Retry-After must be honored: got %v, want 10m", got)
	}
	// A smaller valid Retry-After must not undercut the floor.
	short := pollErrf("rate-limited", "claude usage: HTTP 429; retry-after=1")
	if got := rateLimitBackoff(short, 1); got != defaultSnapshotTTL {
		t.Errorf("a Retry-After below the floor must not undercut it: got %v, want %v", got, defaultSnapshotTTL)
	}
}

// expireBackoff simulates the clock reaching the persisted deadline without a
// real sleep: it rewinds only BackoffUntil on the persisted record, leaving the
// streak, account key and any banked reading exactly as production wrote them.
// This is the clock fixture the exact-poll-count assertions depend on.
func expireBackoff(t *testing.T, name string) cachedProviderRecord {
	t.Helper()
	var record cachedProviderRecord
	if err := withSnapshotFileLock(func() error {
		raw, err := os.ReadFile(snapshotCachePath())
		if err != nil {
			return err
		}
		var c cachedSnapshot
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		r, ok := c.Providers[name]
		if !ok {
			t.Fatalf("no persisted record for %q to expire", name)
		}
		record = r
		r.BackoffUntil = time.Now().Add(-time.Second)
		c.Providers[name] = r
		return writeCachedRecords(c.Providers)
	}); err != nil {
		t.Fatal(err)
	}
	quotaCache.Lock()
	quotaCache.snap, quotaCache.fetchedAt = nil, time.Time{}
	quotaCache.Unlock()
	return record
}

func persistedRecord(t *testing.T, name string) cachedProviderRecord {
	t.Helper()
	record, ok := readProviderRecord(name)
	if !ok {
		t.Fatalf("no persisted record for %q", name)
	}
	return record
}

// TestConsecutive429sPollExactlyOncePerWindowAndEscalate is the amplification
// regression. With a zero or absent Retry-After the provider must be polled
// EXACTLY once per backoff window -- never per caller -- and each consecutive
// failure must widen the next window.
func TestConsecutive429sPollExactlyOncePerWindowAndEscalate(t *testing.T) {
	for _, header := range []string{"retry-after=0", "retry-after=unspecified"} {
		t.Run(header, func(t *testing.T) {
			t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
			calls := coldStartEnv(t, func() (ProviderUsage, error) {
				return ProviderUsage{}, pollErrf("rate-limited", "codex usage: HTTP 429; %s", header)
			})

			// Window 1: five callers, exactly one live poll.
			for i := 0; i < 5; i++ {
				if _, err := fetchProviderCached("codex", false); err == nil {
					t.Fatalf("caller %d: rate-limited provider reported success", i)
				}
			}
			if got := atomic.LoadInt32(calls); got != 1 {
				t.Fatalf("window 1: want exactly 1 live poll for 5 callers, got %d", got)
			}
			first := persistedRecord(t, "codex")
			if first.FailureStreak != 1 {
				t.Fatalf("first failure streak = %d, want 1", first.FailureStreak)
			}
			if window := time.Until(first.BackoffUntil); window < defaultSnapshotTTL-time.Second {
				t.Fatalf("first window %v is shorter than the %v success TTL", window, defaultSnapshotTTL)
			}

			// Window 2: the deadline lapses, exactly one more poll, wider window.
			expireBackoff(t, "codex")
			for i := 0; i < 5; i++ {
				if _, err := fetchProviderCached("codex", false); err == nil {
					t.Fatalf("window 2 caller %d: rate-limited provider reported success", i)
				}
			}
			if got := atomic.LoadInt32(calls); got != 2 {
				t.Fatalf("window 2: want exactly 2 live polls in total, got %d", got)
			}
			second := persistedRecord(t, "codex")
			if second.FailureStreak != 2 {
				t.Fatalf("second failure streak = %d, want 2", second.FailureStreak)
			}
			if got := rateLimitBackoff(pollErrf("rate-limited", "codex usage: HTTP 429; %s", header), second.FailureStreak); got != 2*defaultSnapshotTTL {
				t.Fatalf("second window did not escalate: %v", got)
			}
		})
	}
}

// TestSuccessfulPollResetsFailureStreak: escalation must be a response to a
// CURRENT streak, not a permanent penalty.
func TestSuccessfulPollResetsFailureStreak(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	rateLimited := true
	coldStartEnv(t, func() (ProviderUsage, error) {
		if rateLimited {
			return ProviderUsage{}, pollErrf("rate-limited", "codex usage: HTTP 429; retry-after=0")
		}
		return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 20, Remaining: 80, WindowSeconds: 18000},
		}}, nil
	})

	if _, err := fetchProviderCached("codex", false); err == nil {
		t.Fatal("expected the first 429")
	}
	expireBackoff(t, "codex")
	if _, err := fetchProviderCached("codex", false); err == nil {
		t.Fatal("expected the second 429")
	}
	if got := persistedRecord(t, "codex").FailureStreak; got != 2 {
		t.Fatalf("streak before recovery = %d, want 2", got)
	}

	expireBackoff(t, "codex")
	rateLimited = false
	if _, err := fetchProviderCached("codex", false); err != nil {
		t.Fatalf("recovery poll failed: %v", err)
	}
	if got := persistedRecord(t, "codex").FailureStreak; got != 0 {
		t.Fatalf("a successful poll left the failure streak at %d, want 0", got)
	}

	// The next failure must start over at the floor, not resume the escalation.
	rateLimited = true
	if _, err := fetchProviderCached("codex", false); err == nil {
		t.Fatal("expected a 429 after recovery")
	}
	if got := persistedRecord(t, "codex").FailureStreak; got != 1 {
		t.Fatalf("streak after recovery+failure = %d, want 1", got)
	}
}

// TestAccountSwitchResetsFailureStreak: a different credential is a different
// rate limit and must not inherit the previous account's penalty.
func TestAccountSwitchResetsFailureStreak(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	coldStartEnv(t, func() (ProviderUsage, error) {
		return ProviderUsage{}, pollErrf("rate-limited", "codex usage: HTTP 429; retry-after=0")
	})
	if _, err := fetchProviderCached("codex", false); err == nil {
		t.Fatal("expected the first 429")
	}
	expireBackoff(t, "codex")
	if _, err := fetchProviderCached("codex", false); err == nil {
		t.Fatal("expected the second 429")
	}
	before := persistedRecord(t, "codex")
	if before.FailureStreak != 2 || before.AccountKey == "" {
		t.Fatalf("precondition: streak=%d accountKey=%q", before.FailureStreak, before.AccountKey)
	}

	// Swap the credential on disk; the account key changes with it.
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"),
		[]byte(`{"tokens":{"account_id":"telemetry-acct-rotated"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if currentProviderAccountKey("codex") == before.AccountKey {
		t.Fatal("fixture did not actually rotate the account key")
	}
	expireBackoff(t, "codex")
	if _, err := fetchProviderCached("codex", false); err == nil {
		t.Fatal("expected a 429 for the rotated account")
	}
	after := persistedRecord(t, "codex")
	if after.FailureStreak != 1 {
		t.Fatalf("rotated account inherited the prior account's streak: %d, want 1", after.FailureStreak)
	}
	if after.AccountKey == before.AccountKey {
		t.Fatal("record still carries the previous account key")
	}
}

// TestLastRealReadingRetainedAcrossConsecutive429s: escalation must not cost
// the stale-reading fallback. The banked reading has to survive every
// subsequent failure, not just the first.
func TestLastRealReadingRetainedAcrossConsecutive429s(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	rateLimited := false
	coldStartEnv(t, func() (ProviderUsage, error) {
		if rateLimited {
			return ProviderUsage{}, pollErrf("rate-limited", "codex usage: HTTP 429; retry-after=0")
		}
		return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 20, Remaining: 80, WindowSeconds: 18000},
		}}, nil
	})
	if _, err := fetchProviderCached("codex", false); err != nil {
		t.Fatalf("healthy first reading: %v", err)
	}
	rateLimited = true
	for i := 1; i <= 3; i++ {
		snap, err := fetchProviderCached("codex", false)
		if err == nil {
			t.Fatalf("failure %d: expected a rate-limited error", i)
		}
		if snap == nil {
			t.Fatalf("failure %d: lost the stale snapshot entirely", i)
		}
		provider, ok := snap.Providers["codex"]
		if !ok || !provider.Stale {
			t.Fatalf("failure %d: stale reading missing or unmarked: %+v", i, provider)
		}
		if got := provider.Resources["primary"].Remaining; got != 80 {
			t.Fatalf("failure %d: banked reading lost or altered: remaining=%v", i, got)
		}
		if got := persistedRecord(t, "codex").FailureStreak; got != i {
			t.Fatalf("failure %d: streak = %d", i, got)
		}
		expireBackoff(t, "codex")
	}
}

// TestGenuineExhaustionIsNotTelemetryFailure: a real reading at the cap must
// still classify as exhausted and must carry no telemetry cause. The fix must
// not blur "out of quota" into "could not read quota".
func TestGenuineExhaustionIsNotTelemetryFailure(t *testing.T) {
	snap := &UsageSnapshot{
		GeneratedAt: time.Now().UTC(),
		Providers: map[string]ProviderUsage{"codex": {DisplayName: "Codex", Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 100, Remaining: 0, WindowSeconds: 18000},
		}}},
	}
	st, ok := NewQuotaEngine().ComputeAll(snap)["codex"]
	if !ok {
		t.Fatal("a genuinely exhausted provider disappeared from ComputeAll")
	}
	if st.Available {
		t.Fatalf("exhausted provider reported available: %+v", st)
	}
	if st.Cause != "" {
		t.Fatalf("genuine exhaustion was mislabelled as a telemetry failure: cause=%q", st.Cause)
	}
	if st.Reason == reasonNoQuotaData {
		t.Fatalf("genuine exhaustion collapsed into no-quota-data: %+v", st)
	}
}

// TestNonRateLimitedFailuresKeepPriorBehavior: only the telemetry rate-limit
// class gains an identity here. Changing how auth or transport failures route
// is a separate decision and must not ride along with this fix.
func TestNonRateLimitedFailuresKeepPriorBehavior(t *testing.T) {
	snap := &UsageSnapshot{
		GeneratedAt: time.Now().UTC(),
		Providers:   map[string]ProviderUsage{},
		Errors:      map[string]string{"codex": "http-401: codex usage: HTTP 401", "grok": "unreachable: dial tcp: no route"},
	}
	computed := NewQuotaEngine().ComputeAll(snap)
	for _, name := range []string{"codex", "grok"} {
		if _, ok := computed[name]; ok {
			t.Fatalf("%s: a non-rate-limited failure gained a BurnState; routing treatment changed as a side effect", name)
		}
	}
}
