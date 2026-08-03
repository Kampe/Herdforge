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
		{Name: "z-optional", URL: server.URL + "/z", HealthURL: server.URL, Requirement: HookOptional},
		{Name: "a-required", URL: server.URL + "/a", HealthURL: server.URL, Requirement: HookRequired},
	}, HookIdentity{Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium"}, server.Client())
	if !report.RequiredHealthy || report.DegradedWarning != "" {
		t.Fatalf("healthy hooks report = %+v", report)
	}
	if len(report.Results) != 2 || report.Results[0].Name != stableHookName("a-required") || report.Results[1].Name != stableHookName("z-optional") {
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
		{name: "healthy", hook: Hook{Name: "healthy", URL: valid.URL, HealthURL: valid.URL, Requirement: HookRequired}, status: HookHealthy},
		{name: "malformed-url", hook: Hook{Name: "bad-url", URL: "localhost:bad", Requirement: HookRequired}, status: HookMalformed},
		{name: "malformed-response", hook: Hook{Name: "bad-response", URL: malformed.URL, HealthURL: malformed.URL, Requirement: HookRequired}, status: HookMalformed},
		{name: "unavailable", hook: Hook{Name: "unavailable", URL: "http://127.0.0.1:1", Requirement: HookRequired, Timeout: 100 * time.Millisecond}, status: HookUnavailable},
		{name: "timeout", hook: Hook{Name: "timeout", URL: slow.URL, HealthURL: slow.URL, Requirement: HookRequired, Timeout: time.Millisecond}, status: HookTimeout},
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
		{Name: "a-optional", URL: "http://127.0.0.1:2", Requirement: HookOptional},
	}, HookIdentity{}, nil)
	if !report.RequiredHealthy {
		t.Fatal("optional failures must not fail required health")
	}
	want := "optional harness hooks degraded: " + stableHookName("a-optional") + "=unavailable," + stableHookName("b-optional") + "=unavailable"
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
		{name: "redirect-ssrf", hook: Hook{Name: "redirect", URL: redirect.URL, HealthURL: redirect.URL, Requirement: HookRequired}, code: HookCodeRedirect},
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
	if len(report.Results) != 3 || report.Results[1].Code != HookCodeDuplicate || report.Results[2].Status != HookHealthy {
		t.Fatalf("bounded policy report = %+v", report.Results)
	}
}

func TestOptionalPolicyFailureFailsClosed(t *testing.T) {
	report := CheckHooks(context.Background(), []Hook{{Name: "bad-optional", URL: "http://example.com/health", Requirement: HookOptional}}, HookIdentity{}, nil)
	if report.RequiredHealthy || report.DegradedWarning != "" || len(report.Results) != 1 || report.Results[0].Code != HookCodeAuthority {
		t.Fatalf("optional policy failure was degraded: %+v", report)
	}
}

func TestOptionalLoopbackRedirectFailsClosed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	report := CheckHooks(context.Background(), []Hook{{Name: "optional-redirect", URL: redirect.URL, HealthURL: redirect.URL, Requirement: HookOptional}}, HookIdentity{}, redirect.Client())
	if report.RequiredHealthy || report.DegradedWarning != "" || report.Results[0].Code != HookCodeRedirect {
		t.Fatalf("local redirect was not fatal: %+v", report)
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

func TestClaudeDiscoveryMixedAndCommandOnlySettings(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	mixed := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo secret-body","timeout":30,"async":true},{"type":"http","url":"http://127.0.0.1:8790/health","headers":{"Authorization":"Bearer secret"},"allowedEnvVars":["TOKEN"],"if":"$CLAUDE_PROJECT_DIR","statusMessage":"sending","once":true,"timeout":1}]}]}}`
	if err := os.WriteFile(path, []byte(mixed), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (ClaudeDiscovery{Paths: []string{path}}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 2 {
		t.Fatalf("mixed Claude settings = %+v, err=%v", result, err)
	}
	if result.Hooks[0].Name == "" || result.Hooks[1].Name == "" || strings.Contains(result.Hooks[0].Name, "secret-body") || strings.Contains(result.Hooks[1].Name, "secret-body") {
		t.Fatalf("HTTP hook metadata = %+v", result.Hooks[0])
	}
	commandOnly := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo secret-body","timeout":30,"async":true},{"type":"mcp_tool","tool":"secret-tool"},{"type":"prompt","prompt":"secret-prompt"},{"type":"agent","prompt":"secret-agent"}]}]}}`
	if err := os.WriteFile(path, []byte(commandOnly), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = (ClaudeDiscovery{Paths: []string{path}}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 4 {
		t.Fatalf("command-only Claude settings must bind every configured hook = %+v, err=%v", result, err)
	}
	if strings.Contains(result.Hooks[0].Name, "secret-body") {
		t.Fatal("command body leaked into hook identity")
	}
}

func TestClaudeDiscoveryMergesGlobalAndLocalLayersDeterministically(t *testing.T) {
	dir := t.TempDir()
	global := dir + "/global.json"
	local := dir + "/local.json"
	globalConfig := `{"hooks":{"SessionStart":[{"hooks":[{"type":"http","name":"global-only","url":"http://127.0.0.1:8790/global","timeout":1}]}],"Stop":[{"hooks":[{"type":"http","name":"override","url":"http://127.0.0.1:8790/global-override","timeout":"1s"}]}]}}`
	localConfig := `{"hooks":{"Stop":[{"hooks":[{"type":"http","name":"override","url":"http://127.0.0.1:8790/local-override","timeout":1}]}]}}`
	if err := os.WriteFile(global, []byte(globalConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(localConfig), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (ClaudeDiscovery{Paths: []string{global, local}}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 2 {
		t.Fatalf("merged Claude layers = %+v, err=%v", result, err)
	}
	if result.Hooks[0].Name != claudeHookIdentity("SessionStart", "", "global-only", "http://127.0.0.1:8790/global", 0) || result.Hooks[1].Name != claudeHookIdentity("Stop", "", "override", "http://127.0.0.1:8790/local-override", 0) || result.Hooks[1].URL != "http://127.0.0.1:8790/local-override" {
		t.Fatalf("merged ordering/override = %+v", result.Hooks)
	}
	if err := os.WriteFile(local, []byte(`{"hooks":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = (ClaudeDiscovery{Paths: []string{global, local}}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 2 {
		t.Fatalf("empty local layer masked global hooks: %+v, err=%v", result, err)
	}
}

func TestDefaultDiscoveryUsesBuiltInNoHooksWhenOverrideAbsent(t *testing.T) {
	discovery := DefaultDiscovery{}
	result, err := discovery.Discover("codex")
	if err != nil || result.State != DiscoveryNoHooks {
		t.Fatalf("built-in codex policy = %+v, err=%v", result, err)
	}
}

func TestClaudeCommandIncidentBindsDigestAndChecksExecutable(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	config := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/definitely-missing-herd-hook 127.0.0.1:8790","timeout":600}]}]}}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (ClaudeDiscovery{Paths: []string{path}}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 1 {
		t.Fatalf("command incident discovery = %+v, err=%v", result, err)
	}
	hook := result.Hooks[0]
	if hook.Requirement != HookRequired || strings.Contains(hook.Name, "definitely-missing") || hook.Timeout != 600*time.Second {
		t.Fatalf("command binding = %+v", hook)
	}
	report := CheckHooks(context.Background(), result.Hooks, HookIdentity{}, nil)
	if report.RequiredHealthy || report.Results[0].Code != HookCodeUnavailable || report.Results[0].EndpointClass != EndpointCommand {
		t.Fatalf("command incident preflight = %+v", report)
	}
}

func TestClaudeUnknownEventRequirementFailsClosed(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	config := `{"hooks":{"FutureEvent":[{"hooks":[{"type":"http","url":"http://127.0.0.1:8790"}]}]}}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (ClaudeDiscovery{Paths: []string{path}}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 1 || result.Hooks[0].Requirement != HookRequirement("unknown") {
		t.Fatalf("unknown event policy = %+v, err=%v", result, err)
	}
	report := CheckHooks(context.Background(), result.Hooks, HookIdentity{}, nil)
	if report.RequiredHealthy || report.Results[0].Code != HookCodeUnknownRequirement {
		t.Fatalf("unknown event did not fail closed = %+v", report)
	}
}

func TestExplicitClaudeSettingsFileMissingFailsClosed(t *testing.T) {
	t.Setenv("HERD_CLAUDE_SETTINGS_FILE", t.TempDir()+"/missing.json")
	result, err := (ClaudeDiscovery{}).Discover("claude")
	if err == nil || result.State != DiscoveryFailed {
		t.Fatalf("missing explicit Claude settings = %+v, err=%v", result, err)
	}
}
