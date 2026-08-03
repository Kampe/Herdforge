package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func writeKaneoConfig(t *testing.T, root string, cfg kaneoCLIAuthConfig) string {
	t.Helper()
	dir := filepath.Join(root, "kaneo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withUserConfigDir(t *testing.T, dir string) {
	t.Helper()
	prev := userConfigDirFn
	userConfigDirFn = func() (string, error) {
		if dir == "" {
			return "", errors.New("unset user config dir")
		}
		return dir, nil
	}
	t.Cleanup(func() { userConfigDirFn = prev })
}

// TestResolveKaneoAPIKey_UserConfigDir_macOSStyle uses an absolute Application
// Support-style root (canonical macOS UserConfigDir layout).
func TestResolveKaneoAPIKey_UserConfigDir_macOSStyle(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	root := filepath.Join(t.TempDir(), "Library", "Application Support")
	if !filepath.IsAbs(root) {
		t.Fatal("temp root must be absolute")
	}
	withUserConfigDir(t, root)
	const want = "mac-profile-key-aaaaaaaa"
	const trusted = "https://kanban-api.example.test"
	writeKaneoConfig(t, root, kaneoCLIAuthConfig{
		DefaultProfile: "default",
		Profiles: map[string]kaneoCLIProfile{
			"default": {APIKey: want, APIURL: trusted},
			"other":   {APIKey: "must-not-use-other", APIURL: "https://other.example.test"},
		},
	})
	cred := ResolveKaneoProfileCred()
	if cred.Key != want {
		t.Fatalf("want default_profile key only (len=%d), got len=%d match=%v",
			len(want), len(cred.Key), cred.Key == want)
	}
	origin, err := canonicalizeHTTPOrigin(trusted)
	if err != nil || cred.TrustedOrigin != origin {
		t.Fatalf("want trusted origin %q, got %q err=%v", origin, cred.TrustedOrigin, err)
	}
	// Never scan sibling profiles when default_profile is set.
	if cred.Key == "must-not-use-other" {
		t.Fatal("must not scan arbitrary first/other profile")
	}
	// ResolveKaneoAPIKey only returns profile key when origin pair is complete.
	if got := ResolveKaneoAPIKey(""); got != want {
		t.Fatalf("ResolveKaneoAPIKey should surface origin-bound profile key")
	}
}

// TestResolveKaneoAPIKey_UserConfigDir_LinuxStyle uses ~/.config-style root.
func TestResolveKaneoAPIKey_UserConfigDir_LinuxStyle(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	root := filepath.Join(t.TempDir(), ".config")
	withUserConfigDir(t, root)
	const want = "linux-profile-key-bbbbbbbb"
	writeKaneoConfig(t, root, kaneoCLIAuthConfig{
		DefaultProfile: "work",
		Profiles: map[string]kaneoCLIProfile{
			"work":    {APIKey: want, APIURL: "https://api.work.example"},
			"default": {APIKey: "not-selected", APIURL: "https://api.default.example"},
		},
	})
	cred := ResolveKaneoProfileCred()
	if cred.Key != want {
		t.Fatalf("want profiles[default_profile]=work key, got len=%d equal=%v", len(cred.Key), cred.Key == want)
	}
	if !strings.Contains(cred.TrustedOrigin, "api.work.example") {
		t.Fatalf("trusted origin must come from selected profile api_url, got %q", cred.TrustedOrigin)
	}
}

// TestResolveKaneoAPIKey_UnsetUserConfigDir refuses empty/relative roots.
func TestResolveKaneoAPIKey_UnsetUserConfigDir(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	withUserConfigDir(t, "")
	if got := ResolveKaneoAPIKey(""); got != "" {
		t.Fatalf("unset UserConfigDir must yield empty key, got len=%d", len(got))
	}
	// Relative root must be refused (worktree credential substitution).
	prev := userConfigDirFn
	userConfigDirFn = func() (string, error) { return ".config", nil }
	t.Cleanup(func() { userConfigDirFn = prev })
	if got := ResolveKaneoAPIKey(""); got != "" {
		t.Fatalf("relative UserConfigDir must yield empty key, got len=%d", len(got))
	}
}

// TestResolveKaneoAPIKey_MalformedConfig fails closed.
func TestResolveKaneoAPIKey_MalformedConfig(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	root := t.TempDir()
	withUserConfigDir(t, root)
	dir := filepath.Join(root, "kaneo")
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveKaneoAPIKey(""); got != "" {
		t.Fatalf("malformed config must yield empty, got len=%d", len(got))
	}
}

// TestResolveKaneoAPIKey_WrongDefaultProfile fails closed (no first-profile scan).
func TestResolveKaneoAPIKey_WrongDefaultProfile(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	root := t.TempDir()
	withUserConfigDir(t, root)
	writeKaneoConfig(t, root, kaneoCLIAuthConfig{
		DefaultProfile: "missing",
		Profiles: map[string]kaneoCLIProfile{
			"default": {APIKey: "present-but-not-selected", APIURL: "https://api.example"},
		},
	})
	if cred := ResolveKaneoProfileCred(); cred.Key != "" {
		t.Fatalf("missing default_profile must not fall back to another profile, got len=%d", len(cred.Key))
	}
	// Empty default_profile also fails closed.
	writeKaneoConfig(t, root, kaneoCLIAuthConfig{
		DefaultProfile: "",
		Profiles: map[string]kaneoCLIProfile{
			"default": {APIKey: "present-but-no-default_profile", APIURL: "https://api.example"},
		},
	})
	if cred := ResolveKaneoProfileCred(); cred.Key != "" {
		t.Fatalf("empty default_profile must yield empty, got len=%d", len(cred.Key))
	}
	// Profile key without api_url is unusable (no trusted origin).
	writeKaneoConfig(t, root, kaneoCLIAuthConfig{
		DefaultProfile: "default",
		Profiles: map[string]kaneoCLIProfile{
			"default": {APIKey: "key-without-url"},
		},
	})
	if cred := ResolveKaneoProfileCred(); cred.Key != "" || cred.TrustedOrigin != "" {
		t.Fatalf("key without api_url must be unusable, got keyLen=%d origin=%q", len(cred.Key), cred.TrustedOrigin)
	}
	// Top-level legacy {"default":{api_key}} must NOT be accepted.
	legacy := []byte(`{"default":{"api_key":"legacy-top-level","api_url":"https://evil.example"}}`)
	if err := os.WriteFile(filepath.Join(root, "kaneo", "config.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if cred := ResolveKaneoProfileCred(); cred.Key != "" {
		t.Fatalf("legacy top-level default.api_key must not be accepted, got len=%d", len(cred.Key))
	}
}

// TestResolveKaneoAPIKey_EnvOverridesProfile.
func TestResolveKaneoAPIKey_EnvOverridesProfile(t *testing.T) {
	root := t.TempDir()
	withUserConfigDir(t, root)
	writeKaneoConfig(t, root, kaneoCLIAuthConfig{
		DefaultProfile: "default",
		Profiles: map[string]kaneoCLIProfile{
			"default": {APIKey: "from-profile", APIURL: "https://profile.example"},
		},
	})
	t.Setenv("KANEO_API_KEY", "from-env")
	if got := ResolveKaneoAPIKey(""); got != "from-env" {
		t.Fatalf("env must win, got len=%d", len(got))
	}
	if got := ResolveKaneoAPIKey("explicit"); got != "explicit" {
		t.Fatalf("override must win, got len=%d", len(got))
	}
}

// TestResolveKaneoAPIKey_NoCredential_ListProjectRelationsFailClosed ties
// resolver emptiness to production graph snapshot refuse (no CLI fan-out).
func TestResolveKaneoAPIKey_NoCredential_ListProjectRelationsFailClosed(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	withUserConfigDir(t, t.TempDir()) // empty dir, no config.json
	k := NewKaneoProvider("https://kanban-api.example.test", "proj", true)
	k.APIKey = ""
	if k.resolvedAPIKey() != "" {
		t.Fatal("expected empty resolved key")
	}
	_, err := k.ListProjectRelations(context.Background(), "proj")
	if err == nil {
		t.Fatal("expected fail-closed")
	}
	if !errors.Is(err, ErrGraphCredentialsRequired) && !strings.Contains(err.Error(), "refusing silent CLI") {
		t.Fatalf("want ErrGraphCredentialsRequired, got %v", err)
	}
}

// TestKaneoCLIConfigPath_IsAbsolute under real UserConfigDir.
func TestKaneoCLIConfigPath_IsAbsolute(t *testing.T) {
	// Restore real UserConfigDir for this test.
	userConfigDirFn = os.UserConfigDir
	t.Cleanup(func() { userConfigDirFn = os.UserConfigDir })
	path, err := kaneoCLIConfigPath()
	if err != nil {
		// Some CI images may lack UserConfigDir; skip only then.
		t.Skipf("UserConfigDir unavailable: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("config path must be absolute, got %q", path)
	}
	if !strings.HasSuffix(filepath.ToSlash(path), "/kaneo/config.json") {
		t.Fatalf("want .../kaneo/config.json suffix, got %q", path)
	}
}

// TestAuthorizeKaneo_ProfileKeyNotExfiltratedToMaliciousRepoURL proves a
// repository-configured APIURL (evil host) cannot receive the global profile
// bearer token. No Authorization header and no credential bytes on the wire.
func TestAuthorizeKaneo_ProfileKeyNotExfiltratedToMaliciousRepoURL(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	root := t.TempDir()
	withUserConfigDir(t, root)

	const profileKey = "profile-secret-key-DO-NOT-LEAK"
	const trustedAPI = "https://kanban-api.trusted.example"
	writeKaneoConfig(t, root, kaneoCLIAuthConfig{
		DefaultProfile: "default",
		Profiles: map[string]kaneoCLIProfile{
			"default": {APIKey: profileKey, APIURL: trustedAPI},
		},
	})
	cred := ResolveKaneoProfileCred()
	if cred.Key != profileKey {
		t.Fatal("profile cred must load for test setup")
	}

	var sawAuth atomic.Bool
	var bodies strings.Builder
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "" {
			sawAuth.Store(true)
		}
		// Capture any credential bytes in headers or body for exfil proof.
		for k, vs := range r.Header {
			for _, v := range vs {
				bodies.WriteString(k + ":" + v + "\n")
			}
		}
		b, _ := io.ReadAll(r.Body)
		bodies.Write(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(evil.Close)

	// Repo-controlled malicious API URL; no explicit APIKey (would use profile if unbound).
	k := NewKaneoProvider(evil.URL, "proj", false)
	k.APIKey = ""
	// PreferHTTP must be false: evil origin ≠ profile trusted origin.
	if k.preferHTTPForRelations() {
		t.Fatal("must not claim HTTP credentials for malicious repo APIURL")
	}
	if k.credentialForAPIURL() != "" {
		t.Fatal("credentialForAPIURL must not return profile key for foreign origin")
	}

	// Direct authorize on a request to evil.
	req, err := http.NewRequest(http.MethodGet, evil.URL+"/api/task-relation/t1", nil)
	if err != nil {
		t.Fatal(err)
	}
	k.authorizeKaneo(req)
	if req.Header.Get("Authorization") != "" {
		t.Fatal("Authorization must not be set for foreign origin")
	}

	// Live HTTP path: list relations against evil host.
	_, _ = k.listRelationsHTTPOnly(context.Background(), "t1")
	if sawAuth.Load() {
		t.Fatal("evil server must not receive Authorization header")
	}
	wire := bodies.String()
	if strings.Contains(wire, profileKey) {
		t.Fatal("profile credential bytes must not appear on malicious origin wire")
	}
	if strings.Contains(wire, "Bearer ") {
		t.Fatal("no Bearer credentials may leave the profile origin")
	}

	// Positive control: same profile key IS attached when APIURL is trusted origin.
	// Use httptest with TLS-like path via rewriting — bind APIURL origin to a local
	// server by using the profile URL host is not local; instead set APIURL to
	// trusted and prove credentialForRequestOrigin matches, and authorize on a
	// synthetic request URL with that origin.
	trustedOrigin, err := canonicalizeHTTPOrigin(trustedAPI)
	if err != nil {
		t.Fatal(err)
	}
	if k2 := (&KaneoProvider{APIURL: trustedAPI, APIKey: ""}); k2.credentialForRequestOrigin(trustedOrigin) != profileKey {
		t.Fatal("profile key must authorize exact trusted origin")
	}
	// Port-equivalent: https://host vs https://host:443
	alt := "https://kanban-api.trusted.example:443/api/task-relation/x"
	req2, _ := http.NewRequest(http.MethodGet, alt, nil)
	(&KaneoProvider{APIURL: trustedAPI}).authorizeKaneo(req2)
	if got := req2.Header.Get("Authorization"); got != "Bearer "+profileKey {
		t.Fatalf("same-origin :443 must authorize; got auth present=%v", got != "")
	}
	// userinfo rejected
	if _, err := canonicalizeHTTPOrigin("https://user:pass@kanban-api.trusted.example/"); err == nil {
		t.Fatal("userinfo must be rejected")
	}
	// Cross-origin with userinfo-shaped host still no auth on evil
	req3, _ := http.NewRequest(http.MethodGet, evil.URL, nil)
	(&KaneoProvider{APIURL: trustedAPI}).authorizeKaneo(req3)
	if req3.Header.Get("Authorization") != "" {
		t.Fatal("trusted APIURL must not attach profile key to evil request")
	}
}

// TestAuthorizeKaneo_EnvKeyBoundToOperatorOrigin proves repository APIURL
// mutation cannot move a global environment key to a different origin.
func TestAuthorizeKaneo_EnvKeyBoundToOperatorOrigin(t *testing.T) {
	withUserConfigDir(t, t.TempDir())

	var sawAuth atomic.Bool
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(good.Close)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			t.Errorf("evil must not see Authorization")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(evil.Close)

	const key = "explicit-operator-key"
	t.Setenv("KANEO_API_KEY", key)
	t.Setenv("KANEO_API_URL", good.URL)
	k := NewKaneoProvider(good.URL, "proj", false)
	if k.credentialForAPIURL() != key {
		t.Fatal("env key must authorize the independently trusted operator origin")
	}
	_, _ = k.listRelationsHTTPOnly(context.Background(), "t1")
	if !sawAuth.Load() {
		t.Fatal("good origin must receive Authorization for explicit key")
	}

	// Repository mutation cannot move the already-bound environment key.
	k.APIURL = evil.URL
	if k.credentialForAPIURL() != "" {
		t.Fatal("repository APIURL mutation must not move operator key authority")
	}
	_, _ = k.listRelationsHTTPOnly(context.Background(), "t1")

	// Direct foreign-origin authorization also remains empty.
	k2 := &KaneoProvider{
		APIURL: good.URL, APIKey: key, KeyTrustedOrigin: good.URL, Client: kaneoHTTPClient(),
	}
	req, _ := http.NewRequest(http.MethodGet, evil.URL+"/api/x", nil)
	k2.authorizeKaneo(req)
	if req.Header.Get("Authorization") != "" {
		t.Fatal("explicit key must not authorize foreign request origin")
	}
}

func TestNewFromHerdConfig_CustomKeyEnvUsesOperatorOrigin(t *testing.T) {
	withUserConfigDir(t, t.TempDir())
	t.Setenv("FAC159_TEST_KANEO_KEY", "custom-env-key")
	t.Setenv("KANEO_API_KEY", "")
	t.Setenv("KANEO_API_URL", "https://operator.example.test")

	tp, err := NewFromHerdConfig(&config.Config{TaskProvider: config.TaskProvider{
		Type: "kaneo", ProjectID: "proj", UseCLI: true,
		APIURL: "https://repo-controlled.example.test", APIKeyEnv: "FAC159_TEST_KANEO_KEY",
	}})
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := tp.(*BoundClient)
	if !ok {
		t.Fatalf("want BoundClient, got %T", tp)
	}
	k, ok := bound.Inner.(*KaneoProvider)
	if !ok {
		t.Fatalf("want KaneoProvider, got %T", bound.Inner)
	}
	wantOrigin, _ := canonicalizeHTTPOrigin("https://operator.example.test")
	if k.APIKey != "custom-env-key" || k.KeyTrustedOrigin != wantOrigin {
		t.Fatalf("custom env key/origin binding mismatch: key=%v origin=%q", k.APIKey != "", k.KeyTrustedOrigin)
	}
	if k.credentialForAPIURL() != "" {
		t.Fatal("repo-controlled APIURL must not be authorized by custom env key")
	}
}

// TestCanonicalizeHTTPOrigin_EffectivePortAndRejects.
func TestCanonicalizeHTTPOrigin_EffectivePortAndRejects(t *testing.T) {
	o1, err := canonicalizeHTTPOrigin("https://Example.COM/path")
	if err != nil {
		t.Fatal(err)
	}
	o2, err := canonicalizeHTTPOrigin("https://example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if o1 != o2 {
		t.Fatalf("effective port must match: %q vs %q", o1, o2)
	}
	if _, err := canonicalizeHTTPOrigin("https://user:secret@example.com/"); err == nil {
		t.Fatal("userinfo must fail")
	}
	if _, err := canonicalizeHTTPOrigin("ftp://example.com/"); err == nil {
		t.Fatal("non-http scheme must fail")
	}
}
