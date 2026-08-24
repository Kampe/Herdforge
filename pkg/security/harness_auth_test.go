package security

import "testing"

// TestClaudeNeedsNoBrokeredHostCredential is the FAC-576 gate.
//
// Claude is used through its HARNESS on this fleet and never through an API key.
// RequiredBrokerHostsForKind returned api.anthropic.com anyway, so worker
// admission demanded a handle-backed credential for a host the harness never
// contacts with a user key. A fully logged-in claude reported
// brokerable=false / hosts_creds=[] / authority=none, and every reviewer launch
// was refused before it started. That is a category error, not a missing
// credential — which is why installing one would not have helped.
func TestClaudeNeedsNoBrokeredHostCredential(t *testing.T) {
	if hosts := RequiredBrokerHostsForKind(AuthorKindClaude); len(hosts) != 0 {
		t.Errorf("claude is harness-authenticated and needs no brokered host, got %v", hosts)
	}
	// RequiredBrokerHostsForKind answers which host a brokered key would be FOR,
	// which is a different question from whether the kind needs one. codex and
	// grok keep a real mapping here because the broker's host machinery needs it;
	// FAC-587 exempts them at admission via harnessAuthenticated instead. Blanking
	// this map reported every kind as unbrokerable_kind.
	for _, kind := range []string{AuthorKindCodex, AuthorKindGrok} {
		if hosts := RequiredBrokerHostsForKind(kind); len(hosts) == 0 {
			t.Errorf("%s must keep a host mapping for the broker machinery", kind)
		}
	}
}

// A harness-authenticated kind must classify as native, exactly as agy already
// did. agy was exempt by name; claude was not, though the same reasoning applies.
func TestHarnessAuthenticatedKindsClassifyNative(t *testing.T) {
	for _, kind := range []string{
		AuthorKindClaude, AuthorKindAGY, "antigravity",
		// FAC-587: codex and grok belong here too. They run as harnesses in
		// herdr panes with their own CLI logins, verified on this host:
		// ~/.codex/auth.json is auth_mode=chatgpt with an OAuth token and an
		// empty OPENAI_API_KEY, and `codex exec` executes with no key present.
		AuthorKindCodex, AuthorKindGrok,
	} {
		if !harnessAuthenticated(kind) {
			t.Errorf("%s authenticates through its own harness on this fleet", kind)
		}
	}
	// opencode is still not a harness-auth kind: it is a gateway proxy, not a
	// CLI holding its own provider session, and it remains out of scope.
	if harnessAuthenticated("opencode") {
		t.Error("opencode is a gateway proxy and must not be treated as harness-authenticated")
	}
}

// The diagnosis a preflight prints must say brokerable for a harness-auth kind,
// with a native authority class — not a HostCreds demand.
func TestClaudeDiagnosisIsBrokerable(t *testing.T) {
	d := DiagnoseKindAuthReadiness(AuthorKindClaude)
	// A host without the claude CLI cannot be asked, and an unanswerable probe
	// must not invent a blocker.
	if HarnessLoginState(AuthorKindClaude) == HarnessLoggedOut {
		t.Skip("claude reports logged out on this host; the blocker is correct")
	}
	if !d.Brokerable {
		t.Fatalf("a harness-authenticated claude must be brokerable: %+v", d)
	}
	if d.AuthorityClass != "native" {
		t.Errorf("authority = %q, want native", d.AuthorityClass)
	}
	if d.ReasonCode != "native_auth" {
		t.Errorf("reason = %q, want native_auth", d.ReasonCode)
	}
	if d.Blocker != "" {
		t.Errorf("a brokerable kind must carry no blocker, got %q", d.Blocker)
	}
}

// An unanswerable login probe must not be read as logged out: a CLI that is
// absent or too old to answer is not evidence of a missing session.
func TestUnknownLoginIsNotLoggedOut(t *testing.T) {
	if got := HarnessLoginState("codex"); got != HarnessLoginUnknown {
		t.Errorf("a kind with no session probe must be unknown, got %q", got)
	}
	if HarnessLoginUnknown == HarnessLoggedOut {
		t.Fatal("unknown and logged-out must be distinct")
	}
}
