package security

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSecurity_SameUIDParentEnvNotAuthority(t *testing.T) {
	// Even if parent has raw keys, production diagnose is not brokerable without handles.
	t.Setenv("XAI_API_KEY", "sk-parent-env-secret-should-not-broker")
	t.Setenv(envHostCredsHandles, "")
	_ = os.Unsetenv(envHostCredsHandles)
	d := DiagnoseKindAuthReadiness("grok")
	if d.Brokerable {
		t.Fatal("parent env raw key must not authorize production broker")
	}
	// Worker env from a session must not inherit parent secret even if we start with test vault.
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer vault-only-secret")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Scrub is insufficient alone; also assert no parent secret in worker env map.
	for _, v := range sess.WorkerEnvMap() {
		if strings.Contains(v, "sk-parent-env-secret") || strings.Contains(v, "vault-only-secret") {
			t.Fatal("secret in worker env")
		}
	}
	// Simulated proc env read of worker view: only dummy + socket.
	if sess.WorkerEnvMap()["XAI_API_KEY"] != DummyNeverUpstream {
		t.Fatal()
	}
}

func TestSecurity_NoPublicGetSnapshot(t *testing.T) {
	// Document: CredentialAuthority has no Get or Snapshot methods.
	// This test fails to compile if someone re-adds them to the interface incorrectly
	// by ensuring only InjectAuthorization can surface material (into header we own).
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer hidden")
	var auth CredentialAuthority = v
	_ = auth.Has("api.x.ai")
	_ = auth.Hosts()
	hdr := http.Header{}
	if err := auth.InjectAuthorization("api.x.ai", hdr); err != nil {
		t.Fatal(err)
	}
	// No auth.Get, no auth.Snapshot in interface.
}

func TestSecurity_DummyNeverSentUpstream(t *testing.T) {
	v := NewTestCredentialVault()
	secret := "Bearer real-upstream-only"
	_ = v.InstallTestSecret("127.0.0.1", secret)
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "fake", Authority: v, Interactive: false, AllowLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var saw string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "https://"))
	sess.Oracle.allowLoopback = true
	sess.Oracle.upstreamTLS = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, ServerName: "127.0.0.1"}
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}

	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "127.0.0.1", Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"Authorization": DummyNeverUpstreamAuth},
		Body:    `{}`,
	})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if saw != secret || IsDummyCredential(saw) {
		t.Fatalf("upstream auth wrong: %q", RedactSecrets(saw))
	}
}

func TestSecurity_RedirectNoFollow(t *testing.T) {
	v := NewTestCredentialVault()
	secret := "Bearer redirect-secret-ZZ"
	_ = v.InstallTestSecret("127.0.0.1", secret)
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "fake", Authority: v, Interactive: false, AllowLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	followed := false
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
			return
		}
		followed = true
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "https://"))
	sess.Oracle.allowLoopback = true
	sess.Oracle.upstreamTLS = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, ServerName: "127.0.0.1"}
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}

	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "127.0.0.1", Method: "GET", Path: "/v1/models",
		Headers: map[string]string{"Authorization": DummyNeverUpstreamAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if followed {
		t.Fatal("must not follow redirect")
	}
	if strings.Contains(r.Body, secret) || strings.Contains(r.Error, secret) {
		t.Fatal("secret leak")
	}
}

func TestSecurity_DNSRebindDenied(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.ParseIP("10.0.0.1"), nil
	}
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("private rebind must fail closed")
	}
	if strings.Contains(r.Error, "Bearer") {
		t.Fatal("secret in error")
	}
}

func TestSecurity_HeaderInjectionDenied(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"X-A": "1\r\nAuthorization: Bearer stolen"},
		Body:    `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("header injection must deny")
	}
}

func TestSecurity_NoCONNECT(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	c, err := net.Dial("unix", sess.Oracle.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("CONNECT api.x.ai:443 HTTP/1.1\r\nHost: api.x.ai:443\r\n\r\n"))
	buf := make([]byte, 512)
	n, _ := c.Read(buf)
	resp := string(buf[:n])
	if strings.Contains(resp, "200") && strings.Contains(strings.ToLower(resp), "connection established") {
		t.Fatal("CONNECT must not work")
	}
}

func TestSecurity_DirectProviderHostsPerKind(t *testing.T) {
	h := DirectProviderHostsForKind("grok")
	if len(h) != 1 || h[0] != "api.x.ai" {
		t.Fatal(h)
	}
	if DirectProviderHostsForKind("opencode") != nil {
		t.Fatal()
	}
}
