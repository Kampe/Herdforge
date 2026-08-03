package security

import (
	"strings"
	"testing"
)

func TestMutation_NoProxyBearer(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut-secret")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		t.Fatal(err)
	}
}

func TestMutation_NoGetSnapshotExport(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut-secret")
	// Public interface must not include Get/Snapshot — compile-time + runtime hosts check.
	var _ CredentialAuthority = v
	if err := AssertNoPublicSecretExport(v); err != nil {
		t.Fatal(err)
	}
}

func TestMutation_PerKindAllowlist(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut-secret")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// api.openai.com is not on grok rules
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.openai.com", Method: "POST", Path: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION: cross-kind host allowed")
	}
	r, err = CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/admin/exfil",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION: path allowlist broken")
	}
}

func TestMutation_DummyNeverUpstream(t *testing.T) {
	v := NewTestCredentialVault()
	if err := v.InstallTestSecret("api.x.ai", DummyNeverUpstreamAuth); err == nil {
		t.Fatal("MUTATION: dummy accepted into vault")
	}
	_ = v.InstallTestSecret("api.x.ai", "Bearer real-mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"Authorization": "Bearer sk-not-dummy"},
		Body:    `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION: worker real Authorization accepted")
	}
}

func TestMutation_MissingCredsNoFallback(t *testing.T) {
	v := NewTestCredentialVault()
	_, err := StartHostCredsSession(SessionConfig{Kind: "claude", Authority: v, Interactive: false})
	if err == nil {
		t.Fatal("MUTATION: silent fallback")
	}
}

func TestMutation_Redaction(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-mutation-leaked-value-999")
	d := DiagnoseKindAuthReadiness("grok")
	pkt := FormatKindAuthBlocker(d)
	if strings.Contains(pkt, "sk-mutation") {
		t.Fatal(pkt)
	}
}

func TestMutation_SessionRevoke(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Revoke()
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions",
	})
	if err == nil && r.OK {
		t.Fatal("MUTATION: revoked session serves")
	}
}
