package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Kampe/Herdforge/pkg/usage"
)

// TestRouteComputedQuotaUsesCachedBackoffNotRawPolling calls the exact
// production line herd route uses (routeComputedQuota), not a copy of it.
// It is the FAC-786 regression: routeComputedQuota used to be an inline
// usage.FetchSnapshot() call in runRoute with no persisted backoff, so a
// provider that had just 429'd on its usage endpoint disappeared from every
// subsequent route decision instead of surfacing as stale, and every route
// invocation re-hit the endpoint unbounded.
//
// If routeComputedQuota is ever changed back to call the raw, uncached
// usage.FetchSnapshot(), this test fails: the poll counter below would climb
// past 2 (unbounded repeated polling), and the provider would vanish from
// `computed` under backoff instead of surfacing Class/Reason "stale".
func TestRouteComputedQuotaUsesCachedBackoffNotRawPolling(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"route-seam-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls int32
	rateLimited := false
	restore := usage.SetNativePollersForTest(map[string]func() (usage.ProviderUsage, error){
		"codex": func() (usage.ProviderUsage, error) {
			atomic.AddInt32(&calls, 1)
			if rateLimited {
				return usage.ProviderUsage{}, usage.RateLimitedPollError("HTTP 429; retry-after=300")
			}
			return usage.ProviderUsage{Resources: map[string]usage.ResourceUsage{
				"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 20, Remaining: 80, WindowSeconds: 18000},
			}}, nil
		},
	})
	defer restore()
	usage.InvalidateSnapshotCache()
	defer usage.InvalidateSnapshotCache()

	e := usage.NewQuotaEngine()

	// First route decision: healthy reading, persisted to disk.
	computed := routeComputedQuota(e)
	if st, ok := computed["codex"]; !ok || !st.Available {
		t.Fatalf("expected a healthy codex binding on the first route call, got %+v (ok=%v)", computed["codex"], ok)
	}

	rateLimited = true

	// Three more route decisions while the provider is rate-limited: the
	// production seam must bound them to at most one more live poll and must
	// keep surfacing the provider as stale, never absent/unknown.
	for i := 0; i < 3; i++ {
		computed = routeComputedQuota(e)
		st, ok := computed["codex"]
		if !ok {
			t.Fatalf("route call %d: codex dropped from computed quota under backoff instead of surfacing stale", i)
		}
		if !st.Stale {
			t.Fatalf("route call %d: codex must be marked stale under backoff, got %+v", i, st)
		}
		if st.Reason != "stale" {
			t.Fatalf("route call %d: expected reason %q, got %q -- stale/unknown/exhausted distinction lost", i, "stale", st.Reason)
		}
		if st.Available {
			t.Fatalf("route call %d: a stale reading under backoff must never read as Available (no fabricated headroom)", i)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 live polls (1 success + 1 429) across 4 route decisions, got %d -- production seam is not bounding repeated polling", got)
	}
}

// TestRouteComputedQuotaHasNoOpinionWithoutAnyPriorReading is the
// no-prior-reading half of the same seam: a provider that has never
// successfully polled must read as unknown (absent from `computed`, or
// Available:false with no fabricated numbers), never as healthy.
func TestRouteComputedQuotaHasNoOpinionWithoutAnyPriorReading(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"route-seam-no-prior"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	restore := usage.SetNativePollersForTest(map[string]func() (usage.ProviderUsage, error){
		"codex": func() (usage.ProviderUsage, error) {
			return usage.ProviderUsage{}, usage.RateLimitedPollError("HTTP 429; retry-after=300")
		},
	})
	defer restore()
	usage.InvalidateSnapshotCache()
	defer usage.InvalidateSnapshotCache()

	e := usage.NewQuotaEngine()
	computed := routeComputedQuota(e)
	if st, ok := computed["codex"]; ok && st.Available {
		t.Fatalf("codex has no prior reading and is rate-limited; must never read as Available, got %+v", st)
	}
}
