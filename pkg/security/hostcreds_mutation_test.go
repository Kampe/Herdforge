package security

import (
	"errors"
	"net"
	"os"
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

func TestMutation_LiveRequiresOSBoundary(t *testing.T) {
	// Production live path must fail closed without separate-UID broker.
	t.Setenv(EnvAllowSameUIDTest, "")
	_ = os.Unsetenv(EnvAllowSameUIDTest)
	t.Setenv(EnvBrokerUID, "")
	_ = os.Unsetenv(EnvBrokerUID)
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "grok", Prompt: "x"})
	if err == nil {
		t.Fatal("MUTATION: live started without OS boundary")
	}
	if !errors.Is(err, ErrHostCredsBlocked) {
		// may wrap
		if _, ok := err.(*BlockedError); !ok {
			t.Fatalf("want BlockedError, got %T %v", err, err)
		}
	}
}

func TestMutation_LiveRefusesFakeKind(t *testing.T) {
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "fake", Prompt: "x"})
	if err == nil {
		t.Fatal("MUTATION: fake kind accepted as live")
	}
}

func TestMutation_LiveRefusesOpenCode(t *testing.T) {
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "opencode", Prompt: "x"})
	if err == nil {
		t.Fatal("MUTATION: opencode accepted")
	}
}

func TestMutation_LiveRefusesSameUIDTestMode(t *testing.T) {
	t.Setenv(EnvAllowSameUIDTest, "1")
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "grok", Prompt: "x"})
	if err == nil {
		t.Fatal("MUTATION: same-UID test mode allowed for live")
	}
}

func TestMutation_TLSOnlyNoPlaintextInject(t *testing.T) {
	// Oracle has no forceHTTP field path — credentialed forward always TLS.
	// Guard: upstreamTLS/dial must still handshake TLS (tested by proof suite).
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Dial a plaintext HTTP server; TLS handshake must fail closed (no inject over HTTP).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	sess.Oracle.allowLoopback = true
	sess.Oracle.Hosts = append(sess.Oracle.Hosts, "127.0.0.1")
	sess.Oracle.Rules = RequestRulesForKind("fake")
	_ = v.InstallTestSecret("127.0.0.1", "Bearer x")
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "127.0.0.1", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION: plaintext HTTP accepted for credentialed inject")
	}
}
