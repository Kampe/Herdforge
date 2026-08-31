package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pinNonHandoffQuotaTest(t *testing.T) {
	t.Helper()
	t.Setenv("HERD_QUOTA_HANDOFF_REQUIRED", "0")
	t.Setenv("HERD_QUOTA_HANDOFF_BIN", "")
}

func nonHandoffQuotaEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		if strings.HasPrefix(entry, "HERD_QUOTA_HANDOFF_REQUIRED=") ||
			strings.HasPrefix(entry, "HERD_QUOTA_HANDOFF_BIN=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "HERD_QUOTA_HANDOFF_REQUIRED=0", "HERD_QUOTA_HANDOFF_BIN=")
}

func installUnmeteredUsageFixture(t *testing.T) string {
	t.Helper()
	pinNonHandoffQuotaTest(t)
	dir := t.TempDir()
	openusage := filepath.Join(dir, "openusage")
	snapshot := `{"generatedAt":"2026-08-30T15:00:00Z","providers":{"grok":{"displayName":"Grok","entitlement":"unmetered","resources":{},"stale":false}}}`
	script := "#!/bin/sh\nprintf '%s\\n' '" + snapshot + "'\n"
	if err := os.WriteFile(openusage, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return openusage
}

func TestUsageRendersFlatRateEntitlementWithoutInventedConsumption(t *testing.T) {
	openusage := installUnmeteredUsageFixture(t)
	cmd := exec.Command(buildHerd(t), "usage")
	cmd.Env = append(os.Environ(), "HERD_OPENUSAGE_BIN="+openusage)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd usage: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "entitlement="+"unmetered") {
		t.Fatalf("flat-rate entitlement was not rendered explicitly: %s", text)
	}
	for _, fabricated := range []string{"utilization=", "0/0"} {
		if strings.Contains(text, fabricated) {
			t.Errorf("flat-rate entitlement rendered fabricated consumption %q: %s", fabricated, text)
		}
	}
}

func TestQuotaJSONOmitsInventedFlatRateMeasurements(t *testing.T) {
	openusage := installUnmeteredUsageFixture(t)
	cmd := exec.Command(buildHerd(t), "quota", "--json")
	cmd.Env = append(os.Environ(), "HERD_OPENUSAGE_BIN="+openusage)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd quota --json: %v\n%s", err, out)
	}

	var providers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(out, &providers); err != nil {
		t.Fatalf("decode quota JSON: %v\n%s", err, out)
	}
	grok := providers["grok"]
	for key, want := range map[string]string{
		"available":   "true",
		"entitlement": `"unmetered"`,
		"reason":      `"unmetered"`,
	} {
		if got := string(grok[key]); got != want {
			t.Errorf("grok %s = %s, want %s; output: %s", key, got, want, out)
		}
	}
	for _, fabricated := range []string{
		"resource", "window", "windowSeconds", "used", "remaining", "resetsAt", "resetsIn", "pace", "pressure",
	} {
		if _, ok := grok[fabricated]; ok {
			t.Errorf("flat-rate quota JSON fabricated %q: %s", fabricated, out)
		}
	}
}

func TestQuotaJSONReportsRequiredRemoteHandoffFailure(t *testing.T) {
	dir := t.TempDir()
	openusage := filepath.Join(dir, "openusage")
	if err := os.WriteFile(openusage, []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(buildHerd(t), "quota", "--json")
	cmd.Env = []string{
		"HOME=" + dir,
		"PATH=" + dir,
		"HERD_OPENUSAGE_BIN=" + openusage,
		"HERD_QUOTA_HANDOFF_REQUIRED=1",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd quota --json must return structured UNKNOWN handoff evidence: %v\n%s", err, out)
	}

	var providers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(out, &providers); err != nil {
		t.Fatalf("decode quota JSON: %v\n%s", err, out)
	}
	for _, name := range []string{"antigravity", "claude", "codex", "grok"} {
		provider := providers[name]
		if got := string(provider["reason"]); got != `"quota-handoff-error"` {
			t.Errorf("%s reason = %s, want quota-handoff-error; output: %s", name, got, out)
		}
		if detail := string(provider["quotaHandoffError"]); !strings.Contains(detail, "127") {
			t.Errorf("%s omitted the exit-127 handoff evidence: %s", name, out)
		}
		if got := string(provider["available"]); got != "false" {
			t.Errorf("%s available = %s, want false after required handoff failure", name, got)
		}
	}
}

func TestQuotaJSONConsumesFreshConfiguredHandoff(t *testing.T) {
	dir := t.TempDir()
	handoff := filepath.Join(dir, "quota-handoff")
	generatedAt := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	snapshot := fmt.Sprintf(`{
  "generatedAt":%q,
  "schema":"openusage.limits.v1",
  "providers":{
    "antigravity":{"displayName":"Antigravity","resources":{
      "geminiWeekly":{"kind":"consumption","limit":100,"remaining":68,"unit":"percent","used":32,"utilization":0.32,"resetsAt":"2099-09-06T05:48:13Z","windowSeconds":604800},
      "nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"unit":"percent","used":100,"utilization":1,"resetsAt":"2099-09-06T05:48:13Z","windowSeconds":604800}},"stale":false},
    "claude@8f460da5":{"displayName":"Claude","resources":{"weekly":{"kind":"consumption","limit":100,"remaining":50,"unit":"percent","used":50,"utilization":0.5,"resetsAt":"2099-09-06T05:48:13Z","windowSeconds":604800}},"stale":false},
    "grok":{"displayName":"Grok","resources":{"weekly":{"kind":"consumption","limit":100,"remaining":100,"unit":"percent","used":0,"utilization":0,"resetsAt":"2099-09-06T05:48:13Z","windowSeconds":604800}},"stale":false}
  }
}`, generatedAt)
	if err := os.WriteFile(handoff, []byte("#!/bin/sh\nprintf '%s\\n' '"+snapshot+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(buildHerd(t), "quota", "--json")
	cmd.Env = append(os.Environ(),
		"HERD_QUOTA_HANDOFF_BIN="+handoff,
		"HERD_QUOTA_HANDOFF_REQUIRED=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd quota --json: %v\n%s", err, out)
	}

	var providers map[string]struct {
		Available        bool                       `json:"available"`
		Reason           string                     `json:"reason"`
		Remaining        float64                    `json:"remaining"`
		QuotaSource      string                     `json:"quotaSource"`
		QuotaGeneratedAt string                     `json:"quotaGeneratedAt"`
		Pools            map[string]json.RawMessage `json:"pools"`
	}
	if err := json.Unmarshal(out, &providers); err != nil {
		t.Fatalf("decode quota JSON: %v\n%s", err, out)
	}
	if _, ok := providers["claude"]; !ok {
		t.Fatalf("configured handoff did not canonicalize Claude: %s", out)
	}
	if _, leaked := providers["claude@8f460da5"]; leaked {
		t.Fatalf("instance-qualified Claude key leaked into machine quota: %s", out)
	}
	grok := providers["grok"]
	if !grok.Available || grok.Reason != "ok" || grok.Remaining != 100 {
		t.Fatalf("fresh handed-off Grok quota was not admitted: %+v", grok)
	}
	if grok.QuotaSource != "openusage-handoff" || grok.QuotaGeneratedAt != generatedAt {
		t.Fatalf("source evidence was lost or restamped: %+v", grok)
	}
	var gemini struct {
		Available bool    `json:"available"`
		Remaining float64 `json:"remaining"`
	}
	if err := json.Unmarshal(providers["antigravity"].Pools["gemini"], &gemini); err != nil {
		t.Fatalf("decode AGY Gemini pool: %v\n%s", err, out)
	}
	if !gemini.Available || gemini.Remaining != 68 {
		t.Fatalf("healthy AGY Gemini pool did not survive handoff: %+v", gemini)
	}
}
