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
	"time"
)

// Regression: 2c6c7c6 fail-open peer bypass and empty production peer PID.
func TestE2E_WorkerPeerBinding_NoBypass(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build")
	}
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer e2e-secret")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "e2e-peer", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Without AllowPID, worker CONNECT denied.
	if err := ProveMITMRequiresAllowPID(sess.Mitm, "api.x.ai"); err != nil {
		t.Fatal(err)
	}

	// With child AllowPID + production peer resolution: deny host 403, allow host 200 CONNECT.
	cap, err := NewCapability(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sess.RunWorkerForbiddenAndAllowProbe(cap.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if res.PID <= 0 {
		t.Fatal("worker pid missing")
	}
	if res.CapabilityMarker != cap.Expected {
		t.Fatalf("marker %q want %q", res.CapabilityMarker, cap.Expected)
	}
	if strings.Contains(CapabilityPrompt(cap), cap.Expected) {
		t.Fatal("expected must not appear in prompt")
	}
}

func TestE2E_CausalMarkerNotInPrompt(t *testing.T) {
	c, err := NewCapability("sess-marker")
	if err != nil {
		t.Fatal(err)
	}
	p := CapabilityPrompt(c)
	if strings.Contains(p, c.Expected) {
		t.Fatal("vacuous: expected in prompt")
	}
	if !VerifyCapabilityOutput(c, p, "noise "+c.Expected+" tail") {
		t.Fatal("verify failed")
	}
	if VerifyCapabilityOutput(c, p, p) {
		t.Fatal("prompt echo must not verify")
	}
}

func TestE2E_RotateRestartAuthority(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer rot-old")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "e2e-rot", Authority: v, EnableOracle: true, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Inject via oracle TLS loopback after rotate.
	secretNew := "Bearer rot-new-xyz"
	if err := v.RotateTestSecret("api.x.ai", secretNew); err != nil {
		t.Fatal(err)
	}
	// Restart MITM + keep authority.
	if err := sess.Restart(); err != nil {
		t.Fatal(err)
	}
	if sess.Mitm == nil || sess.Mitm.Addr() == "" {
		t.Fatal("mitm dead")
	}
	// Prove inject uses new secret via oracle component path.
	var saw string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "https://"))
	// Re-seed loopback for oracle rules of grok won't include 127.0.0.1 — use inject on api.x.ai
	// by forcing dial of local TLS for api.x.ai host SNI.
	sess.Oracle.upstreamTLS = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, ServerName: "api.x.ai"}
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.IPv4(1, 2, 3, 4), nil // non-private
	}
	// Bypass private check: use public IP that dialHook ignores
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if saw != secretNew {
		t.Fatalf("rotate not applied: %q", RedactSecrets(saw))
	}

	// Revoke host — inject fails.
	if err := sess.RevokeHost("api.x.ai"); err != nil {
		t.Fatal(err)
	}
	r, err = CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("revoked host still injects")
	}
}

func TestE2E_MutationAgainst_2c6c7c6_PeerFailOpen(t *testing.T) {
	// If authorizePeer allowed pid==0, this would pass without child AllowPID.
	// We assert CONNECT without registration fails.
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "e2e-mut", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Parent process dials without AllowPID — production peer for parent may resolve
	// to this test PID if we had allowed it; we have not, so must 403.
	c, err := net.DialTimeout("tcp", sess.Mitm.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("CONNECT api.x.ai:443 HTTP/1.1\r\nHost: api.x.ai:443\r\n\r\n"))
	buf := make([]byte, 128)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Fatalf("2c6c7c6 regression: unregistered CONNECT not 403: %q", string(buf[:n]))
	}
}

func TestE2E_LiveStillNeedsFAC169(t *testing.T) {
	restore := SetRequireOSBoundaryForTest(nil)
	defer restore()
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "grok", SessionID: "e2e-live", Prompt: "x"})
	if err == nil {
		t.Fatal()
	}
	if be, ok := err.(*BlockedError); !ok || be.Code != "fac169_required" {
		t.Fatal(err)
	}
}

// Ensure go.mod module path works for build in e2e peer tests.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
