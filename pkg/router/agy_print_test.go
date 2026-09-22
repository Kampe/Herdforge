package router

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
)

func TestInsertAgyStructuredPrintFlagsBeforePrint(t *testing.T) {
	got := InsertAgyStructuredPrintFlags([]string{"--model", "gemini-3.1-pro-high", "--print", "Reply with exactly: HERD_PROVIDER_PROBE_OK"})
	printAt, formatAt, jsonAt, disableAt := -1, -1, -1, -1
	for i, a := range got {
		switch a {
		case "--print", "-p", "--prompt":
			if printAt < 0 {
				printAt = i
			}
		case OutputFormatFlag:
			formatAt = i
		case "json":
			jsonAt = i
		case agentpolicy.DisableSlashCommandsFlag:
			disableAt = i
		}
	}
	if formatAt < 0 || jsonAt != formatAt+1 {
		t.Fatalf("missing --output-format json in %v", got)
	}
	if disableAt < 0 {
		t.Fatalf("missing --disable-slash-commands in %v", got)
	}
	if printAt < 0 {
		t.Fatalf("missing --print in %v", got)
	}
	if formatAt > printAt || disableAt > printAt {
		t.Fatalf("JSON flags must precede --print: %v", got)
	}
}

func TestInsertAgyStructuredPrintFlagsWhenPrintAbsent(t *testing.T) {
	got := InsertAgyStructuredPrintFlags([]string{"--model", "gemini-3.1-pro-high"})
	foundFormat, foundJSON, foundDisable := false, false, false
	for i, a := range got {
		if a == OutputFormatFlag {
			foundFormat = true
			if i+1 < len(got) && got[i+1] == "json" {
				foundJSON = true
			}
		}
		if a == agentpolicy.DisableSlashCommandsFlag {
			foundDisable = true
		}
	}
	if !foundFormat || !foundJSON || !foundDisable {
		t.Fatalf("flags must still be appended when --print is absent: %v", got)
	}
}

func TestAgyStructuredProbeReasonFixtures(t *testing.T) {
	healthy := `{"conversation_id":"cb414436-f458-407c-8541-6ce272b9e066","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK","model":"gemini-3.1-pro-high"}`
	cases := []struct {
		name   string
		stdout string
		model  string
		token  string
		ok     bool
		reason string
	}{
		{name: "json-success", stdout: healthy, model: "gemini-3.1-pro-high", token: providerProbeSentinel, ok: true},
		{name: "raw-sentinel", stdout: "HERD_PROVIDER_PROBE_OK", model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "no structured agy probe result"},
		{name: "tool-preamble", stdout: "view_file antigravity_guide\nHERD_PROVIDER_PROBE_OK\n", model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "no structured agy probe result"},
		{name: "slash-command-no-session", stdout: `{"conversation_id":"","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK"}`, model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "agy probe missing conversation session"},
		{name: "response-extra", stdout: `{"conversation_id":"sess-1","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK\nextra"}`, model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "agy probe response is not the exact token"},
		{name: "wrong-model", stdout: `{"conversation_id":"sess-1","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK","model":"gemini-2.5-flash"}`, model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "agy probe model does not match requested model"},
		{name: "not-success", stdout: `{"conversation_id":"sess-1","status":"ERROR","response":"HERD_PROVIDER_PROBE_OK"}`, model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "agy probe status is not SUCCESS"},
		{name: "stream-json", stdout: `{"event":"result","result":{"conversation_id":"sess-1","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK"}}`, model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "no structured agy probe result"},
		{name: "trailing-garbage", stdout: healthy + "\nERROR boom\n", model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "no structured agy probe result"},
		{name: "second-result", stdout: healthy + "\n" + `{"conversation_id":"sess-2","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK","model":"gemini-3.1-pro-high"}`, model: "gemini-3.1-pro-high", token: providerProbeSentinel, reason: "no structured agy probe result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := AgyStructuredProbeReason(tc.stdout, tc.model, tc.token)
			if tc.ok {
				if reason != "" {
					t.Fatalf("want success, got %q", reason)
				}
				return
			}
			if reason != tc.reason {
				t.Fatalf("got %q want %q", reason, tc.reason)
			}
		})
	}
}

func TestAgyAdmissionClassifyRequiresStructuredSuccess(t *testing.T) {
	healthy := `{"conversation_id":"sess-1","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK"}`
	ok, reason := classifyProviderProbeResult("agy", "gemini-3.1-pro-high", healthy, healthy, nil, false)
	if !ok || reason != "" {
		t.Fatalf("structured success must admit: ok=%t reason=%q", ok, reason)
	}
	ok, reason = classifyProviderProbeResult("agy", "gemini-3.1-pro-high", providerProbeSentinel, providerProbeSentinel, nil, false)
	if ok || !strings.Contains(reason, "no structured agy probe result") {
		t.Fatalf("raw sentinel must not admit agy: ok=%t reason=%q", ok, reason)
	}
}

func TestAgyAdmissionTimeoutStaysUnknownEvenWithHealthyJSON(t *testing.T) {
	healthy := `{"conversation_id":"sess-1","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK"}`
	ok, reason := classifyProviderProbeResult("agy", "gemini-3.1-pro-high", healthy, healthy, nil, true)
	if ok {
		t.Fatal("timeout must not convert to healthy")
	}
	if reason != probeTimeoutMarker {
		t.Fatalf("timeout reason = %q, want %q", reason, probeTimeoutMarker)
	}
}
