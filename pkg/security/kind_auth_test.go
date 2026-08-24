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
	// FAC-587: codex and grok join claude. This fleet runs every agent as a
	// harness inside a herdr pane, never against a provider API, so demanding a
	// handle-backed credential refused their launches for something that does not
	// exist here. Verified on this host: ~/.codex/auth.json is auth_mode=chatgpt
	// with an OAuth token and an empty OPENAI_API_KEY, and `codex exec` runs with
	// no key present.
	//
	// The secret-hygiene half of this test is the part that still matters, and it
	// is kept: a diagnosis must never carry key material, brokerable or not.
	for _, kind := range []string{"codex", "grok"} {
		d := DiagnoseKindAuthReadiness(kind)
		if !d.Brokerable {
			t.Errorf("%s runs as a harness on this fleet and must be brokerable", kind)
		}
		if d.AuthorityClass != "native" {
			t.Errorf("%s authority must be native (its own harness session), got %q",
				kind, d.AuthorityClass)
		}
		pkt := FormatKindAuthBlocker(d)
		if strings.Contains(strings.ToLower(pkt), "sk-") || strings.Contains(pkt, "Bearer ") {
			t.Fatalf("packet leaked secret material: %s", pkt)
		}
		t.Logf("%s: %s", kind, pkt)
	}

	// A kind that is NOT a harness must still be refused, or this stops being a
	// gate at all. opencode is a gateway proxy and deliberately out of scope.
	if d := DiagnoseKindAuthReadiness("opencode"); d.Brokerable {
		t.Error("opencode is not a harness kind and must not be brokerable")
	}
}

func TestDiagnoseKindAuthReadiness_RawAPIKeyNotProductionAuthority(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real-xxxxxxxx")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	_ = os.Unsetenv(envHostCredsHandles)
	d := DiagnoseKindAuthReadiness("codex")

	// FAC-170's property, restated for FAC-587. codex is brokerable because it is
	// harness-authenticated, so "not brokerable" can no longer express this. What
	// must hold is that the raw env key is NOT the thing granting authority: the
	// authority class stays native, and the key is never echoed.
	if d.AuthorityClass != "native" || d.ReasonCode != "native_auth" {
		t.Errorf("authority must be the harness session, not the env key: class=%q reason=%q",
			d.AuthorityClass, d.ReasonCode)
	}
	if !EnvRawAPIKeysPresent() {
		t.Error("the raw key must still be DETECTED; it is simply never authority")
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
