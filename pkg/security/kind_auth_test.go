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
	for _, kind := range []string{"codex", "claude", "grok"} {
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
	if got := RequiredBrokerHostsForKind("claude"); len(got) == 0 || got[0] != "api.anthropic.com" {
		t.Fatal(got)
	}
	if RequiredBrokerHostsForKind("nope") != nil {
		t.Fatal("unknown")
	}
}
