package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/usage"
)

func pinQuotaSupervisorAccounts(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"sup-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	grokDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(grokDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grokDir, "auth.json"), []byte(`{"sup-grok-key":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaSupervisorHonorsBackoffWhileLiveFetchRepeats(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "quota.json")
	t.Setenv("HERD_QUOTA_CACHE_PATH", cache)
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	pinQuotaSupervisorAccounts(t, home)
	observed := time.Now().UTC()
	var claudeCalls, grokCalls atomic.Int32
	limited := atomic.Bool{}
	restore := usage.SetNativePollersForTest(map[string]func() (usage.ProviderUsage, error){
		"claude": func() (usage.ProviderUsage, error) {
			claudeCalls.Add(1)
			if limited.Load() {
				return usage.ProviderUsage{}, usage.RateLimitedPollError("claude usage: HTTP 429; retry-after=0")
			}
			return usage.ProviderUsage{ObservedAt: observed, Resources: map[string]usage.ResourceUsage{
				"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 20, Remaining: 80, WindowSeconds: 18000},
			}}, nil
		},
		"grok": func() (usage.ProviderUsage, error) {
			grokCalls.Add(1)
			return usage.ProviderUsage{ObservedAt: observed, Resources: map[string]usage.ResourceUsage{
				"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 10, Remaining: 90, WindowSeconds: 18000},
			}}, nil
		},
	})
	t.Cleanup(restore)
	t.Cleanup(func() { usage.InvalidateSnapshotCache() })
	usage.InvalidateSnapshotCache()

	snap, err := loadQuotaSupervisorSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := quotaObservationTime(snap, "claude"); !got.Equal(observed) {
		t.Fatalf("SourceAt used assembly time %v, want provider ObservedAt %v (GeneratedAt=%v)", got, observed, snap.GeneratedAt)
	}
	if claudeCalls.Load() != 1 || grokCalls.Load() != 1 {
		t.Fatalf("first load polls=%d/%d, want 1/1", claudeCalls.Load(), grokCalls.Load())
	}

	limited.Store(true)
	beforeClaude, beforeGrok := claudeCalls.Load(), grokCalls.Load()
	if _, err := loadQuotaSupervisorSnapshot(); err == nil {
		// cached path returns stale+error; FetchSnapshotCached may still return snap
	}
	afterFirst429 := claudeCalls.Load()
	if afterFirst429 != beforeClaude+1 {
		t.Fatalf("first 429 did not poll once: before=%d after=%d", beforeClaude, afterFirst429)
	}

	for i := 0; i < 5; i++ {
		_, _ = loadQuotaSupervisorSnapshot()
	}
	if claudeCalls.Load() != afterFirst429 {
		t.Fatalf("supervisor repeated live polls during backoff: %d after first 429, want %d", claudeCalls.Load(), afterFirst429)
	}
	if grokCalls.Load() != beforeGrok {
		t.Fatalf("fresh healthy grok re-polled during claude backoff: %d want %d (claude=%d)", grokCalls.Load(), beforeGrok, claudeCalls.Load())
	}

	liveBefore := claudeCalls.Load()
	for i := 0; i < 5; i++ {
		_, _ = usage.FetchSnapshot()
	}
	if claudeCalls.Load() != liveBefore+5 {
		t.Fatalf("negative control: FetchSnapshot live bypass did not repeat: got %d want %d", claudeCalls.Load(), liveBefore+5)
	}

	expireClaudeBackoff(t, cache)
	beforeExpiry := claudeCalls.Load()
	_, _ = loadQuotaSupervisorSnapshot()
	if claudeCalls.Load() != beforeExpiry+1 {
		t.Fatalf("after backoff expiry supervisor must poll once, got %d want %d", claudeCalls.Load(), beforeExpiry+1)
	}
}

func TestQuotaSupervisorRepollsStaleHealthyPeerAndKeepsObservedAt(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "quota.json")
	t.Setenv("HERD_QUOTA_CACHE_PATH", cache)
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	pinQuotaSupervisorAccounts(t, home)
	stale := time.Now().UTC().Add(-2 * time.Minute)
	var grokCalls atomic.Int32
	restore := usage.SetNativePollersForTest(map[string]func() (usage.ProviderUsage, error){
		"claude": func() (usage.ProviderUsage, error) {
			return usage.ProviderUsage{ObservedAt: stale, Resources: map[string]usage.ResourceUsage{
				"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 20, Remaining: 80, WindowSeconds: 18000},
			}}, nil
		},
		"grok": func() (usage.ProviderUsage, error) {
			grokCalls.Add(1)
			return usage.ProviderUsage{ObservedAt: stale, Resources: map[string]usage.ResourceUsage{
				"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 10, Remaining: 90, WindowSeconds: 18000},
			}}, nil
		},
	})
	t.Cleanup(restore)
	t.Cleanup(func() { usage.InvalidateSnapshotCache() })
	usage.InvalidateSnapshotCache()

	snap, err := loadQuotaSupervisorSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := quotaObservationTime(snap, "grok"); !got.Equal(stale) {
		t.Fatalf("stale grok ObservedAt rewritten: got %v want %v GeneratedAt=%v", got, stale, snap.GeneratedAt)
	}
	if grokCalls.Load() != 1 {
		t.Fatalf("first stale load grok polls=%d want 1", grokCalls.Load())
	}
	for i := 0; i < 3; i++ {
		snap, err = loadQuotaSupervisorSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if got := quotaObservationTime(snap, "grok"); !got.Equal(stale) {
			t.Fatalf("stale observation must stay %v, got %v", stale, got)
		}
	}
	if grokCalls.Load() <= 1 {
		t.Fatalf("stale healthy grok must re-poll outside TTL: polls=%d", grokCalls.Load())
	}
}

func expireClaudeBackoff(t *testing.T, cache string) {
	t.Helper()
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var wrap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrap); err != nil {
		t.Fatal(err)
	}
	var providers map[string]map[string]any
	if err := json.Unmarshal(wrap["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	rec := providers["claude"]
	if rec == nil {
		t.Fatal("no claude record")
	}
	rec["backoff_until"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	providers["claude"] = rec
	body, err := json.Marshal(map[string]any{"providers": providers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatal(err)
	}
	usage.InvalidateSnapshotCache()
}
