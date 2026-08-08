package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
