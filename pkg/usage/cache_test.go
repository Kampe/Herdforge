package usage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderCacheSubprocessHelper(t *testing.T) {
	if os.Getenv("HERD_CACHE_SUBPROCESS_HELPER") != "1" {
		return
	}
	url := os.Getenv("HERD_CACHE_FIXTURE_URL")
	restore := SetNativePollersForTest(map[string]func() (ProviderUsage, error){"codex": func() (ProviderUsage, error) {
		resp, err := http.Get(url)
		if err != nil {
			return ProviderUsage{}, err
		}
		_ = resp.Body.Close()
		return ProviderUsage{DisplayName: "Codex", Account: codexAccountIdentity(), Resources: map[string]ResourceUsage{
			"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Remaining: 80, WindowSeconds: 18000},
		}}, nil
	}})
	defer restore()
	if _, err := FetchProviderForce("codex", false); err != nil {
		t.Fatal(err)
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
	cmds := make([]*exec.Cmd, 2)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestProviderCacheSubprocessHelper$", "-test.v")
		cmds[i].Env = baseEnv
	}
	results := make(chan error, len(cmds))
	for _, cmd := range cmds {
		go func(cmd *exec.Cmd) {
			output, err := cmd.CombinedOutput()
			if err != nil {
				results <- fmt.Errorf("helper failed: %w: %s", err, strings.TrimSpace(string(output)))
				return
			}
			results <- nil
		}(cmd)
	}
	for range cmds {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cross-process cache lock allowed %d fixture requests, want 1", got)
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
	InvalidateSnapshotCache()
	home := t.TempDir()
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
	InvalidateSnapshotCache()
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
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
	InvalidateSnapshotCache()
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
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
	InvalidateSnapshotCache()
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
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

// Invalidation forces the next decision to refetch, so anything that materially
// changes quota is not followed by a decision acting on the number it just
// invalidated.
func TestInvalidateForcesARefetch(t *testing.T) {
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
