package usage

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolatedHome points HOME (and clears the config-dir overrides) at a temp
// directory so identity resolution never reads this machine's real accounts.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", home)
	return home
}

// The opaque key is the ONLY account material that ever leaves this package.
// It must be deterministic, distinct per account, and must not embed the claim.
func TestOpaqueAccountKeyIsStableAndNonReversible(t *testing.T) {
	a1 := opaqueAccountKey("claude", "uuid-A")
	if a1 == "" || len(a1) != 10 {
		t.Fatalf("key shape wrong: %q", a1)
	}
	if opaqueAccountKey("claude", "uuid-A") != a1 {
		t.Error("same claim must yield the same key across calls (and across token refreshes)")
	}
	if opaqueAccountKey("claude", "uuid-B") == a1 {
		t.Error("different accounts must yield different keys")
	}
	if opaqueAccountKey("codex", "uuid-A") == a1 {
		t.Error("keys must be namespaced per provider")
	}
	if strings.Contains(a1, "uuid-A") {
		t.Error("the key must not embed the raw claim")
	}
}

func writeJSONFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAccountIdentityFromConfig(t *testing.T) {
	home := isolatedHome(t)
	writeJSONFile(t, filepath.Join(home, ".claude.json"),
		`{"oauthAccount":{"accountUuid":"11111111-2222-3333-4444-555555555555"}}`)
	id := claudeAccountIdentity()
	if id == nil {
		t.Fatal("a logged-in claude config must yield an identity")
	}
	if id.Key != opaqueAccountKey("claude", "11111111-2222-3333-4444-555555555555") {
		t.Errorf("key = %q, want digest of the account uuid", id.Key)
	}
	if id.Provenance != "claude-config:.claude.json:oauthAccount.accountUuid" {
		t.Errorf("provenance must name the exact claim source, got %q", id.Provenance)
	}
	if strings.Contains(id.Key, "11111111") {
		t.Error("raw uuid must never appear in the key")
	}
}

func TestClaudeAccountIdentityUnknownWithoutClaim(t *testing.T) {
	home := isolatedHome(t)
	writeJSONFile(t, filepath.Join(home, ".claude.json"), `{"other": true}`)
	if claudeAccountIdentity() != nil {
		t.Error("no oauthAccount claim means identity is unknown, not guessed")
	}
}

// Refreshing a token must not fork the billing account: the identity comes
// from the account claim, never from token material.
func TestTokenRefreshDoesNotForkAccount(t *testing.T) {
	home := isolatedHome(t)
	writeJSONFile(t, filepath.Join(home, ".claude.json"),
		`{"oauthAccount":{"accountUuid":"11111111-2222-3333-4444-555555555555"}}`)
	before := claudeAccountIdentity()

	// A fresh OAuth token pair for the same account (different expiry, same uuid).
	writeJSONFile(t, filepath.Join(home, ".claude", ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"totally-different-token","expiresAt":9999999999999.5}}`)
	after := claudeAccountIdentity()
	if before == nil || after == nil || before.Key != after.Key {
		t.Fatalf("same account, new token: keys forked (%v -> %v)", before, after)
	}
}

func TestCodexAccountIdentityHonoursConfigDir(t *testing.T) {
	home := isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeJSONFile(t, filepath.Join(dir, "auth.json"),
		`{"tokens":{"access_token":"t","account_id":"acct-codex-1"}}`)
	// A decoy in HOME must not win over the explicit override.
	writeJSONFile(t, filepath.Join(home, ".codex", "auth.json"),
		`{"tokens":{"access_token":"t","account_id":"acct-codex-DECOY"}}`)
	id := codexAccountIdentity()
	if id == nil || id.Key != opaqueAccountKey("codex", "acct-codex-1") {
		t.Fatalf("CODEX_HOME account claim must win, got %+v", id)
	}
	if id.Provenance != "codex-auth:auth.json:tokens.account_id" {
		t.Errorf("provenance = %q", id.Provenance)
	}
}

func TestGrokAccountIdentitySingleEntryOnly(t *testing.T) {
	home := isolatedHome(t)
	writeJSONFile(t, filepath.Join(home, ".grok", "auth.json"),
		`{"some-account-label":{"key":"tok","refresh_token":"r"}}`)
	if id := grokAccountIdentity(); id == nil || id.Key != opaqueAccountKey("grok", "some-account-label") {
		t.Fatalf("single-entry auth must bind to the entry key, got %+v", id)
	}

	// Two profiles are ambiguous: quota must never be attributed to a random one.
	writeJSONFile(t, filepath.Join(home, ".grok", "auth.json"),
		`{"label-a":{"key":"tok"},"label-b":{"key":"tok2"}}`)
	if grokAccountIdentity() != nil {
		t.Error("multiple auth entries are ambiguous and must stay unknown")
	}
}

// gemini identity comes from the id_token sub claim: the JWT payload is decoded
// locally, never verified, never printed.
func TestGeminiAccountIdentityFromIDTokenSubject(t *testing.T) {
	home := isolatedHome(t)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"google-sub-123","email":"user@example.com"}`))
	writeJSONFile(t, filepath.Join(home, ".gemini", "oauth_creds.json"),
		fmt.Sprintf(`{"access_token":"t","id_token":"head.%s.sig","expiry_date":9999999999999}`, payload))
	id := geminiAccountIdentity()
	if id == nil || id.Key != opaqueAccountKey("gemini", "google-sub-123") {
		t.Fatalf("identity must come from the sub claim, got %+v", id)
	}
	if id.Provenance != "gemini-oauth:oauth_creds.json:id_token.sub" {
		t.Errorf("provenance = %q", id.Provenance)
	}
	if strings.Contains(id.Key, "google-sub") || strings.Contains(id.Key, "user") {
		t.Error("neither the subject nor the email may appear in the key")
	}
}

func TestGeminiAccountIdentityMalformedTokenUnknown(t *testing.T) {
	home := isolatedHome(t)
	writeJSONFile(t, filepath.Join(home, ".gemini", "oauth_creds.json"),
		`{"access_token":"t","id_token":"not-a-jwt"}`)
	if geminiAccountIdentity() != nil {
		t.Error("a malformed id_token leaves identity unknown, never guessed")
	}
}

func TestProviderAccountIdentityUnknownProviders(t *testing.T) {
	isolatedHome(t)
	for _, name := range []string{"antigravity", "agy", "opencode", "kimi", ""} {
		if providerAccountIdentity(name) != nil {
			t.Errorf("provider %q has no native identity source; it must be unknown", name)
		}
	}
}

// --- identity-bound persisted cache ---

func cachedAccountSnapshot(t *testing.T, accountUUID string) (*UsageSnapshot, *AccountIdentity) {
	t.Helper()
	acc := &AccountIdentity{
		Key:        opaqueAccountKey("claude", accountUUID),
		Provenance: "claude-config:.claude.json:oauthAccount.accountUuid",
	}
	return &UsageSnapshot{
		GeneratedAt: time.Now().UTC(),
		Providers: map[string]ProviderUsage{
			"claude": {DisplayName: "Claude", Account: acc, Stale: false,
				Resources: map[string]ResourceUsage{
					"weekly": {Kind: "consumption", Unit: "percent", Used: 10, Remaining: 90, Limit: 100, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800},
				}},
		},
	}, acc
}

// A local Claude config is only a hint; it cannot authorize cache reuse.
func TestCacheRejectsUnverifiedClaudeIdentity(t *testing.T) {
	home := isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "q.json"))
	const uuid = "11111111-2222-3333-4444-555555555555"
	writeJSONFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"`+uuid+`"}}`)
	snap, acc := cachedAccountSnapshot(t, uuid)
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: time.Now(), Snapshot: snap})
	if err := os.WriteFile(filepath.Join(dir, "q.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Fatal("local Claude config hint must not authorize persisted cache reuse")
	}
	_ = acc
}

// Changing accounts must never reuse the old account's cached quota.
func TestCacheRefusedAfterAccountSwitch(t *testing.T) {
	home := isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "q.json"))
	const oldUUID = "11111111-2222-3333-4444-555555555555"
	const newUUID = "99999999-8888-7777-6666-555555555555"
	snap, _ := cachedAccountSnapshot(t, oldUUID)
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: time.Now(), Snapshot: snap})
	if err := os.WriteFile(filepath.Join(dir, "q.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"`+newUUID+`"}}`)
	if got, _, ok := readSnapshotFile(time.Minute); ok {
		t.Fatalf("a proven account switch must invalidate the whole reading, got %+v", got.Providers)
	}
}

// Unknown identity on either side drops only that provider; other provable
// providers stay served, and if nothing is provable nothing is served.
func TestCacheDropsUnprovableProviders(t *testing.T) {
	home := isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "q.json"))
	const uuid = "11111111-2222-3333-4444-555555555555"
	writeJSONFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"`+uuid+`"}}`)
	acc := &AccountIdentity{Key: opaqueAccountKey("claude", uuid), Provenance: "claude-config:.claude.json:oauthAccount.accountUuid"}
	snap := &UsageSnapshot{
		GeneratedAt: time.Now().UTC(),
		Providers: map[string]ProviderUsage{
			"claude": {DisplayName: "Claude", Account: acc, Resources: map[string]ResourceUsage{
				"weekly": {Kind: "consumption", Unit: "percent", Used: 10, Limit: 100, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800}}},
			// gemini identity is unprovable in this HOME: it must be dropped,
			// reading as unknown rather than possibly-another-account.
			"gemini": {DisplayName: "Gemini", Resources: map[string]ResourceUsage{
				"geminiWeekly": {Kind: "consumption", Unit: "requests", Used: 1, Limit: 100, ResetsAt: "2099-01-01T00:00:00Z"}}},
		},
	}
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: time.Now(), Snapshot: snap})
	if err := os.WriteFile(filepath.Join(dir, "q.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, ok := readSnapshotFile(time.Minute)
	if ok {
		t.Fatal("a Claude reading remains unusable until its credential is profile-bound")
	}
}

// --- parallel bounded polling ---

// fetchDirectAll must poll providers concurrently: serial polling is what made
// a full fetch cost four 10-second deadlines. Each fake poller waits at a
// barrier that only releases once every provider has arrived; a serial loop
// would time out waiting.
func TestFetchDirectAllPollsProvidersInParallel(t *testing.T) {
	isolatedHome(t)
	saved := make(map[string]func() (ProviderUsage, error), len(nativePollers))
	for k, v := range nativePollers {
		saved[k] = v
	}
	t.Cleanup(func() {
		nativePollers = saved
	})

	const n = 4
	arrived := make(chan string, n)
	release := make(chan struct{})
	fake := func(name string) func() (ProviderUsage, error) {
		return func() (ProviderUsage, error) {
			arrived <- name
			<-release
			return ProviderUsage{
				DisplayName: name,
				Resources: map[string]ResourceUsage{
					"weekly": {Kind: "consumption", Unit: "percent", Used: 5, Remaining: 95, Limit: 100, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800},
				},
			}, nil
		}
	}
	for name := range nativePollers {
		nativePollers[name] = fake(name)
	}

	done := make(chan struct{})
	var snap *UsageSnapshot
	var err error
	go func() {
		snap, err = fetchDirectAll()
		close(done)
	}()

	seen := 0
	timeout := time.After(5 * time.Second)
	for seen < n {
		select {
		case <-arrived:
			seen++
		case <-timeout:
			close(release)
			<-done
			t.Fatalf("only %d of %d pollers ran concurrently; fetchDirectAll is serial", seen, n)
			return
		}
	}
	close(release)
	<-done

	if err != nil {
		t.Fatalf("all-healthy parallel fetch must succeed: %v", err)
	}
	if len(snap.Providers) != n {
		t.Fatalf("providers = %d, want %d", len(snap.Providers), n)
	}
	for name, pu := range snap.Providers {
		if pu.Source != providerSource[name] {
			t.Errorf("%s source = %q, want %q", name, pu.Source, providerSource[name])
		}
		// HOME is an empty tempdir: identity is unknown and must be absent,
		// never fabricated.
		if pu.Account != nil {
			t.Errorf("%s account identity must be nil when unprovable, got %+v", name, pu.Account)
		}
	}
}

// A provider failure records a machine-readable reason and keeps the others.
func TestFetchDirectAllRecordsPartialErrors(t *testing.T) {
	isolatedHome(t)
	saved := make(map[string]func() (ProviderUsage, error), len(nativePollers))
	for k, v := range nativePollers {
		saved[k] = v
	}
	t.Cleanup(func() { nativePollers = saved })

	healthy := func() (ProviderUsage, error) {
		return ProviderUsage{
			DisplayName: "ok",
			Resources: map[string]ResourceUsage{
				"weekly": {Kind: "consumption", Unit: "percent", Used: 5, Remaining: 95, Limit: 100, ResetsAt: "2099-01-01T00:00:00Z", WindowSeconds: 604800},
			},
		}, nil
	}
	nativePollers["claude"] = func() (ProviderUsage, error) {
		return ProviderUsage{}, pollErrf("auth-expired", "claude credentials expired; re-authenticate with the claude CLI")
	}
	nativePollers["codex"] = func() (ProviderUsage, error) {
		return ProviderUsage{}, httpStatusPollError("codex usage", 401)
	}
	nativePollers["grok"] = healthy
	nativePollers["gemini"] = healthy

	snap, err := fetchDirectAll()
	if err != nil {
		t.Fatalf("healthy providers keep the snapshot usable: %v", err)
	}
	if _, ok := snap.Providers["claude"]; ok {
		t.Error("a failed provider must not appear in providers")
	}
	if len(snap.Providers) != 2 {
		t.Fatalf("providers = %d, want the 2 healthy ones: %+v", len(snap.Providers), snap.Providers)
	}
	if !strings.HasPrefix(snap.Errors["claude"], "auth-expired: ") {
		t.Errorf("claude error = %q, want auth-expired prefix", snap.Errors["claude"])
	}
	if !strings.HasPrefix(snap.Errors["codex"], "http-401: ") {
		t.Errorf("codex error = %q, want http-401 prefix", snap.Errors["codex"])
	}
	if snap.Errors["grok"] != "" || snap.Errors["gemini"] != "" {
		t.Error("healthy providers must not carry error entries")
	}
}

// --- error classification ---

type fakeNetErr struct{ timeout bool }

func (e fakeNetErr) Error() string   { return "fake net error" }
func (e fakeNetErr) Timeout() bool   { return e.timeout }
func (e fakeNetErr) Temporary() bool { return false }

func TestClassifyPollErrorCodes(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{httpStatusPollError("x", 401), "http-401"},
		{httpStatusPollError("x", 429), "http-429"},
		{pollErrf("decode-failed", "bad json"), "decode-failed"},
		{netPollError(fakeNetErr{timeout: true}), "timeout"},
		{netPollError(fakeNetErr{timeout: false}), "unreachable"},
		{netPollError(errors.New("boom")), "unreachable"},
		{fmt.Errorf("legacy error"), "unknown"},
	}
	for _, tc := range cases {
		if got := pollErrorCode(tc.err); got != tc.want {
			t.Errorf("pollErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	var ne net.Error
	if !errors.As(netPollError(fakeNetErr{timeout: true}), &ne) || !ne.Timeout() {
		t.Error("net classification must preserve the net.Error chain")
	}
}

// --- limits report ---

func TestLimitsReportShape(t *testing.T) {
	var nilSnap *UsageSnapshot
	r := nilSnap.LimitsReport(0)
	if r.Schema != LimitsSchema {
		t.Errorf("schema = %q, want %q", r.Schema, LimitsSchema)
	}
	if r.Providers == nil || len(r.Providers) != 0 {
		t.Errorf("nil snapshot must render empty providers, not null: %+v", r.Providers)
	}
	snap := &UsageSnapshot{
		GeneratedAt: time.Date(2026, 9, 9, 19, 30, 0, 0, time.UTC),
		Providers:   map[string]ProviderUsage{"claude": {DisplayName: "Claude"}},
		Errors:      map[string]string{"grok": "auth-missing: no auth.json"},
	}
	r = snap.LimitsReport(12 * time.Second)
	if r.CacheAgeSeconds != 12 {
		t.Errorf("cacheAgeSeconds = %v, want 12", r.CacheAgeSeconds)
	}
	if r.Providers["claude"].DisplayName != "Claude" || r.Errors["grok"] == "" {
		t.Errorf("report must carry providers and errors verbatim: %+v", r)
	}
}
