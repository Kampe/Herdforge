package usage

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureJSON = `{
  "generatedAt": "2026-08-02T07:45:53.080Z",
  "providers": {
    "claude": {
      "displayName": "Claude",
      "plan": "Max 20x",
      "resources": {
        "session": {
          "kind": "consumption",
          "limit": 100,
          "remaining": 100,
          "unit": "percent",
          "used": 0,
          "utilization": 0,
          "windowSeconds": 18000
        },
        "weekly": {
          "kind": "consumption",
          "limit": 100,
          "remaining": 20,
          "resetsAt": "2026-08-09T00:00:00Z",
          "unit": "percent",
          "used": 80,
          "utilization": 0.8,
          "windowSeconds": 604800
        }
      },
      "stale": false
    },
    "codex": {
      "displayName": "Codex",
      "plan": "Pro 5x",
      "resources": {
        "weekly": {
          "kind": "consumption",
          "limit": 100,
          "remaining": 0,
          "resetsAt": "2026-08-08T00:00:00Z",
          "unit": "percent",
          "used": 100,
          "utilization": 1,
          "windowSeconds": 604800
        }
      },
      "stale": false
    }
  },
  "schema": "openusage.limits.v1"
}`

func TestParseSnapshot(t *testing.T) {
	var snap UsageSnapshot
	if err := json.Unmarshal([]byte(fixtureJSON), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Providers == nil {
		t.Fatal("providers is nil")
	}
	claude, ok := snap.Providers["claude"]
	if !ok {
		t.Fatal("missing claude provider")
	}
	if claude.Plan != "Max 20x" {
		t.Errorf("plan: got %q", claude.Plan)
	}
}

func TestUtilization(t *testing.T) {
	var snap UsageSnapshot
	json.Unmarshal([]byte(fixtureJSON), &snap)

	u := snap.Utilization("claude")
	if u < 0.39 || u > 0.41 {
		t.Errorf("claude utilization: expected ~0.4, got %f", u)
	}

	empty := snap.Utilization("unknown")
	if empty != 0 {
		t.Errorf("expected 0 for unknown, got %f", empty)
	}
}

func TestHasCapacity(t *testing.T) {
	var snap UsageSnapshot
	json.Unmarshal([]byte(fixtureJSON), &snap)

	if !snap.HasCapacity("claude", 0.9) {
		t.Error("claude should have capacity below 0.9")
	}
	if snap.HasCapacity("codex", 0.9) {
		t.Error("codex should NOT have capacity below 0.9")
	}
	if snap.HasCapacity("unknown", 0.9) {
		t.Error("missing provider must stay UNKNOWN, not become zero-use capacity")
	}
}

func TestNilSnapshot(t *testing.T) {
	var snap *UsageSnapshot
	if snap.Utilization("anything") != 0 {
		t.Error("nil snapshot utilization should be 0")
	}
	if snap.HasCapacity("anything", 0.5) {
		t.Error("nil snapshot HasCapacity should be false")
	}
}

func TestOpenUsageProviderInstancesNormalizeWithoutCollapsingPools(t *testing.T) {
	previousNow := quotaNow
	quotaNow = func() time.Time { return time.Date(2026, 8, 30, 15, 8, 0, 0, time.UTC) }
	t.Cleanup(func() { quotaNow = previousNow })
	dir := t.TempDir()
	openusage := filepath.Join(dir, "openusage")
	snapshot := `{
  "generatedAt":"2026-08-30T15:07:21.928Z",
  "schema":"openusage.limits.v1",
  "providers":{
    "antigravity":{"displayName":"Antigravity","resources":{
      "geminiWeekly":{"kind":"consumption","limit":100,"remaining":68,"unit":"percent","used":32,"utilization":0.32,"resetsAt":"2026-09-06T05:48:13Z","windowSeconds":604800},
      "nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"unit":"percent","used":100,"utilization":1,"resetsAt":"2026-09-06T05:48:13Z","windowSeconds":604800}},"stale":false},
    "claude@8f460da5":{"displayName":"Claude","resources":{"weekly":{"kind":"consumption","limit":100,"remaining":50,"unit":"percent","used":50,"utilization":0.5,"resetsAt":"2026-09-06T05:48:13Z","windowSeconds":604800}},"stale":false},
    "codex":{"displayName":"Codex","resources":{"weekly":{"kind":"consumption","limit":100,"remaining":79,"unit":"percent","used":21,"utilization":0.21,"resetsAt":"2026-09-06T05:48:13Z","windowSeconds":604800}},"stale":false},
    "grok":{"displayName":"Grok","resources":{"weekly":{"kind":"consumption","limit":100,"remaining":100,"unit":"percent","used":0,"utilization":0,"resetsAt":"2026-09-06T05:48:13Z","windowSeconds":604800}},"stale":false}
  }
}`
	if err := os.WriteFile(openusage, []byte("#!/bin/sh\nprintf '%s\\n' '"+snapshot+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_QUOTA_HANDOFF_BIN", openusage)
	t.Setenv("HERD_QUOTA_HANDOFF_REQUIRED", "1")

	snap, err := FetchSnapshot()
	if err != nil {
		t.Fatalf("FetchSnapshot: %v", err)
	}
	if _, ok := snap.Providers["claude"]; !ok {
		t.Fatalf("suffixed Claude provider was not normalized: keys=%v", providerKeys(snap.Providers))
	}
	if _, leaked := snap.Providers["claude@8f460da5"]; leaked {
		t.Fatalf("instance-qualified provider key leaked past normalization: keys=%v", providerKeys(snap.Providers))
	}
	wantGeneratedAt := time.Date(2026, 8, 30, 15, 7, 21, 928000000, time.UTC)
	if snap.QuotaSource != QuotaSourceOpenUsageHandoff || !snap.GeneratedAt.Equal(wantGeneratedAt) {
		t.Fatalf("handoff source evidence was restamped or lost: source=%q generatedAt=%s", snap.QuotaSource, snap.GeneratedAt)
	}
	if snap.QuotaHandoffError != "" {
		t.Fatalf("fresh configured handoff was rejected: %s", snap.QuotaHandoffError)
	}

	engine := NewQuotaEngine()
	engine.Now = func() time.Time { return time.Date(2026, 8, 30, 15, 8, 0, 0, time.UTC) }
	computed := engine.ComputeAll(snap)
	if codex := computed["codex"]; !codex.Available || codex.Remaining != 79 || codex.Reason != "ok" {
		t.Fatalf("Codex 21/100 meter was not preserved as 79%% remaining: %+v", codex)
	}
	if grok := computed["grok"]; !grok.Available || grok.Remaining != 100 || grok.Reason != "ok" {
		t.Fatalf("Grok live 0/100 window was not preserved as real headroom: %+v", grok)
	}
	agy := computed["antigravity"]
	if gemini := agy.Pools["gemini"]; !gemini.Available || gemini.Remaining != 68 {
		t.Fatalf("healthy AGY Gemini pool was collapsed into its exhausted sibling: %+v", gemini)
	}
	if nonGemini := agy.Pools["nonGemini"]; nonGemini.Available || nonGemini.Reason != "exhausted" {
		t.Fatalf("exhausted AGY nonGemini pool was weakened: %+v", nonGemini)
	}
}

func TestRequiredQuotaHandoffRejectsStaleCorruptAndAmbiguousSnapshots(t *testing.T) {
	previousNow := quotaNow
	quotaNow = func() time.Time { return time.Date(2026, 8, 30, 15, 8, 0, 0, time.UTC) }
	t.Cleanup(func() { quotaNow = previousNow })
	previousPollers := nativePollers
	nativePollers = map[string]func() (ProviderUsage, error){
		"codex": func() (ProviderUsage, error) {
			return ProviderUsage{DisplayName: "Codex", Resources: map[string]ResourceUsage{
				"weekly": {Kind: "consumption", Limit: 100, Used: 21, Remaining: 79, Utilization: 0.21, Unit: "percent", WindowSeconds: WindowWeekly, ResetsAt: "2026-09-06T05:48:13Z"},
			}}, nil
		},
	}
	t.Cleanup(func() { nativePollers = previousPollers })

	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "stale source timestamp",
			body: `{"generatedAt":"2026-08-30T14:00:00Z","schema":"openusage.limits.v1","providers":{"grok":{"displayName":"Grok","resources":{"weekly":{"kind":"consumption","limit":100,"remaining":100,"unit":"percent","used":0,"utilization":0,"resetsAt":"2026-09-06T05:48:13Z","windowSeconds":604800}},"stale":false}}}`,
		},
		{
			name: "corrupt payload",
			body: `{not-json`,
		},
		{
			name: "ambiguous canonical provider",
			body: `{"generatedAt":"2026-08-30T15:07:30Z","schema":"openusage.limits.v1","providers":{"claude":{"displayName":"Claude A","resources":{},"stale":false},"claude@8f460da5":{"displayName":"Claude B","resources":{},"stale":false}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			handoff := filepath.Join(dir, "quota-handoff")
			script := "#!/bin/sh\nprintf '%s\\n' '" + tc.body + "'\n"
			if err := os.WriteFile(handoff, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HERD_QUOTA_HANDOFF_BIN", handoff)
			t.Setenv("HERD_QUOTA_HANDOFF_REQUIRED", "1")

			snap, err := FetchSnapshot()
			if err != nil {
				t.Fatalf("invalid handoff must remain structured UNKNOWN evidence: %v", err)
			}
			if snap.QuotaHandoffError == "" {
				t.Fatalf("%s handoff was accepted: %+v", tc.name, snap)
			}
			for name, state := range NewQuotaEngine().ComputeAll(snap) {
				if state.Available || state.Reason != "quota-handoff-error" {
					t.Fatalf("%s provider %s became admission authority: %+v", tc.name, name, state)
				}
			}
		})
	}
}

func TestConfiguredQuotaHandoffCommandIsBounded(t *testing.T) {
	dir := t.TempDir()
	handoff := filepath.Join(dir, "quota-handoff")
	if err := os.WriteFile(handoff, []byte("#!/bin/sh\nexec sleep 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_QUOTA_HANDOFF_BIN", handoff)
	t.Setenv("HERD_QUOTA_HANDOFF_REQUIRED", "1")
	t.Setenv("HERD_QUOTA_COMMAND_TIMEOUT_SECONDS", "1")
	previous := nativePollers
	nativePollers = map[string]func() (ProviderUsage, error){
		"codex": func() (ProviderUsage, error) {
			return ProviderUsage{DisplayName: "Codex", Resources: map[string]ResourceUsage{}}, nil
		},
	}
	t.Cleanup(func() { nativePollers = previous })

	started := time.Now()
	snap, err := FetchSnapshot()
	if err != nil {
		t.Fatalf("timeout must remain structured handoff evidence: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 1800*time.Millisecond {
		t.Fatalf("handoff command exceeded its one-second bound: %s", elapsed)
	}
	if !strings.Contains(strings.ToLower(snap.QuotaHandoffError), "timeout") {
		t.Fatalf("bounded failure did not name the timeout: %+v", snap)
	}
}

func TestRequiredWSLQuotaHandoffFailsClosedWithoutBlamingAProvider(t *testing.T) {
	dir := t.TempDir()
	openusage := filepath.Join(dir, "openusage")
	if err := os.WriteFile(openusage, []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_OPENUSAGE_BIN", openusage)
	t.Setenv("HERD_QUOTA_HANDOFF_REQUIRED", "1")

	previous := nativePollers
	nativePollers = map[string]func() (ProviderUsage, error){
		"codex": func() (ProviderUsage, error) {
			return ProviderUsage{DisplayName: "Codex", Resources: map[string]ResourceUsage{
				"weekly": {Kind: "consumption", Limit: 100, Used: 21, Remaining: 79, Utilization: 0.21, Unit: "percent", WindowSeconds: WindowWeekly, ResetsAt: "2026-09-06T05:48:13Z"},
			}}, nil
		},
	}
	t.Cleanup(func() { nativePollers = previous })

	snap, err := FetchSnapshot()
	if err != nil {
		t.Fatalf("the handoff failure must remain machine-readable, got top-level error: %v", err)
	}
	if snap.QuotaSource != "native" || !strings.Contains(snap.QuotaHandoffError, "127") {
		t.Fatalf("missing OpenUsage was not recorded as an explicit handoff failure: %+v", snap)
	}
	if snap.ProviderErrors["codex"] != "" {
		t.Fatalf("missing OpenUsage was mislabeled as a Codex provider outage: %+v", snap.ProviderErrors)
	}
	computed := NewQuotaEngine().ComputeAll(snap)
	codex := computed["codex"]
	if codex.Available || codex.Reason != "quota-handoff-error" || codex.Remaining != 79 {
		t.Fatalf("WSL native fallback silently became routing authority: %+v", codex)
	}
	if codex.ProviderError != "" || codex.QuotaHandoffError == "" {
		t.Fatalf("handoff failure and provider failure were conflated: %+v", codex)
	}
	if snap.HasCapacity("codex", 0.95) {
		t.Fatal("a failed shared-account handoff must not authorize capacity")
	}
}

func providerKeys(providers map[string]ProviderUsage) []string {
	keys := make([]string, 0, len(providers))
	for key := range providers {
		keys = append(keys, key)
	}
	return keys
}

func TestGrokBillingResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"plan":"metered-test","total":100,"used":42,"remaining":58}`))
	}))
	defer ts.Close()

	p, err := grokPollWithURL(ts.URL, "test-token")
	if err != nil {
		t.Fatalf("grok poll: %v", err)
	}
	if p.DisplayName != "Grok" {
		t.Errorf("expected Grok, got %s", p.DisplayName)
	}
	if p.Plan != "metered-test" {
		t.Errorf("plan = %q, want only the API-provided plan", p.Plan)
	}
	billing, ok := p.Resources["billing"]
	if !ok {
		t.Fatalf("real metered response has no billing resource: %+v", p.Resources)
	}
	if billing.Utilization != 0.42 || billing.Unit != "credits" {
		t.Errorf("metered billing = %+v, want 42%% of explicit credits", billing)
	}
	if billing.WindowSeconds != 0 || billing.ResetsAt != "" {
		t.Errorf("metered response fabricated a reset/window: %+v", billing)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"entitlement":"metered"`) {
		t.Fatalf("metered response omitted explicit entitlement evidence: %s", raw)
	}
}

func TestGrokBillingExhausted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":100,"used":100,"remaining":0}`))
	}))
	defer ts.Close()

	p, err := grokPollWithURL(ts.URL, "test-token")
	if err != nil {
		t.Fatalf("grok poll: %v", err)
	}
	if p.Resources["billing"].Utilization != 1.0 {
		t.Errorf("expected util 1.0 for exhausted, got %f", p.Resources["billing"].Utilization)
	}
	computed := NewQuotaEngine().ComputeAll(&UsageSnapshot{
		Providers: map[string]ProviderUsage{"grok": p},
	})
	state := computed["grok"]
	if state.Available || state.Reason != "exhausted" {
		t.Fatalf("metered exhaustion became available: %+v", state)
	}
	if state.Window != "" || state.WindowSeconds != 0 || state.ResetsAt != "" {
		t.Fatalf("metered exhaustion fabricated a temporal window: %+v", state)
	}
}

func TestGrokBillingZeroTotal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":0,"used":0,"remaining":0}`))
	}))
	defer ts.Close()

	p, err := grokPollWithURL(ts.URL, "test-token")
	if err != nil {
		t.Fatalf("grok poll: %v", err)
	}
	if len(p.Resources) != 0 {
		t.Fatalf("flat-rate response was rendered as consumption: %+v", p.Resources)
	}
	if p.Plan != "" {
		t.Fatalf("flat-rate response inherited a hard-coded plan: %q", p.Plan)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"entitlement":"unmetered"`) {
		t.Fatalf("flat-rate response omitted authenticated entitlement evidence: %s", raw)
	}
	computed := NewQuotaEngine().ComputeAll(&UsageSnapshot{
		Providers: map[string]ProviderUsage{"grok": p},
	})
	state := computed["grok"]
	if !state.Available || state.Reason != "unmetered" {
		t.Fatalf("authenticated flat-rate entitlement was not admitted explicitly: %+v", state)
	}
	if state.Window != "" || state.WindowSeconds != 0 || state.ResetsAt != "" {
		t.Fatalf("flat-rate entitlement fabricated quota window data: %+v", state)
	}
}

func TestGrokBillingFailuresRemainUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "authentication", status: http.StatusUnauthorized, body: `{"total":0,"used":0,"remaining":0}`},
		{name: "decode", status: http.StatusOK, body: `{not-json`},
		{name: "error body under 200", status: http.StatusOK, body: `{"error":"account unavailable","total":0,"used":0,"remaining":0}`},
		{name: "missing meter", status: http.StatusOK, body: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			if _, err := grokPollWithURL(ts.URL, "test-token"); err == nil {
				t.Fatal("billing failure became known availability")
			}
		})
	}

	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()
	if _, err := grokPollWithURL(url, "test-token"); err == nil {
		t.Fatal("transport failure became known availability")
	}
}

func TestFetchDirectAllPreservesEachProviderError(t *testing.T) {
	previous := nativePollers
	nativePollers = map[string]func() (ProviderUsage, error){
		"claude": func() (ProviderUsage, error) {
			return ProviderUsage{DisplayName: "Claude", Resources: map[string]ResourceUsage{}}, nil
		},
		"grok": func() (ProviderUsage, error) {
			return ProviderUsage{}, errors.New("authentication failed")
		},
	}
	t.Cleanup(func() { nativePollers = previous })

	snap, err := fetchDirectAll()
	if err != nil {
		t.Fatalf("partial aggregate failed instead of preserving healthy providers: %v", err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	errs, ok := doc["providerErrors"].(map[string]any)
	if !ok || !strings.Contains(errs["grok"].(string), "authentication") {
		t.Fatalf("aggregate silently dropped Grok failure: %s", raw)
	}
	if _, fabricated := snap.Providers["grok"]; fabricated {
		t.Fatal("failed Grok poll was also represented as known usage")
	}
	state := NewQuotaEngine().ComputeAll(snap)["grok"]
	if state.Available || state.Reason != "provider-error" || !strings.Contains(state.ProviderError, "authentication") {
		t.Fatalf("named provider error did not remain UNKNOWN in quota state: %+v", state)
	}
}

func TestFetchDirectProviderUnknown(t *testing.T) {
	_, err := fetchDirectProvider("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestGrokPollNoAuthFile(t *testing.T) {
	tmp := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmp)
	defer os.Setenv("HOME", oldHome)

	_, err := grokPoll()
	if err == nil {
		t.Fatal("expected error when no grok auth file exists")
	}
}

func TestGrokPollInvalidAuthFile(t *testing.T) {
	tmp := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmp)
	defer os.Setenv("HOME", oldHome)

	authDir := filepath.Join(tmp, ".grok")
	os.MkdirAll(authDir, 0755)
	os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`not json`), 0644)

	_, err := grokPoll()
	if err == nil {
		t.Fatal("expected error for invalid auth.json")
	}
}
