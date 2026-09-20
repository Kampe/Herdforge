package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/usage"
)

// Use the test executable so os.Exit in the real command is observable without
// building another binary or invoking an installed provider CLI.
func TestQuotaLimitsStaleBackoffExitAndJSON(t *testing.T) {
	if mode := os.Getenv("HERD_TEST_STALE_LIMITS"); mode != "" {
		var calls int
		restore := usage.SetNativePollersForTest(map[string]func() (usage.ProviderUsage, error){
			"codex": func() (usage.ProviderUsage, error) {
				calls++
				if calls > 1 {
					return usage.ProviderUsage{}, usage.RateLimitedPollError("codex usage: HTTP 429; retry-after=600")
				}
				return usage.ProviderUsage{ObservedAt: time.Date(2026, 9, 12, 22, 6, 48, 0, time.UTC), Resources: map[string]usage.ResourceUsage{
					"primary": {Kind: "consumption", Unit: "percent", Limit: 100, Used: 62, Remaining: 38},
				}}, nil
			},
		})
		defer restore()
		if _, err := usage.FetchProviderForce("codex", true); err != nil {
			t.Fatal(err)
		}
		if mode == "persisted" {
			if _, err := usage.FetchProviderForce("codex", true); err == nil {
				t.Fatal("fixture failed to establish backoff")
			}
		}
		provider := "codex"
		if mode == "aggregate" {
			provider = ""
		}
		runQuotaLimits(provider, true)
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"new", "persisted", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota.json"))
			t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
			t.Setenv("HERD_TEST_STALE_LIMITS", mode)
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"account_id":"limits-cli-fixture"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			body, err := exec.CommandContext(ctx, exe, "-test.run=^TestQuotaLimitsStaleBackoffExitAndJSON$").Output()
			if mode == "aggregate" {
				if err != nil {
					t.Fatalf("aggregate quota limits failed: %v", err)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 4 {
					t.Fatalf("provider quota limits exit=%v want=4", err)
				}
			}
			var report usage.LimitsReport
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatalf("decode quota limits JSON: %v; output=%s", err, body)
			}
			if len(report.Errors) != 1 || !strings.HasPrefix(report.Errors["codex"], "rate-limited: ") || !strings.Contains(report.Errors["codex"], "HTTP 429; retry-after=600") {
				t.Fatalf("quota limits exit lost provider telemetry error in JSON: %v", report.Errors)
			}
			p := report.Providers["codex"]
			if report.Schema != usage.LimitsSchema || !p.Stale || p.Account == nil || p.Account.Key == "" || p.ObservedAt.Format(time.RFC3339) != "2026-09-12T22:06:48Z" || p.Resources["primary"].Used != 62 || p.Resources["primary"].Remaining != 38 {
				t.Fatalf("quota limits lost its banked reading: %+v", report)
			}
		})
	}
}
