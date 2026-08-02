package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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

func TestGrokBillingResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":100,"used":42,"remaining":58}`))
	}))
	defer ts.Close()

	p, err := grokPollWithURL(ts.URL, "test-token")
	if err != nil {
		t.Fatalf("grok poll: %v", err)
	}
	if p.DisplayName != "Grok" {
		t.Errorf("expected Grok, got %s", p.DisplayName)
	}
	if p.Resources["weekly"].Utilization != 0.42 {
		t.Errorf("expected util 0.42, got %f", p.Resources["weekly"].Utilization)
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
	if p.Resources["weekly"].Utilization != 1.0 {
		t.Errorf("expected util 1.0 for exhausted, got %f", p.Resources["weekly"].Utilization)
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
	if p.Resources["weekly"].Utilization != 0 {
		t.Errorf("expected util 0 for zero total, got %f", p.Resources["weekly"].Utilization)
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
