package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
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
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 3 {
		t.Fatalf("merged Claude layers = %+v, err=%v", result, err)
	}
	if result.Hooks[0].Name == "" || result.Hooks[1].Name == "" || result.Hooks[2].Name == "" || result.Hooks[0].URL != "http://127.0.0.1:8790/global" {
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
	home := t.TempDir()
	bin := t.TempDir()
	moshi := filepath.Join(home, ".local", "bin", "moshi-hook")
	if err := os.MkdirAll(filepath.Dir(moshi), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(moshi, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/settings.json"
	config := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/.local/bin/moshi-hook --port 8790","timeout":600}]}],"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"/bin/sh -c 'moshi-hook --port 8790'"}]}],"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"python3 $HOME/.local/bin/moshi-hook --port 8790"}]}],"PostToolUseFailure":[{"matcher":"Edit","hooks":[{"type":"command","command":"bash $HOME/.local/bin/moshi-hook --port 8790"}]}],"SubagentStart":[{"matcher":"","hooks":[{"type":"command","command":"/bin/sh -c 'moshi-hook --port 8790 subagent'"}]}]}}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (ClaudeDiscovery{Paths: []string{path}}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 5 {
		t.Fatalf("command incident discovery = %+v, err=%v", result, err)
	}
	for _, hook := range result.Hooks {
		if hook.Name == "" || !strings.HasPrefix(hook.Name, "claude:") || strings.Contains(hook.Name, "moshi-hook") {
			t.Fatalf("command binding = %+v", hook)
		}
	}
	policies := []HookPolicy{{HandlerDigest: result.Hooks[0].Name, Requirement: HookRequired, HealthURL: "http://" + refused + "/health"}}
	bound, code, _ := ApplyHookPolicies(result.Hooks[:1], policies, policyRevision(policies))
	if code != HookCodeHealthy {
		t.Fatalf("trusted health policy binding = %s", code)
	}
	report := CheckHooks(context.Background(), bound, HookIdentity{PolicyRevision: policyRevision(policies)}, nil)
	if report.RequiredHealthy || len(report.Results) != 1 || report.Results[0].Code != HookCodeUnavailable || report.Results[0].EndpointClass != EndpointLoopback || report.Results[0].Name != result.Hooks[0].Name {
		t.Fatalf("installed command health refusal = %+v", report)
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

func TestHookPoliciesBindExactRevisionAndHealthAuthority(t *testing.T) {
	hook := Hook{Name: "claude:pre-tool:canonical", Requirement: HookRequired}
	valid := HookPolicy{HandlerDigest: hook.Name, Requirement: HookRequired, HealthURL: "http://127.0.0.1:8790/health"}
	revision := policyRevision([]HookPolicy{valid})
	if bound, code, digest := ApplyHookPolicies([]Hook{hook}, []HookPolicy{valid}, revision); code != HookCodeHealthy || digest != "" || bound[0].HealthURL != valid.HealthURL {
		t.Fatalf("valid policy binding = %+v, code=%s", bound, code)
	}
	telemetry := HookPolicy{HandlerDigest: hook.Name, Requirement: HookOptional, HealthURL: valid.HealthURL}
	if bound, code, _ := ApplyHookPolicies([]Hook{hook}, []HookPolicy{telemetry}, policyRevision([]HookPolicy{telemetry})); code != HookCodeHealthy || bound[0].Requirement != HookOptional {
		t.Fatalf("exact optional policy did not classify handler = %+v, code=%s", bound, code)
	}
	cases := []struct {
		name     string
		policy   HookPolicy
		revision string
		code     HookCode
	}{
		{"missing", HookPolicy{}, "", HookCodePolicySetMissing},
		{"stale", valid, "sha256:stale", HookCodePolicyStale},
		{"mismatch", HookPolicy{HandlerDigest: hook.Name, Requirement: HookRequirement("unknown"), HealthURL: valid.HealthURL}, "", HookCodePolicyMismatch},
		{"external", HookPolicy{HandlerDigest: hook.Name, Requirement: HookRequired, HealthURL: "https://example.com/health"}, "", HookCodeAuthority},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policies := []HookPolicy{tc.policy}
			if tc.name == "missing" {
				policies = nil
			}
			if tc.name == "mismatch" || tc.name == "external" {
				tc.revision = policyRevision(policies)
			}
			if _, code, digest := ApplyHookPolicies([]Hook{hook}, policies, tc.revision); code != tc.code || (tc.name != "missing" && tc.name != "stale" && digest != hook.Name) {
				t.Fatalf("policy code=%s, want %s", code, tc.code)
			}
		})
	}
	if _, code, digest := ApplyHookPolicies([]Hook{hook}, []HookPolicy{valid, valid}, policyRevision([]HookPolicy{valid, valid})); code != HookCodePolicyDuplicate || digest != valid.HandlerDigest {
		t.Fatalf("duplicate policy code=%s", code)
	}
}

func TestPlainCommandAndPassiveHandlersAreStructuralOnly(t *testing.T) {
	command := Hook{Name: "command", Requirement: HookRequired, kind: hookCommand, executable: "/bin/sh", Timeout: 600 * time.Second}
	passive := Hook{Name: "passive", Requirement: HookOptional, kind: hookPassive}
	report := CheckHooks(context.Background(), []Hook{command, passive}, HookIdentity{}, nil)
	if report.RequiredHealthy || len(report.Results) != 2 || report.Results[0].Status != HookStructural || report.Results[0].Code != HookCodeNoHealth || report.Results[1].Status != HookStructural {
		t.Fatalf("structural-only handlers = %+v", report)
	}
}

func TestPolicyHealthProbeBindsHandlerAndRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","hook_digest":%q,"policy_revision":%q}`, r.Header.Get("X-Herd-Hook-Digest"), r.Header.Get("X-Herd-Policy-Revision"))
	}))
	defer server.Close()
	hook := Hook{Name: "claude:pre-tool:" + strings.Repeat("a", 64), URL: server.URL, HealthURL: server.URL, Requirement: HookRequired}
	identity := HookIdentity{Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", PolicyRevision: "sha256:" + strings.Repeat("b", 64)}
	report := CheckHooks(context.Background(), []Hook{hook}, identity, server.Client())
	if !report.RequiredHealthy || report.Results[0].Status != HookHealthy || report.Results[0].Name != hook.Name {
		t.Fatalf("bound policy health = %+v", report)
	}
	passive := Hook{Name: "claude:permission:" + strings.Repeat("c", 64), HealthURL: server.URL, Requirement: HookRequired, kind: hookPassive}
	report = CheckHooks(context.Background(), []Hook{passive}, identity, server.Client())
	if !report.RequiredHealthy || report.Results[0].Status != HookHealthy {
		t.Fatalf("passive bound policy health = %+v", report)
	}
}

func TestClaudeHTTPIdentityIncludesPathAndBehavior(t *testing.T) {
	a := `{"hooks":{"PostToolUse":[{"matcher":"","hooks":[{"type":"http","name":"same","url":"http://127.0.0.1:8790/a","headers":{"X-Test":"one"},"once":true}]}]}}`
	b := `{"hooks":{"PostToolUse":[{"matcher":"","hooks":[{"type":"http","name":"same","url":"http://127.0.0.1:8790/b","headers":{"X-Test":"two"},"once":false}]}]}}`
	pa, pb := t.TempDir()+"/a.json", t.TempDir()+"/b.json"
	if err := os.WriteFile(pa, []byte(a), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pb, []byte(b), 0600); err != nil {
		t.Fatal(err)
	}
	ra, err := (ClaudeDiscovery{Paths: []string{pa}}).Discover("claude")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := (ClaudeDiscovery{Paths: []string{pb}}).Discover("claude")
	if err != nil {
		t.Fatal(err)
	}
	if ra.Hooks[0].Name == rb.Hooks[0].Name {
		t.Fatalf("HTTP path/behavior did not affect identity: %q", ra.Hooks[0].Name)
	}
}

func TestDefaultDiscoveryAugmentsClaudeInsteadOfReplacingIt(t *testing.T) {
	settingsPath := t.TempDir() + "/settings.json"
	settings := `{"hooks":{"PostToolUse":[{"matcher":"","hooks":[{"type":"http","name":"canonical","url":"http://127.0.0.1:8790/live"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	overridePath := t.TempDir() + "/policy.json"
	t.Setenv("HERD_CLAUDE_SETTINGS_FILE", settingsPath)
	discovered, err := (ClaudeDiscovery{}).Discover("claude")
	if err != nil || len(discovered.Hooks) != 1 {
		t.Fatalf("discovery fixture = %+v, err=%v", discovered, err)
	}
	override := `{"providers":{"claude":{"policies":[{"handler_digest":` + fmt.Sprintf("%q", discovered.Hooks[0].Name) + `,"requirement":"required","health_url":"http://127.0.0.1:8790/health"}]}}}`
	if err := os.WriteFile(overridePath, []byte(override), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (DefaultDiscovery{OverridePath: overridePath}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Hooks) != 1 || result.Hooks[0].URL != "http://127.0.0.1:8790/live" || len(result.Policies) != 1 || !result.PolicyRequired {
		t.Fatalf("override replaced canonical discovery = %+v, err=%v", result, err)
	}
}

func TestDefaultDiscoveryFindsRepositoryPolicyFromNonRootCWD(t *testing.T) {
	repo := t.TempDir()
	if err := exec.Command("git", "-C", repo, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(repo, "settings.json")
	settings := `{"hooks":{"PostToolUse":[{"matcher":"","hooks":[{"type":"http","name":"canonical","url":"http://127.0.0.1:8790/live"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CLAUDE_SETTINGS_FILE", settingsPath)
	t.Setenv("HERD_HARNESS_HOOKS_FILE", "")
	t.Setenv(gitroot.EnvProjectRoot, "")
	discovered, err := (ClaudeDiscovery{}).Discover("claude")
	if err != nil || len(discovered.Hooks) != 1 {
		t.Fatalf("discovery fixture = %+v, err=%v", discovered, err)
	}
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(canonicalRepo, ".herd", "harness-hooks.json")
	if err := os.MkdirAll(filepath.Dir(policyPath), 0700); err != nil {
		t.Fatal(err)
	}
	policy := `{"providers":{"claude":{"policies":[{"handler_digest":` + fmt.Sprintf("%q", discovered.Hooks[0].Name) + `,"requirement":"required","health_url":"http://127.0.0.1:8790/health"}]}}}`
	if err := os.WriteFile(policyPath, []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(repo, "lane", "nested")
	if err := os.MkdirAll(elsewhere, 0700); err != nil {
		t.Fatal(err)
	}
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })

	result, err := (DefaultDiscovery{}).Discover("claude")
	if err != nil || result.State != DiscoveryHooks || len(result.Policies) != 1 || result.PolicyPath != policyPath {
		t.Fatalf("non-root shipped discovery = %+v, err=%v", result, err)
	}
}

func TestClaudeScopesConcatenateAndDocumentedDeduplication(t *testing.T) {
	user := t.TempDir() + "/user.json"
	project := t.TempDir() + "/project.json"
	userConfig := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"moshi-hook --port 8790","timeout":30}]}],"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"moshi-hook --port 8790","timeout":30}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"moshi-hook --port 8790 --mode user","timeout":30}]}]}}`
	projectConfig := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"moshi-hook --port 8790","timeout":30}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"moshi-hook --port 8790 --mode project","timeout":30}]}]}}`
	if err := os.WriteFile(user, []byte(userConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(projectConfig), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (ClaudeDiscovery{Paths: []string{user, project}}).Discover("claude")
	if err != nil || len(result.Hooks) != 4 {
		t.Fatalf("scope merge/dedupe = %+v, err=%v", result, err)
	}
	if result.Hooks[0].Name == result.Hooks[1].Name || result.Hooks[1].Name == result.Hooks[2].Name {
		t.Fatalf("event/matcher/args attribution collapsed: %+v", result.Hooks)
	}
}

func TestClaudeScopesRejectConflictingSameRuntimeBehavior(t *testing.T) {
	user := t.TempDir() + "/user.json"
	project := t.TempDir() + "/project.json"
	fixture := func(status string) string {
		return `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"http","url":"http://127.0.0.1:8790/health","statusMessage":"` + status + `"}]}]}}`
	}
	if err := os.WriteFile(user, []byte(fixture("user")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(fixture("project")), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (ClaudeDiscovery{Paths: []string{user, project}}).Discover("claude")
	if err == nil || result.State != DiscoveryFailed {
		t.Fatalf("conflicting same-runtime behavior was accepted: %+v, err=%v", result, err)
	}
}

func TestHookPolicyInventoryRoundTripsStaticPolicy(t *testing.T) {
	settingsPath := t.TempDir() + "/settings.json"
	settings := `{"hooks":{"SessionStart":[{"hooks":[{"type":"http","url":"http://127.0.0.1:8790/health"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	discovery := ClaudeDiscovery{Paths: []string{settingsPath}}
	inventory, err := DiscoverHookPolicyInventory(discovery, "claude")
	if err != nil || len(inventory.Handlers) != 1 || inventory.Handlers[0].HandlerDigest == "" {
		t.Fatalf("inventory = %+v, err=%v", inventory, err)
	}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	var decoded HookPolicyInventory
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Provider != inventory.Provider || decoded.PolicyRevision != inventory.PolicyRevision || len(decoded.Handlers) != len(inventory.Handlers) {
		t.Fatalf("inventory JSON round trip = %+v, err=%v", decoded, err)
	}
	operatorJSON, err := DiscoverHookPolicyInventoryJSON(discovery, "claude")
	if err != nil || strings.Contains(string(operatorJSON), "127.0.0.1") || strings.Contains(string(operatorJSON), "http") || strings.Contains(string(operatorJSON), "command") {
		t.Fatalf("operator inventory leaked endpoint data: %s", operatorJSON)
	}
	policies := []HookPolicy{{HandlerDigest: inventory.Handlers[0].HandlerDigest, Requirement: inventory.Handlers[0].Requirement, HealthURL: "http://127.0.0.1:8790/health"}}
	revision := HookPolicyRevision(policies)
	if revision == "" || revision == inventory.PolicyRevision {
		// The authored policy revision must be independently actionable from the
		// empty pre-authoring policy set.
		t.Fatalf("policy revision is not independent: inventory=%q authored=%q", inventory.PolicyRevision, revision)
	}
	bound, code, digest := ApplyHookPolicies(inventoryToHooks(inventory), policies, revision)
	if code != HookCodeHealthy || digest != "" || len(bound) != 1 || bound[0].HealthURL != policies[0].HealthURL {
		t.Fatalf("inventory policy round trip = hooks=%+v code=%s digest=%q", bound, code, digest)
	}
}

func inventoryToHooks(inventory HookPolicyInventory) []Hook {
	hooks := make([]Hook, 0, len(inventory.Handlers))
	for _, entry := range inventory.Handlers {
		hooks = append(hooks, Hook{Name: entry.HandlerDigest, Requirement: entry.Requirement, kind: hookHTTP})
	}
	return hooks
}
