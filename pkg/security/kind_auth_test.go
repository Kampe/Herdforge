package security

import (
	"os"
	"strings"
	"testing"
)

func TestDiagnoseKindAuthReadiness_NoAPIKeys_External(t *testing.T) {
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	for _, kind := range []string{"codex", "claude", "grok"} {
		d := DiagnoseKindAuthReadiness(kind)
		if d.Brokerable {
			t.Fatalf("%s: must not be brokerable without HostCreds", kind)
		}
		if d.Class != KindAuthExternal && d.Class != KindAuthConfig {
			t.Fatalf("%s class=%s", kind, d.Class)
		}
		if d.Blocker == "" || !strings.Contains(d.Blocker, "FAC-133 BLOCKED") {
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

func TestDiagnoseKindAuthReadiness_WithAPIKey_OKOrCodexOAuth(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real-xxxxxxxx")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	d := DiagnoseKindAuthReadiness("codex")
	// Host may still be chatgpt OAuth — either external (oauth) or ok (api key present).
	if d.Blocker != "" && d.Class == KindAuthOK {
		t.Fatal("inconsistent")
	}
	if d.Class == KindAuthOK && !d.Brokerable {
		t.Fatal("ok must be brokerable")
	}
	// Must mention hosts, not key material.
	if strings.Contains(FormatKindAuthBlocker(d), "sk-test") {
		t.Fatal("must not echo API key")
	}
}

func TestRequiredBrokerHostsForKind(t *testing.T) {
	if got := RequiredBrokerHostsForKind("claude"); got[0] != "api.anthropic.com" {
		t.Fatal(got)
	}
	if RequiredBrokerHostsForKind("nope") != nil {
		t.Fatal("unknown")
	}
}
