package security

import (
	"os"
	"strings"
	"testing"
)

// FAC-133 tests rebased onto FAC-170 HostCreds oracle: production brokerable
// requires handle-backed authority, not raw env API keys.

func TestDiagnoseKindAuthReadiness_NoAPIKeys_External(t *testing.T) {
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS", envHostCredsHandles} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	// FAC-576: claude is deliberately absent from this list now. It is used
	// through its HARNESS on this fleet and never through an API key, so it has
	// no brokered host credential to be missing — asserting it must be
	// unbrokerable without HostCreds encoded the category error that refused
	// every reviewer launch on a fully logged-in host. Its own behaviour is
	// covered by TestClaudeDiagnosisIsBrokerable.
	//
	// codex and grok stay: they genuinely authenticate by API key, and raw env
	// keys still must not count as production authority.
	for _, kind := range []string{"codex", "grok"} {
		d := DiagnoseKindAuthReadiness(kind)
		if d.Brokerable {
			t.Fatalf("%s: must not be brokerable without HostCreds", kind)
		}
		if d.Class != KindAuthExternal && d.Class != KindAuthConfig && d.Class != KindAuthPlatform {
			t.Fatalf("%s class=%s", kind, d.Class)
		}
		if d.Blocker == "" || !strings.Contains(d.Blocker, "FAC-170 BLOCKED") {
			t.Fatalf("blocker: %q", d.Blocker)
		}
		// Packet must not contain bearer-looking secrets.
		pkt := FormatKindAuthBlocker(d)
		if strings.Contains(strings.ToLower(pkt), "sk-") || strings.Contains(pkt, "Bearer ") {
			t.Fatalf("packet leaked secret material: %s", pkt)
		}
		t.Logf("%s: %s", kind, pkt)
	}
}

func TestDiagnoseKindAuthReadiness_RawAPIKeyNotProductionAuthority(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real-xxxxxxxx")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	_ = os.Unsetenv(envHostCredsHandles)
	d := DiagnoseKindAuthReadiness("codex")
	// Raw env keys alone must not make production brokerable (FAC-170).
	if d.Brokerable {
		t.Fatal("raw OPENAI_API_KEY must not make codex brokerable")
	}
	if strings.Contains(FormatKindAuthBlocker(d), "sk-test") {
		t.Fatal("must not echo API key")
	}
}

func TestRequiredBrokerHostsForKind_KindAuthSurface(t *testing.T) {
	// FAC-576: claude requires NO brokered host. This asserted
	// api.anthropic.com, which is the host the harness never contacts with a
	// user key on this fleet. Covered positively by
	// TestClaudeNeedsNoBrokeredHostCredential.
	if got := RequiredBrokerHostsForKind("claude"); len(got) != 0 {
		t.Fatalf("claude is harness-authenticated and needs no brokered host, got %v", got)
	}
	// A kind that does use an API key must still name its host.
	if got := RequiredBrokerHostsForKind("codex"); len(got) == 0 || got[0] != "api.openai.com" {
		t.Fatal(got)
	}
	if RequiredBrokerHostsForKind("nope") != nil {
		t.Fatal("unknown")
	}
}

func TestDiagnoseKindAuthReadiness_AGYNative(t *testing.T) {
	for _, kind := range []string{"agy", "antigravity"} {
		d := DiagnoseKindAuthReadiness(kind)
		if !d.Brokerable {
			t.Fatalf("%s must be brokerable via native auth classification", kind)
		}
		if d.AuthorityClass != "native" {
			t.Fatalf("%s authority class = %q, want native", kind, d.AuthorityClass)
		}
		if d.Class != KindAuthOK {
			t.Fatalf("%s class = %q, want ok", kind, d.Class)
		}
		if d.ReasonCode != "native_auth" {
			t.Fatalf("%s reason code = %q, want native_auth", kind, d.ReasonCode)
		}
		if d.Blocker != "" {
			t.Fatalf("%s blocker = %q, want empty", kind, d.Blocker)
		}
	}
}
