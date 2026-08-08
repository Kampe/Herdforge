package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fixtures are the SHAPE of real responses, captured from live calls to each
// endpoint while building FAC-229 (values edited; no tokens or account ids).

const claudeFixture = `{
  "five_hour":  {"utilization": 2.0,  "resets_at": "2026-08-08T19:00:00Z"},
  "seven_day":  {"utilization": 99.0, "resets_at": "2026-08-09T08:00:00Z"},
  "seven_day_opus": null
}`

const codexFixture = `{
  "plan_type": "pro",
  "rate_limit": {
    "allowed": true, "limit_reached": false,
    "primary_window": {"used_percent": 95, "limit_window_seconds": 604800, "reset_at": 1786309987},
    "secondary_window": null
  }
}`

const geminiFixture = `{
  "quotas": [
    {"name": "geminiSession", "limit": 100, "usage": 40, "remainingCount": 60, "resetTime": "2026-08-08T12:00:00Z"},
    {"name": "geminiWeekly",  "limit": 1000, "usage": 999, "remainingCount": 1, "resetTime": "2026-08-13T00:00:00Z"}
  ]
}`

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("missing or wrong bearer: %q", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// Utilization is a FRACTION in this package (grok divides used/total). Claude
// and codex report a PERCENT, so the pollers must divide. Mixing the scales
// would make a spent surface look 100x healthier than it is and send work at it.
func TestClaudePollNormalisesPercentToFraction(t *testing.T) {
	s := serve(t, 200, claudeFixture)
	p, err := claudePollWithURL(s.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	weekly, ok := p.Resources["weekly"]
	if !ok {
		t.Fatalf("no weekly pool: %+v", p.Resources)
	}
	if weekly.Utilization != 0.99 {
		t.Errorf("weekly utilization = %v, want 0.99 (99%% as a fraction)", weekly.Utilization)
	}
	if weekly.Used != 99 || weekly.Remaining != 1 {
		t.Errorf("weekly used/remaining = %v/%v, want 99/1", weekly.Used, weekly.Remaining)
	}
	if sess := p.Resources["session"]; sess.Utilization != 0.02 {
		t.Errorf("session utilization = %v, want 0.02", sess.Utilization)
	}
	// A null window must be absent, never a zero-utilization entry: zero reads
	// as "plenty of quota".
	if _, present := p.Resources["opusWeekly"]; present {
		t.Error("a null window must be omitted, not recorded as zero utilization")
	}
}

func TestCodexPollMapsPrimaryWindowToWeekly(t *testing.T) {
	s := serve(t, 200, codexFixture)
	p, err := codexPollWithURL(s.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if p.Plan != "pro" {
		t.Errorf("plan = %q, want pro", p.Plan)
	}
	w, ok := p.Resources["weekly"]
	if !ok {
		t.Fatalf("no weekly pool: %+v", p.Resources)
	}
	if w.Utilization != 0.95 {
		t.Errorf("utilization = %v, want 0.95", w.Utilization)
	}
	if w.WindowSeconds != 604800 {
		t.Errorf("window = %d, want 604800", w.WindowSeconds)
	}
	if w.ResetsAt == "" {
		t.Error("reset_at must be rendered as an RFC3339 timestamp")
	}
	if _, present := p.Resources["session"]; present {
		t.Error("a null secondary window must be omitted")
	}
}

func TestGeminiPollKeepsEveryNamedQuota(t *testing.T) {
	s := serve(t, 200, geminiFixture)
	p, err := geminiPollWithURL(s.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Resources) != 2 {
		t.Fatalf("resources = %d, want 2: %+v", len(p.Resources), p.Resources)
	}
	if got := p.Resources["geminiWeekly"].Utilization; got != 0.999 {
		t.Errorf("geminiWeekly utilization = %v, want 0.999", got)
	}
	if got := p.Resources["geminiSession"].Remaining; got != 60 {
		t.Errorf("geminiSession remaining = %v, want 60", got)
	}
}

// Graceful degradation: an unreachable or refusing API must ERROR, so the
// caller omits the provider. Returning an empty ProviderUsage would put a
// zero-utilization entry in the snapshot, which the router reads as healthy.
func TestPollersErrorRatherThanReportZeroQuota(t *testing.T) {
	for name, poll := range map[string]func(string, string) (ProviderUsage, error){
		"claude": claudePollWithURL,
		"codex":  codexPollWithURL,
		"gemini": geminiPollWithURL,
	} {
		t.Run(name+"/http-401", func(t *testing.T) {
			s := serve(t, 401, `{"error":"unauthorized"}`)
			if _, err := poll(s.URL, "tok"); err == nil {
				t.Fatal("a 401 must be an error, not an empty snapshot")
			}
		})
		t.Run(name+"/empty-body", func(t *testing.T) {
			s := serve(t, 200, `{}`)
			if _, err := poll(s.URL, "tok"); err == nil {
				t.Fatal("a response with no quota windows must be an error")
			}
		})
	}
}

// The whole point of FAC-229: these providers are pollable WITHOUT the
// OpenUsage macOS helper.
func TestNativePollersCoverEveryHarness(t *testing.T) {
	for _, want := range []string{"grok", "claude", "codex", "gemini"} {
		if _, ok := nativePollers[want]; !ok {
			t.Errorf("%s has no native poller; it would still need the OpenUsage binary", want)
		}
	}
}

// The live keychain returns expiresAt as a FLOAT with sub-millisecond
// precision (1786223508385.367). An int64 field failed the whole decode, so
// claude reported UNAVAILABLE while its credentials were perfectly valid. No
// fixture caught this — only polling the real keychain did.
func TestClaudeCredentialsDecodeFloatExpiry(t *testing.T) {
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string  `json:"accessToken"`
			ExpiresAt   float64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	raw := []byte(`{"claudeAiOauth":{"accessToken":"t","expiresAt":1786223508385.367}}`)
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("a fractional expiresAt must decode: %v", err)
	}
	if creds.ClaudeAiOauth.ExpiresAt < 1786223508385 {
		t.Errorf("expiry lost precision: %v", creds.ClaudeAiOauth.ExpiresAt)
	}
}

// Reading upstream OpenUsage's mappers — rather than inferring from the
// binary's strings — surfaced two pools that live ONLY in nested arrays. Both
// matter to routing: herdr-quota tracks claude/fable and codex/spark as
// independent pools, and a spent default pool does not imply a spent scoped one.
func TestClaudeExposesScopedWeeklyPools(t *testing.T) {
	body := `{
      "five_hour": {"utilization": 2.0},
      "seven_day": {"utilization": 99.0},
      "limits": [
        {"kind": "session",       "scope": null, "percent": 2},
        {"kind": "weekly_all",    "scope": null, "percent": 99},
        {"kind": "weekly_scoped", "scope": {"model": {"display_name": "Fable"}}, "percent": 78}
      ]
    }`
	s := serve(t, 200, body)
	p, err := claudePollWithURL(s.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	f, ok := p.Resources["fableWeekly"]
	if !ok {
		t.Fatalf("fable pool missing; scoped limits were dropped: %+v", p.Resources)
	}
	if f.Utilization != 0.78 || f.Remaining != 22 {
		t.Errorf("fable util/remaining = %v/%v, want 0.78/22", f.Utilization, f.Remaining)
	}
	// Unscoped entries duplicate five_hour/seven_day and must not become pools.
	if _, dup := p.Resources["weekly_all"]; dup {
		t.Error("unscoped limits entries must not be added as pools")
	}
}

func TestCodexExposesAdditionalRateLimitPools(t *testing.T) {
	body := `{
      "plan_type": "pro",
      "rate_limit": {"primary_window": {"used_percent": 95, "limit_window_seconds": 604800}},
      "additional_rate_limits": [
        {"limit_name": "GPT-5.3-Codex-Spark",
         "rate_limit": {"primary_window": {"used_percent": 13, "limit_window_seconds": 604800}}}
      ]
    }`
	s := serve(t, 200, body)
	p, err := codexPollWithURL(s.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	spark, ok := p.Resources["sparkWeekly"]
	if !ok {
		t.Fatalf("spark pool missing — codex would look 95%% spent while spark had 87%% free: %+v", p.Resources)
	}
	if spark.Utilization != 0.13 || spark.Remaining != 87 {
		t.Errorf("spark util/remaining = %v/%v, want 0.13/87", spark.Utilization, spark.Remaining)
	}
	if p.Resources["weekly"].Utilization != 0.95 {
		t.Errorf("default pool must survive alongside spark: %+v", p.Resources["weekly"])
	}
}

func TestCodexPoolKeyTakesTheModelSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"GPT-5.3-Codex-Spark": "spark",
		"spark":               "spark",
		"":                    "",
	} {
		if got := codexPoolKey(in); got != want {
			t.Errorf("codexPoolKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Credential discovery must not be macOS-only. The first implementation read
// the keychain alone, which cannot work on Linux — the portability this whole
// package exists for.
func TestCredentialDiscoveryHonoursConfigDirEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	t.Setenv("CODEX_HOME", "/custom/codex")
	t.Setenv("GEMINI_CONFIG_DIR", "/custom/gemini")

	if got := claudeCredentialFiles(); len(got) == 0 || got[0] != "/custom/claude/.credentials.json" {
		t.Errorf("CLAUDE_CONFIG_DIR must win: %v", got)
	}
	if got := codexCredentialFiles(); len(got) == 0 || got[0] != "/custom/codex/auth.json" {
		t.Errorf("CODEX_HOME must win: %v", got)
	}
	if got := geminiCredentialFiles(); len(got) == 0 || got[0] != "/custom/gemini/oauth_creds.json" {
		t.Errorf("GEMINI_CONFIG_DIR must win: %v", got)
	}
	// A file-based home must always be searched, so a Linux host with no
	// keychain can still resolve credentials.
	for _, f := range claudeCredentialFiles() {
		if strings.HasSuffix(f, "/.claude/.credentials.json") {
			return
		}
	}
	t.Error("~/.claude/.credentials.json must be a candidate; keychain-only fails on Linux")
}
