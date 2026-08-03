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

func TestSecurity_NoAPIKeysInWorkerEnv(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer vault-only-secret")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "sec1", Authority: v,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for k, val := range sess.WorkerEnvMap() {
		if strings.Contains(val, "vault-only-secret") {
			t.Fatal(k)
		}
		if strings.HasSuffix(k, "API_KEY") && val != "" {
			t.Fatalf("%s=%q", k, val)
		}
	}
}

func TestSecurity_DNSRebindMustNotOK(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "sec2", Authority: v, EnableOracle: true,
	})
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
		t.Fatal("rebind must not OK")
	}
}

func TestSecurity_TLSInjectNoDummy(t *testing.T) {
	v := NewTestCredentialVault()
	secret := "Bearer real-upstream-only"
	_ = v.InstallTestSecret("127.0.0.1", secret)
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "fake", SessionID: "sec3", Authority: v, AllowLoopback: true, EnableOracle: true,
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
	sess.Oracle.Hosts = appendUnique(sess.Oracle.Hosts, "127.0.0.1")
	sess.Oracle.Rules = RequestRulesForKind("fake")
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "127.0.0.1", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if saw != secret {
		t.Fatal(RedactSecrets(saw))
	}
}

func TestSecurity_UpstreamJSONErrorNotOK(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("127.0.0.1", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "fake", SessionID: "sec4", Authority: v, AllowLoopback: true, EnableOracle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"error":{"message":"nope","type":"invalid_request_error"}}`)
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
	sess.Oracle.Hosts = appendUnique(sess.Oracle.Hosts, "127.0.0.1")
	sess.Oracle.Rules = RequestRulesForKind("fake")
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "127.0.0.1", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("JSON error body must not be OK")
	}
}

func TestSecurity_ParentEnvKeysNotInWorker(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-parent-should-not-leak")
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer vault")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "sec5", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// WorkerEnv itself
	if strings.Contains(strings.Join(sess.WorkerEnv(), "\n"), "sk-parent") {
		t.Fatal()
	}
	// After scrub merge
	merged := scrubAndMergeEnv(os.Environ(), sess.WorkerEnv())
	for _, e := range merged {
		if strings.Contains(e, "sk-parent") {
			t.Fatal(e)
		}
		if strings.HasPrefix(e, "XAI_API_KEY=") && e != "XAI_API_KEY=" {
			// worker sets empty; if both present last wins depending on order — ensure no secret
			if strings.Contains(e, "sk-") {
				t.Fatal(e)
			}
		}
	}
}

func TestSecurity_NoCONNECTOpenProxy(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "sec6", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := ProveMITMExactHost(sess.Mitm, "evil.example.com"); err != nil {
		t.Fatal(err)
	}
}
