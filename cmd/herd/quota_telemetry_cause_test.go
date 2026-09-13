package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// coldStart429Quota reproduces the live shape: one provider polling healthily
// while another's usage endpoint is already rate-limited on its FIRST call, so
// nothing is ever banked for it. A healthy peer must be present -- that is what
// production looked like (codex/grok/ollama-cloud reading fine while claude
// 429'd), and it keeps FetchSnapshotCached out of its total-failure branch.
func coldStart429Quota(t *testing.T) map[string]usage.BurnState {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(t.TempDir(), "quota.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"cause-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := usage.SetNativePollersForTest(map[string]func() (usage.ProviderUsage, error){
		"codex": func() (usage.ProviderUsage, error) {
			return usage.ProviderUsage{Resources: map[string]usage.ResourceUsage{
				"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 20, Remaining: 80, WindowSeconds: 18000},
			}}, nil
		},
		"grok": func() (usage.ProviderUsage, error) {
			return usage.ProviderUsage{}, usage.RateLimitedPollError("grok usage: HTTP 429; retry-after=0")
		},
	})
	t.Cleanup(restore)
	t.Cleanup(func() { usage.InvalidateSnapshotCache() })
	usage.InvalidateSnapshotCache()
	return routeComputedQuota(usage.NewQuotaEngine())
}

// TestTelemetryCauseReachesNativeQuotaOutput checks the CONSUMER, not the
// producer. FAC-818's diagnosis was that a cold-start telemetry 429 left the
// provider absent from every native surface, so `herd quota --provider claude`
// answered "no data for provider" (exit 4) with the classified 429 nowhere in
// the output. This drives routeComputedQuota -- the exact line `herd route`
// uses -- and then the exact json.Encoder value `herd quota --provider X
// --json` emits, rather than assuming a struct field is rendered somewhere.
func TestTelemetryCauseReachesNativeQuotaOutput(t *testing.T) {
	computed := coldStart429Quota(t)

	st, ok := computed["grok"]
	if !ok {
		t.Fatal("cold-start telemetry 429 left the provider absent from the computed quota map; `herd quota --provider` still answers \"no data for provider\" and drops the cause")
	}
	if st.Cause == "" || !strings.Contains(st.Cause, usage.CauseTelemetryRateLimited) {
		t.Fatalf("computed state carries no telemetry cause: %+v", st)
	}
	if !strings.Contains(st.Cause, "429") {
		t.Fatalf("telemetry cause lost the underlying classified error: %q", st.Cause)
	}

	// The exact encoding herd quota --provider <p> --json performs.
	var buf strings.Builder
	if err := json.NewEncoder(&buf).Encode(map[string]usage.BurnState{"grok": st}); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &decoded); err != nil {
		t.Fatal(err)
	}
	cause, _ := decoded["grok"]["cause"].(string)
	if !strings.Contains(cause, usage.CauseTelemetryRateLimited) {
		t.Fatalf("native quota JSON does not carry the cause; consumers still cannot tell throttling from exhaustion: %s", buf.String())
	}
	// Never invent quota: the JSON must not advertise headroom.
	if used, _ := decoded["grok"]["used"].(float64); used != 0 {
		t.Fatalf("telemetry-unavailable state published a fabricated used figure: %v", used)
	}
	if avail, _ := decoded["grok"]["available"].(bool); avail {
		t.Fatal("telemetry-unavailable state published itself as available")
	}
}

// TestTelemetryCauseDoesNotChangeRouting is the regression guard on the other
// side. Before this change the provider was ABSENT from the quota map, and
// pkg/router treats an absent provider as unknown-and-routable (available()
// skips the quota gate, effectivePressure returns 50). Naming the cause must
// not smuggle in a refusal: `herd route --provider codex` succeeded before and
// must still succeed, at the same pressure.
func TestTelemetryCauseDoesNotChangeRouting(t *testing.T) {
	computed := coldStart429Quota(t)

	e := usage.NewQuotaEngine()
	sr := router.NewRouter(e, computed)
	sr.Probes = &router.Probes{
		CLIPresent: func(string) bool { return true },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	rt, err := sr.Pick("implementation", "grok", "")
	if err != nil {
		t.Fatalf("naming the telemetry cause started refusing a provider the router accepted before: %v", err)
	}
	if rt.Provider != "grok" {
		t.Fatalf("route moved off the requested provider: %+v", rt)
	}
	if rt.QuotaPressure != 50 {
		t.Fatalf("quota pressure changed from the unknown-provider default: got %d, want 50", rt.QuotaPressure)
	}
}

// TestTelemetryCauseIsLostWhenEveryProviderFails pins a REMAINING boundary
// rather than claiming coverage this change does not have.
//
// routeComputedQuota (main.go) keeps `computed` empty when FetchSnapshotCached
// returns an error, and FetchSnapshotCached errors when NO provider could be
// polled at all. So on a host where every provider's telemetry is failing at
// once, the cause is still dropped before any consumer sees it. That is a
// cmd/herd caller line, not pkg/usage, and widening the snapshot's
// total-failure contract would also move `herd quota --limits` exit codes --
// both outside this change's scope. Asserted here so the gap is a documented
// boundary with a test on it, and a future fix has a failing anchor to flip.
func TestTelemetryCauseIsLostWhenEveryProviderFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(t.TempDir(), "quota.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"all-fail-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := usage.SetNativePollersForTest(map[string]func() (usage.ProviderUsage, error){
		"codex": func() (usage.ProviderUsage, error) {
			return usage.ProviderUsage{}, usage.RateLimitedPollError("codex usage: HTTP 429; retry-after=0")
		},
	})
	defer restore()
	usage.InvalidateSnapshotCache()
	defer usage.InvalidateSnapshotCache()

	if computed := routeComputedQuota(usage.NewQuotaEngine()); len(computed) != 0 {
		t.Fatalf("total-provider-failure boundary moved: routeComputedQuota now returns %d entries. "+
			"If that is intended, this test should become the positive assertion that the cause survives.", len(computed))
	}
}
