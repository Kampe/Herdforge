package security

import (
	"strings"
	"testing"
)

// Mutation-style tests: removing the boundary/allowlist/redaction/oracle model must fail.

func TestMutation_NoProxyBearerDelegation(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer mut-secret")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Store: store, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		t.Fatalf("MUTATION DETECTED: worker received proxy/bearer channel: %v", err)
	}
	for k, v := range sess.WorkerEnvMap() {
		if strings.Contains(strings.ToLower(k), "proxy") && strings.TrimSpace(v) != "" {
			t.Fatalf("MUTATION DETECTED: non-empty proxy env %s=%q", k, v)
		}
		if strings.Contains(v, "Bearer mut-secret") || strings.Contains(v, "mut-secret") {
			t.Fatalf("MUTATION DETECTED: secret in env %s", k)
		}
	}
}

func TestMutation_AllowlistBoundaryIsLoadBearing(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer mut-secret")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Store: store, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.openai.com", Method: "POST", Path: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	// api.openai.com is on default host list for codex but grok session may still
	// have the rule if DefaultRequestRules includes it. Host is allowlisted in
	// default rules — but missing HostCreds for openai should still fail.
	// Use clearly non-allowlisted host:
	r, err = CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "attacker.example", Method: "POST", Path: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION DETECTED: non-allowlisted host accepted")
	}
	r, err = CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/admin/exfil",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION DETECTED: non-allowlisted path accepted")
	}
}

func TestMutation_RedactionIsLoadBearing(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-mutation-leaked-value-999")
	d := DiagnoseKindAuthReadiness("grok")
	pkt := FormatKindAuthBlocker(d)
	if strings.Contains(pkt, "sk-mutation") || strings.Contains(pkt, "leaked-value") {
		t.Fatalf("MUTATION DETECTED: diagnosis packet leaked secret: %s", pkt)
	}
	raw := "Bearer sk-mutation-leaked-value-999"
	if RedactSecrets(raw) == raw {
		t.Fatal("MUTATION DETECTED: RedactSecrets is a no-op")
	}
}

func TestMutation_DummyNeverUpstream(t *testing.T) {
	store := NewMemorySecretStore()
	// Cannot store dummy as real secret.
	if err := store.Set("api.x.ai", DummyNeverUpstreamAuth); err == nil {
		t.Fatal("MUTATION DETECTED: dummy accepted into store")
	}
	_ = store.Set("api.x.ai", "Bearer real-mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Store: store, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Worker injects only dummy — ok; worker injects real key — denied.
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID,
		Host:      "api.x.ai",
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Headers:   map[string]string{"Authorization": "Bearer sk-not-dummy-real-looking"},
		Body:      `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION DETECTED: worker real Authorization accepted")
	}
}

func TestMutation_MissingCredsNoSilentFallback(t *testing.T) {
	store := NewMemorySecretStore()
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "claude", Store: store, Interactive: false,
	})
	if err == nil {
		_ = sess.Close()
		t.Fatal("MUTATION DETECTED: session started without HostCreds")
	}
	be, ok := err.(*BlockedError)
	if !ok || be.Reason != BlockMissingCreds {
		t.Fatalf("want missing_host_creds, got %v", err)
	}
}

func TestMutation_SessionRevokeIsLoadBearing(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Store: store, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Revoke(); err != nil {
		t.Fatal(err)
	}
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions",
	})
	// Socket may be gone (dial error) or return not-ok.
	if err == nil && r.OK {
		t.Fatal("MUTATION DETECTED: revoked session still serves")
	}
}
