package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGetHarnessConfig_GenericFallback(t *testing.T) {
	cfg := GetHarnessConfig("unknown-harness")
	if cfg.Type != HarnessGeneric {
		t.Errorf("expected HarnessGeneric, got %s", cfg.Type)
	}
	if cfg.BinaryName != "unknown-harness" {
		t.Errorf("expected binary unknown-harness, got %s", cfg.BinaryName)
	}
}

func TestBuildInvocation_NoPromptFlag(t *testing.T) {
	cfg := &HarnessConfig{
		Type:       HarnessGeneric,
		BinaryName: "my-tool",
		PromptFlag: "",
		Supported:  true,
	}
	inv := cfg.BuildInvocation("do it")
	expected := []string{"my-tool", "do it"}
	if len(inv) != 2 || inv[0] != "my-tool" || inv[1] != "do it" {
		t.Errorf("BuildInvocation() = %v, expected %v", inv, expected)
	}
}

func TestLookPath_NotFound(t *testing.T) {
	cfg := &HarnessConfig{BinaryName: "this-binary-should-not-exist-xyzzy"}
	_, err := cfg.LookPath()
	if err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCheckHooksDeterministicClassificationAndIdentity(t *testing.T) {
	var gotIdentity HookIdentity
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = HookIdentity{
			Provider: r.Header.Get("X-Herd-Provider"),
			Model:    r.Header.Get("X-Herd-Model"),
			Effort:   r.Header.Get("X-Herd-Effort"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer server.Close()

	report := CheckHooks(context.Background(), []Hook{
		{Name: "z-optional", URL: server.URL + "/z", Requirement: HookOptional},
		{Name: "a-required", URL: server.URL + "/a", Requirement: HookRequired},
	}, HookIdentity{Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium"}, server.Client())
	if !report.RequiredHealthy || report.DegradedWarning != "" {
		t.Fatalf("healthy hooks report = %+v", report)
	}
	if len(report.Results) != 2 || report.Results[0].Hook.Name != "a-required" || report.Results[1].Hook.Name != "z-optional" {
		t.Fatalf("results are not deterministic: %+v", report.Results)
	}
	if gotIdentity != (HookIdentity{Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium"}) {
		t.Fatalf("hook identity = %+v", gotIdentity)
	}
}

func TestCheckHooksFixtures(t *testing.T) {
	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer valid.Close()
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer malformed.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer slow.Close()

	cases := []struct {
		name   string
		hook   Hook
		status HookStatus
	}{
		{name: "healthy", hook: Hook{Name: "healthy", URL: valid.URL, Requirement: HookRequired}, status: HookHealthy},
		{name: "malformed-url", hook: Hook{Name: "bad-url", URL: "localhost:bad", Requirement: HookRequired}, status: HookMalformed},
		{name: "malformed-response", hook: Hook{Name: "bad-response", URL: malformed.URL, Requirement: HookRequired}, status: HookMalformed},
		{name: "unavailable", hook: Hook{Name: "unavailable", URL: "http://127.0.0.1:1", Requirement: HookRequired, Timeout: 100 * time.Millisecond}, status: HookUnavailable},
		{name: "timeout", hook: Hook{Name: "timeout", URL: slow.URL, Requirement: HookRequired, Timeout: time.Millisecond}, status: HookTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := CheckHooks(context.Background(), []Hook{tc.hook}, HookIdentity{}, http.DefaultClient)
			if len(report.Results) != 1 || report.Results[0].Status != tc.status {
				t.Fatalf("fixture report = %+v, want %s", report, tc.status)
			}
			if report.RequiredHealthy == (tc.status != HookHealthy) {
				t.Fatalf("required health = %v for %s", report.RequiredHealthy, tc.status)
			}
		})
	}
}

func TestCheckHooksOptionalFailuresEmitOneWarning(t *testing.T) {
	report := CheckHooks(context.Background(), []Hook{
		{Name: "b-optional", URL: "http://127.0.0.1:1", Requirement: HookOptional},
		{Name: "a-optional", URL: "not-a-url", Requirement: HookOptional},
	}, HookIdentity{}, nil)
	if !report.RequiredHealthy {
		t.Fatal("optional failures must not fail required health")
	}
	want := "optional harness hooks degraded: a-optional=malformed,b-optional=unavailable"
	if report.DegradedWarning != want {
		t.Fatalf("warning = %q, want %q", report.DegradedWarning, want)
	}
}

func TestCheckHooksRejectsAuthorityAndRedirectEscapes(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://example.com/secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	cases := []struct {
		name string
		hook Hook
		code HookCode
	}{
		{name: "query", hook: Hook{Name: "query", URL: redirect.URL + "?token=secret", Requirement: HookRequired}, code: HookCodeMalformed},
		{name: "userinfo", hook: Hook{Name: "userinfo", URL: "http://user:secret@127.0.0.1:1", Requirement: HookRequired}, code: HookCodeMalformed},
		{name: "non-local", hook: Hook{Name: "non-local", URL: "http://example.com/health", Requirement: HookRequired}, code: HookCodeAuthority},
		{name: "redirect-ssrf", hook: Hook{Name: "redirect", URL: redirect.URL, Requirement: HookRequired}, code: HookCodeRedirect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := CheckHooks(context.Background(), []Hook{tc.hook}, HookIdentity{}, redirect.Client())
			if len(report.Results) != 1 || report.Results[0].Code != tc.code {
				t.Fatalf("result = %+v, want code %s", report.Results, tc.code)
			}
			if report.Results[0].RedactedAuthority == "" || strings.Contains(report.Results[0].RedactedAuthority, "secret") {
				t.Fatalf("authority was not safely redacted: %q", report.Results[0].RedactedAuthority)
			}
		})
	}
}

func TestCheckHooksRejectsOversizedTimeoutAndDuplicateIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	report := CheckHooks(context.Background(), []Hook{
		{Name: "same", URL: server.URL, Requirement: HookRequired},
		{Name: "same", URL: server.URL, Requirement: HookRequired},
		{Name: "too-long", URL: server.URL, Requirement: HookRequired, Timeout: 3 * time.Second},
	}, HookIdentity{}, server.Client())
	if len(report.Results) != 3 || report.Results[1].Code != HookCodeDuplicate || report.Results[2].Code != HookCodeTimeoutLimit {
		t.Fatalf("bounded policy report = %+v", report.Results)
	}
}

func TestFileDiscoveryStatesAreDistinct(t *testing.T) {
	path := t.TempDir() + "/hooks.json"
	discovery := FileDiscovery{Path: path}
	if _, err := discovery.Discover("codex"); err == nil {
		t.Fatal("missing discovery file must fail closed")
	}
	if err := os.WriteFile(path, []byte(`{"providers":{"codex":{"hooks":[]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := discovery.Discover("codex")
	if err != nil || result.State != DiscoveryNoHooks {
		t.Fatalf("explicit no-hooks state = %+v, err=%v", result, err)
	}
	result, err = discovery.Discover("claude")
	if err != nil || result.State != DiscoveryNotDiscovered {
		t.Fatalf("missing provider state = %+v, err=%v", result, err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = discovery.Discover("codex")
	if err == nil || result.State != DiscoveryFailed {
		t.Fatalf("malformed discovery state = %+v, err=%v", result, err)
	}
}
