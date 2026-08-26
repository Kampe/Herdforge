package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendRouteDecisionTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    func(string) string
		wantErr string
	}{
		{name: "creates parent and appends", path: func(dir string) string {
			return filepath.Join(dir, "nested", "route-decisions.log")
		}},
		{name: "rejects directory", path: func(dir string) string { return dir }, wantErr: "route decision log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tc.path(dir)
			decision := &Route{
				Provider: "codex", Model: "gpt-5.6-luna", Effort: "high",
				Family: "openai", Task: "implementation", QuotaPressure: 17,
				Score: 2317, Reason: "quota pressure + task-fit penalty (weight=20)",
			}
			err := AppendRouteDecision(path, decision, func() time.Time {
				return time.Date(2026, 8, 19, 13, 41, 20, 123000000, time.UTC)
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("AppendRouteDecision error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("AppendRouteDecision: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read route log: %v", err)
			}
			var entry map[string]any
			if err := json.Unmarshal(data, &entry); err != nil {
				t.Fatalf("route log must contain one JSON object: %v", err)
			}
			if entry["timestamp"] != "2026-08-19T13:41:20.123Z" || entry["provider"] != "codex" || entry["quota_pressure"] != float64(17) || entry["reason"] == "" {
				t.Fatalf("route log entry missing durable decision fields: %v", entry)
			}
		})
	}
}

func TestAppendRouteDecisionRejectsMalformedOrUnknownExistingRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{name: "concatenated objects", data: `{"timestamp":"2026-08-19T13:41:20Z","provider":"codex","task":"implementation"}{"timestamp":"2026-08-19T13:42:20Z","provider":"codex","task":"implementation"}` + "\n", want: "malformed record"},
		{name: "unknown task shape", data: `{"timestamp":"2026-08-19T13:41:20Z","provider":"codex","task":"unknown-lane"}` + "\n", want: "unknown task shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "route-decisions.log")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			err := AppendRouteDecision(path, &Route{Provider: "codex", Task: "implementation"}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AppendRouteDecision error = %v, want %q", err, tc.want)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tc.data {
				t.Fatalf("malformed evidence changed: got %q, want %q", got, tc.data)
			}
		})
	}
}

func TestAppendRouteDecisionRejectsUnknownProvider(t *testing.T) {
	err := AppendRouteDecision(filepath.Join(t.TempDir(), "route-decisions.log"), &Route{Provider: "unknown-lane", Task: "implementation"}, nil)
	if err == nil || !strings.Contains(err.Error(), `unknown provider "unknown-lane"`) {
		t.Fatalf("AppendRouteDecision error = %v, want unknown provider", err)
	}
}
