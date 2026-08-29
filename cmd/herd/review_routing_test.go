package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// TestPoolReviewerIsNotHardwiredToOneHarness is the FAC-574 gate.
//
// The pool launched OpenCode with an Ollama proxy model unconditionally, so no
// native-Claude route existed through the exact-SHA pool lifecycle and a rate
// limited proxy killed exact review entirely. A reviewer harness must come from
// the router, never from a literal in this file.
func TestPoolReviewerIsNotHardwiredToOneHarness(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Comments legitimately name the old default when explaining the fix, so
	// only inspect code lines.
	for i, line := range strings.Split(body, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "//") {
			continue
		}
		for _, banned := range []string{"litellm/", "ollama", "\"opencode\""} {
			if strings.Contains(strings.ToLower(code), banned) {
				t.Errorf("review_pool.go:%d hardwires a reviewer surface (%q): %s", i+1, banned, code)
			}
		}
	}
	if !strings.Contains(body, "router.NewRouter") {
		t.Error("pool reviewer must resolve its harness through the router")
	}
}

// A model without its surface is not a route; accepting it would silently pair
// an operator's model with whatever provider the router happened to pick.
func TestModelWithoutProviderIsRejected(t *testing.T) {
	if _, err := resolvePoolReviewer("", "claude-sonnet-5", ""); err == nil {
		t.Fatal("expected --model without --provider to be refused")
	}
}

// An explicit model is the route, not a string replacement applied after the
// provider's default model has already passed routing. AGY's non-Gemini and
// Gemini pools are independently metered: an exhausted non-Gemini default for
// QA must not reject a healthy exact Gemini request, and the live probe must
// target the exact Gemini model rather than a healthy sibling.
func TestExactReviewModelOverrideUsesItsOwnHealthyPool(t *testing.T) {
	usage.InvalidateSnapshotCache()
	t.Cleanup(usage.InvalidateSnapshotCache)

	dir := t.TempDir()
	probeLog := filepath.Join(dir, "provider-probes.log")
	agy := filepath.Join(dir, "agy")
	if err := os.WriteFile(agy, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HERD_TEST_PROBE_LOG\"\nprintf 'HERD_PROVIDER_PROBE_OK\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	openusage := filepath.Join(dir, "openusage")
	quota := `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"used":100,"utilization":1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	if err := os.WriteFile(openusage, []byte("#!/bin/sh\nprintf '%s\\n' '"+quota+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERD_OPENUSAGE_BIN", openusage)
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota-cache.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HERD_TEST_PROBE_LOG", probeLog)
	t.Setenv("HERDR_ROUTE_STATE_DIR", filepath.Join(dir, "routing-state"))
	t.Setenv("HERD_AVAILABLE_PROVIDERS", "")
	t.Setenv("HERD_UNAVAILABLE_PROVIDERS", "")
	t.Setenv("HERD_FAMILY_POSTURE", "")

	got, err := resolvePoolReviewer("agy", "gemini-3.7-flash", "")
	if err != nil {
		t.Fatalf("healthy exact override was rejected by the exhausted default pool: %v", err)
	}
	if got.Provider != "agy" || got.Model != "gemini-3.7-flash" || got.Family != "google" {
		t.Fatalf("resolved reviewer = %+v, want exact AGY Gemini/Google identity", got)
	}
	if pool := router.QuotaPoolFor(got.Provider, got.Model); pool != "gemini" {
		t.Fatalf("exact override bills pool %q, want gemini", pool)
	}
	probes, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatalf("exact live probe was not run: %v", err)
	}
	if !strings.Contains(string(probes), "--model gemini-3.7-flash") {
		t.Fatalf("live probe did not evaluate the exact override: %s", probes)
	}
	if strings.Contains(string(probes), "gemini-3.1-pro-high") || strings.Contains(string(probes), "claude-opus-4-6-thinking") {
		t.Fatalf("default model was probed before applying the exact override: %s", probes)
	}
	if _, err := os.Stat(filepath.Join(dir, "pool")); !os.IsNotExist(err) {
		t.Fatalf("route evaluation created a pool/lease side effect: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tabs.log")); !os.IsNotExist(err) {
		t.Fatalf("route evaluation created a tab side effect: %v", err)
	}
}

func TestExactReviewModelOverrideRefusalsAreFailClosed(t *testing.T) {
	healthy := `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	cases := []struct {
		name          string
		quota         string
		providerProbe string
		excludeFamily string
		wantReason    string
	}{
		{name: "unknown quota", quota: `{"generatedAt":"2026-08-29T20:00:00Z","providers":{}}`, wantReason: "UNKNOWN quota"},
		{name: "no quota data", quota: `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{}}}}`, wantReason: "UNKNOWN quota"},
		{name: "unavailable exact model", quota: healthy, providerProbe: "case \"$*\" in *gemini-3.7-flash*) printf 'no configured model\\n' ;; *) printf 'HERD_PROVIDER_PROBE_OK\\n' ;; esac", wantReason: "no configured model"},
		{name: "excluded exact family", quota: healthy, excludeFamily: "google", wantReason: "family google excluded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := installExactReviewRouteFixture(t, tc.quota, tc.providerProbe)
			_, err := resolvePoolReviewer("agy", "gemini-3.7-flash", tc.excludeFamily)
			if err == nil {
				t.Fatal("exact route refusal unexpectedly succeeded")
			}
			for _, want := range []string{"provider=agy", "model=gemini-3.7-flash", "pool=gemini", tc.wantReason} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal must report %q, got: %v", want, err)
				}
			}
			if _, statErr := os.Stat(fixture.tabLog); !os.IsNotExist(statErr) {
				t.Fatalf("refused route reached a tab side effect: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.dir, "pool")); !os.IsNotExist(statErr) {
				t.Fatalf("refused route reached a pool/lease side effect: %v", statErr)
			}
		})
	}
}

func TestExactReviewModelOverrideRefreshesStaleQuotaCache(t *testing.T) {
	fresh := `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"used":100,"utilization":1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	stale := `{"generatedAt":"2026-08-29T19:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"used":100,"utilization":1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	fixture := installExactReviewRouteFixture(t, fresh, "")
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	cache := `{"fetched_at":"2026-08-29T18:00:00Z","snapshot":` + stale + `}`
	if err := os.WriteFile(fixture.quotaCache, []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolvePoolReviewer("agy", "gemini-3.7-flash", "")
	if err != nil {
		t.Fatalf("fresh live quota did not replace an aged-out refusal: %v", err)
	}
	if got.Pool != "gemini" || got.QuotaAge != 0 {
		t.Fatalf("route used pool=%q cache_age=%s, want fresh gemini evidence", got.Pool, got.QuotaAge)
	}
	if _, err := os.Stat(fixture.quotaFetchLog); err != nil {
		t.Fatalf("aged cache was used instead of a fresh quota read: %v", err)
	}
}

type exactReviewRouteFixture struct {
	dir           string
	tabLog        string
	quotaCache    string
	quotaFetchLog string
}

func installExactReviewRouteFixture(t *testing.T, quota, providerProbe string) exactReviewRouteFixture {
	t.Helper()
	dir := t.TempDir()
	probeLog := filepath.Join(dir, "provider-probes.log")
	if providerProbe == "" {
		providerProbe = "printf 'HERD_PROVIDER_PROBE_OK\\n'"
	}
	agy := filepath.Join(dir, "agy")
	agyScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HERD_TEST_PROBE_LOG\"\n" + providerProbe + "\n"
	if err := os.WriteFile(agy, []byte(agyScript), 0o755); err != nil {
		t.Fatal(err)
	}
	quotaFetchLog := filepath.Join(dir, "quota-fetch.log")
	openusage := filepath.Join(dir, "openusage")
	openusageScript := "#!/bin/sh\nprintf 'fetch\\n' >> \"$HERD_TEST_QUOTA_LOG\"\nprintf '%s\\n' '" + quota + "'\n"
	if err := os.WriteFile(openusage, []byte(openusageScript), 0o755); err != nil {
		t.Fatal(err)
	}
	tabLog := filepath.Join(dir, "tabs.log")
	fakeHerdr := filepath.Join(dir, "herdr")
	if err := os.WriteFile(fakeHerdr, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HERD_TEST_TAB_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	quotaCache := filepath.Join(dir, "quota-cache.json")
	t.Setenv("PATH", dir)
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERD_OPENUSAGE_BIN", openusage)
	t.Setenv("HERD_QUOTA_CACHE_PATH", quotaCache)
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HERD_TEST_PROBE_LOG", probeLog)
	t.Setenv("HERD_TEST_QUOTA_LOG", quotaFetchLog)
	t.Setenv("HERD_TEST_TAB_LOG", tabLog)
	t.Setenv("HERDR_BIN_PATH", fakeHerdr)
	t.Setenv("HERDR_ROUTE_STATE_DIR", filepath.Join(dir, "routing-state"))
	for _, key := range []string{"HERD_AVAILABLE_PROVIDERS", "HERD_UNAVAILABLE_PROVIDERS", "HERD_FAMILY_POSTURE", "HERD_NO_GEMINI", "HERD_NO_CLAUDE", "HERD_CLAUDE_ONLY", "HERD_ERA_PROVIDERS"} {
		t.Setenv(key, "")
	}
	usage.InvalidateSnapshotCache()
	t.Cleanup(usage.InvalidateSnapshotCache)
	return exactReviewRouteFixture{dir: dir, tabLog: tabLog, quotaCache: quotaCache, quotaFetchLog: quotaFetchLog}
}

// The launch must resolve before any tab or lease side effect, so an
// unroutable reviewer cannot orphan a tab or strand a warm-pool lease.
func TestHarnessResolvesBeforeSideEffects(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	resolve := strings.Index(body, "resolvePoolReviewer(*provider")
	create := strings.Index(body, "herdr.TabCreate(")
	if resolve < 0 || create < 0 {
		t.Fatal("expected both the reviewer resolve and the tab create in the launch path")
	}
	if resolve > create {
		t.Error("reviewer harness must resolve before the tab is created")
	}
}
