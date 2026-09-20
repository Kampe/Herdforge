package usage

import (
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise provider error propagation through a successful poll, a new 429,
// persisted backoff, and recovery. Aggregate assertions below retain coverage
// of pre-existing behavior, independently of this fix. expireBackoff advances
// the persisted clock fixture without sleeping or calling upstream.
func TestStaleBackoffLimitsRetainsProviderError(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	limited := false
	observed := time.Date(2026, 9, 12, 22, 6, 48, 0, time.UTC)
	wantError := "rate-limited: codex usage: HTTP 429; retry-after=600"
	calls := coldStartEnv(t, func() (ProviderUsage, error) {
		if limited {
			return ProviderUsage{}, RateLimitedPollError("codex usage: HTTP 429; retry-after=600")
		}
		return ProviderUsage{ObservedAt: observed, Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 62, Remaining: 38, WindowSeconds: 18000},
		}}, nil
	})
	healthy, err := FetchProviderForce("codex", false)
	if err != nil || healthy == nil {
		t.Fatalf("bank healthy reading: snapshot=%+v error=%v", healthy, err)
	}
	want := healthy.Providers["codex"]
	if want.Account == nil || want.Account.Key == "" {
		t.Fatal("fixture has no account identity")
	}
	want.Stale = true
	assertReading := func(snap *UsageSnapshot, age time.Duration) LimitsReport {
		t.Helper()
		body, err := json.Marshal(snap.LimitsReport(age))
		if err != nil {
			t.Fatal(err)
		}
		var report LimitsReport
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		if report.Schema != LimitsSchema || len(report.Providers) != 1 || !reflect.DeepEqual(report.Providers["codex"], want) {
			t.Fatalf("stale limits changed the banked reading: got=%+v want=%+v", report, want)
		}
		if report.GeneratedAt.IsZero() || report.CacheAgeSeconds != age.Seconds() {
			t.Fatalf("limits report metadata changed: %+v", report)
		}
		return report
	}
	assertProviderError := func(snap *UsageSnapshot) {
		t.Helper()
		report := assertReading(snap, 0)
		if len(report.Errors) != 1 || report.Errors["codex"] != wantError {
			t.Fatalf("stale limits lost exact-provider telemetry error: errors=%v", report.Errors)
		}
	}

	limited = true
	snap, err := FetchProviderForce("codex", true)
	if pollErrorCode(err) != "rate-limited" {
		t.Fatalf("new 429 lost returned error: %v", err)
	}
	assertProviderError(snap)
	backoff := persistedRecord(t, "codex")
	if backoff.FailureStreak != 1 || backoff.Error != wantError || backoff.BackoffUntil.Sub(backoff.ObservedAt) < 600*time.Second {
		t.Fatalf("new 429 did not retain classified error and server deadline: %+v", backoff)
	}
	for _, force := range []bool{false, true} {
		snap, err = FetchProviderForce("codex", force)
		if pollErrorCode(err) != "rate-limited" {
			t.Fatalf("persisted backoff lost returned error: %v", err)
		}
		assertProviderError(snap)
		all, age, err := FetchSnapshotCachedForce(force)
		if err != nil {
			t.Fatalf("aggregate with a banked reading failed: %v", err)
		}
		// Aggregate collection already derives Errors from the separately
		// returned err, overwriting snapshot.Errors. Its persisted error is
		// currently double-prefixed. This compatibility check passes before
		// the fix; the provider assertion above is the mutation oracle.
		report := assertReading(all, age)
		if len(report.Errors) != 1 || report.Errors["codex"] != "rate-limited: "+wantError {
			t.Fatalf("aggregate err-derived compatibility changed: %v", report.Errors)
		}
		if got := persistedRecord(t, "codex"); !reflect.DeepEqual(got, backoff) {
			t.Fatalf("backoff read changed persisted deadline, identity or reading: got=%+v want=%+v", got, backoff)
		}
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("backoff repolled upstream: calls=%d want=2", got)
	}

	expireBackoff(t, "codex")
	limited = false
	observed = observed.Add(time.Hour)
	recovered, err := FetchProviderForce("codex", true)
	if err != nil || recovered == nil {
		t.Fatalf("recovery failed: snapshot=%+v error=%v", recovered, err)
	}
	if got := recovered.LimitsReport(0); len(got.Errors) != 0 || got.Providers["codex"].Stale || !got.Providers["codex"].ObservedAt.Equal(observed) {
		t.Fatalf("successful refresh retained the old failure: %+v", got)
	}
	if got := persistedRecord(t, "codex"); got.Error != "" || !got.BackoffUntil.IsZero() || got.FailureStreak != 0 {
		t.Fatalf("successful refresh did not reset persisted failure: %+v", got)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("recovery polls=%d want=3", got)
	}

	// The reader accepts persisted backoff records without an Error. A
	// concurrent successful refresh can also leave an errorless record for
	// the final post-lock reread. Neither case is a classified empty error.
	errorless := persistedRecord(t, "codex")
	errorless.BackoffUntil = time.Now().Add(time.Minute)
	if err := writeCachedRecords(map[string]cachedProviderRecord{"codex": errorless}); err != nil {
		t.Fatal(err)
	}
	snap, err = FetchProviderForce("codex", true)
	if pollErrorCode(err) != "rate-limited" || snap == nil {
		t.Fatalf("errorless backoff lost existing return contract: snapshot=%+v error=%v", snap, err)
	}
	want = recovered.Providers["codex"]
	want.Stale = true
	if report := assertReading(snap, 0); len(report.Errors) != 0 {
		t.Fatalf("errorless cache record fabricated an empty telemetry error: %v", report.Errors)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("errorless backoff repolled upstream: calls=%d want=3", got)
	}
}
