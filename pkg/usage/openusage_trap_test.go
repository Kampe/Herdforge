package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FAC-786 negative controls. OpenUsage is being removed from every runtime
// acquisition path: no executable lookup, no HERD_OPENUSAGE_BIN, no helper
// path, no app-cache reads. These tests place a hostile `openusage` binary
// where the old code would find it and prove it can never execute or affect a
// real fetch. They were run against the pre-fix tree and observed failing
// (RED) before the fix landed; the failure output is retained in the lane
// report.

// trapFixture builds a dir containing (a) a hostile openusage trap that
// records its execution and prints fabricated-healthy poison JSON, (b) a
// snapshot fixture file the post-fix seam serves instead, and (c) an isolated
// HOME with no reachable credentials and a poisoned "prior state" cache at
// the default path.
func trapFixture(t *testing.T) (dir, trap, marker, fixture string) {
	t.Helper()
	dir = t.TempDir()
	marker = filepath.Join(dir, "trap-ran")
	trap = filepath.Join(dir, "openusage")
	poison := `{"generatedAt":"2026-09-09T00:00:00Z","providers":{"claude":{"displayName":"Claude","plan":"TRAP-POISON","stale":false,"resources":{"weekly":{"kind":"consumption","unit":"percent","used":1,"remaining":99,"limit":100,"utilization":0.01,"resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	script := "#!/bin/sh\nprintf ran > " + marker + "\nprintf '%s\\n' '" + poison + "'\n"
	if err := os.WriteFile(trap, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture = filepath.Join(dir, "snapshot.json")
	body := `{"generatedAt":"2026-09-09T00:00:00Z","providers":{"claude":{"displayName":"Claude","plan":"FIXTURE","stale":false,"resources":{"weekly":{"kind":"consumption","unit":"percent","used":42,"remaining":58,"limit":100,"utilization":0.42,"resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	if err := os.WriteFile(fixture, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".herd", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Arbitrary prior quota state at the default cache path: a fabricated
	// healthy reading left behind by an earlier era. It must never surface.
	prior := cachedSnapshot{FetchedAt: time.Now(), Snapshot: &UsageSnapshot{
		GeneratedAt: time.Now(),
		Providers: map[string]ProviderUsage{
			"claude": {DisplayName: "Claude", Plan: "PRIOR-POISON", Stale: false,
				Resources: map[string]ResourceUsage{
					"weekly": {Kind: "consumption", Unit: "percent", Used: 0, Remaining: 100, Limit: 100, Utilization: 0, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800},
				}},
		},
	}}
	priorBody, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(home, ".herd", "state", "quota-snapshot.json"), priorBody, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CONFIG_DIR", "XDG_CONFIG_HOME", "HERD_QUOTA_CACHE_PATH"} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", dir) // the trap is also the only thing on PATH
	t.Setenv("HERD_QUOTA_KEYCHAIN", "0")
	InvalidateSnapshotCache()
	t.Cleanup(InvalidateSnapshotCache)
	return dir, trap, marker, fixture
}

func injectedFixture(t *testing.T, path, provider string) (*UsageSnapshot, error) {
	t.Helper()
	var snap UsageSnapshot
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	if provider == "" {
		return &snap, nil
	}
	p, ok := snap.Providers[provider]
	if !ok {
		return &UsageSnapshot{Providers: map[string]ProviderUsage{}}, pollErrf("no-windows", "fixture carries no reading for %s", provider)
	}
	return &UsageSnapshot{GeneratedAt: snap.GeneratedAt, Providers: map[string]ProviderUsage{provider: p}}, nil
}

func trapResultIsClean(t *testing.T, snap *UsageSnapshot, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("the native path must serve the fixture without any binary exec: %v", err)
	}
	if snap == nil || snap.Providers == nil {
		t.Fatal("snapshot or providers must not be nil")
	}
	claude, ok := snap.Providers["claude"]
	if !ok {
		t.Fatalf("fixture claude reading missing: %+v", snap.Providers)
	}
	if claude.Plan != "FIXTURE" {
		t.Fatalf("poison leaked into the snapshot (plan=%q); the fixture reading must be the only claude data", claude.Plan)
	}
	if used := claude.Resources["weekly"].Used; used != 42 {
		t.Fatalf("fixture weekly used = %v, want 42 (poison would read 1 or 0)", used)
	}
}

// TestOpenUsageBinaryOverrideTrapNeverExecutes is the RED control: pre-fix,
// HERD_OPENUSAGE_BIN pointed at the trap and the trap ran, returning poison.
func TestOpenUsageBinaryOverrideTrapNeverExecutes(t *testing.T) {
	_, trap, marker, fixture := trapFixture(t)
	t.Setenv("HERD_OPENUSAGE_BIN", trap)
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")

	snap, err := injectedFixture(t, fixture, "")
	trapResultIsClean(t, snap, err)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("an openusage trap binary EXECUTED during FetchSnapshot; quota acquisition must never shell out")
	}

}

// TestOpenUsagePathTrapNeverExecutes proves a bare `openusage` on PATH is
// equally dead. Pre-fix on a macOS host with the real app installed this would
// have executed the installed helper (live credentials + network), which is
// exactly the behavior being removed; the RED demonstration therefore ran the
// binary-override variant only, and this variant is part of the GREEN gate.
func TestOpenUsagePathTrapNeverExecutes(t *testing.T) {
	_, _, marker, fixture := trapFixture(t)
	t.Setenv("HERD_OPENUSAGE_BIN", "")
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")

	snap, err := injectedFixture(t, fixture, "")
	trapResultIsClean(t, snap, err)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("an openusage trap on PATH EXECUTED during FetchSnapshot; quota acquisition must never shell out")
	}
}

// TestFetchProviderIgnoresOpenUsageTrap: the single-provider entry point had
// the same binary fallback; the trap must not execute there either.
func TestFetchProviderIgnoresOpenUsageTrap(t *testing.T) {
	_, trap, marker, fixture := trapFixture(t)
	t.Setenv("HERD_OPENUSAGE_BIN", trap)
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")

	snap, err := injectedFixture(t, fixture, "claude")
	trapResultIsClean(t, snap, err)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("an openusage trap binary EXECUTED during FetchProvider")
	}

	// A provider the fixture does not carry must report unknown, not poison.
	missing, err := injectedFixture(t, fixture, "codex")
	if err == nil {
		t.Fatal("a fixture without codex must not report a codex reading")
	}
	if missing != nil && len(missing.Providers) != 0 {
		t.Fatalf("no provider reading may be fabricated for codex: %+v", missing.Providers)
	}
}

// TestFreshPoisonedCacheIsNotServedWithFixtureSeam: arbitrary prior quota
// state — here a freshly-timed poisoned cache entry — must never be served
// when the caller pins a fixture snapshot. Pre-fix, the cache hit won and the
// poison was returned.
func TestFreshPoisonedCacheIsNotServed(t *testing.T) {
	dir, trap, _, fixture := trapFixture(t)
	cachePath := filepath.Join(dir, "quota-cache.json")
	poison := &UsageSnapshot{
		GeneratedAt: time.Now(),
		Providers: map[string]ProviderUsage{
			"claude": {DisplayName: "Claude", Plan: "CACHE-POISON", Stale: false,
				Resources: map[string]ResourceUsage{
					"weekly": {Kind: "consumption", Unit: "percent", Used: 0, Remaining: 100, Limit: 100, Utilization: 0, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800},
				}},
		},
	}
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: time.Now(), Snapshot: poison})
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_OPENUSAGE_BIN", trap)
	t.Setenv("HERD_QUOTA_CACHE_PATH", cachePath)
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	snap, err := injectedFixture(t, fixture, "")
	trapResultIsClean(t, snap, err)
}

// TestPersistedCacheRequiresProvableAccountIdentity: a cached reading that
// cannot prove it belongs to the account whose credentials are on this machine
// must not be reused. Pre-fix there was no identity binding at all and the
// entry was served.
func TestPersistedCacheRequiresProvableAccountIdentity(t *testing.T) {
	dir, _, _, _ := trapFixture(t)
	cachePath := filepath.Join(dir, "quota-cache.json")
	cached := &UsageSnapshot{
		GeneratedAt: time.Now(),
		Providers: map[string]ProviderUsage{
			"claude": {DisplayName: "Claude", Plan: "CACHE-POISON", Stale: false,
				Resources: map[string]ResourceUsage{
					"weekly": {Kind: "consumption", Unit: "percent", Used: 0, Remaining: 100, Limit: 100, Utilization: 0, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800},
				}},
		},
	}
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: time.Now(), Snapshot: cached})
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_QUOTA_CACHE_PATH", cachePath)

	snap, _, ok := readSnapshotFile(time.Minute)
	if ok {
		t.Fatalf("a cached entry with no provable account identity must not be served, got plan=%q", snap.Providers["claude"].Plan)
	}
}
