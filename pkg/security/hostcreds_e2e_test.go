package security

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func installLocalOrigin(t *testing.T, mitm *TLSMitmProxy, host, expectAuth string) (saw *string, cleanup func()) {
	t.Helper()
	var got string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "path", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"local","choices":[]}`)
	}))
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	mitm.SetResolveHook(func(h string) (net.IP, error) {
		if h == host {
			return net.IPv4(1, 2, 3, 4), nil
		}
		return resolveAndPinIP(h)
	})
	mitm.SetDialHook(func(h string, ip net.IP) (net.Conn, error) {
		if h != host {
			return nil, &net.OpError{Op: "dial", Err: io.EOF}
		}
		return net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 3*time.Second)
	})
	_ = expectAuth
	return &got, up.Close
}

// Required causal path: REAL author child (author-causal) computes marker,
// allowlisted TLS via one-shot FD, bound receipt; no post-hoc helper.
func TestE2E_AuthorCausal_ExactSession(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build")
	}
	v := NewTestCredentialVault()
	secret := "Bearer e2e-author-secret"
	_ = v.InstallTestSecret("api.x.ai", secret)
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "e2e-author", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	saw, cleanup := installLocalOrigin(t, sess.Mitm, "api.x.ai", secret)
	defer cleanup()

	cap, err := NewCapability(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, rcpt, err := sess.RunAuthorCausalProbe(cap.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if res.PID <= 0 {
		t.Fatal("author pid missing")
	}
	if res.CapabilityMarker != cap.Expected {
		t.Fatalf("marker %q want %q", res.CapabilityMarker, cap.Expected)
	}
	if strings.Contains(CapabilityPrompt(cap), cap.Expected) {
		t.Fatal("expected must not appear in prompt")
	}
	if !res.TLSRequestOK {
		t.Fatal("author full TLS required")
	}
	if *saw != secret {
		t.Fatalf("inject not applied: %q", RedactSecrets(*saw))
	}
	if !rcpt.InjectOK || rcpt.SessionID != sess.ID || rcpt.PeerPort != res.PeerPort {
		t.Fatalf("receipt not bound: %+v", rcpt)
	}
	if rcpt.CapabilityNonce != cap.Nonce {
		t.Fatal("receipt nonce unbound")
	}
	// Helper probe must not be admission.
	if _, err := sess.RunWorkerForbiddenAndAllowProbe(cap.Nonce); err == nil {
		t.Fatal("helper probe must not be admission")
	}
}

func TestE2E_OneShotPortReplayDenied(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "e2e-replay", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Upstream may fail; peer consume still tested.
	sess.Mitm.SetDialHook(func(host string, ip net.IP) (net.Conn, error) {
		return nil, io.EOF
	})
	sess.Mitm.SetResolveHook(func(host string) (net.IP, error) {
		return net.IPv4(1, 2, 3, 4), nil
	})
	if err := ProvePortReplayDenied(sess.Mitm, "api.x.ai"); err != nil {
		t.Fatal(err)
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

func TestE2E_MITM_RotateRestart_AuthorCausal(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build")
	}
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer rot-old")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "e2e-rot", Authority: v, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	secretNew := "Bearer rot-new-xyz"
	if err := v.RotateTestSecret("api.x.ai", secretNew); err != nil {
		t.Fatal(err)
	}
	if err := sess.Restart(); err != nil {
		t.Fatal(err)
	}
	if sess.Mitm == nil || sess.Mitm.Addr() == "" {
		t.Fatal("mitm dead")
	}

	saw, cleanup := installLocalOrigin(t, sess.Mitm, "api.x.ai", secretNew)
	defer cleanup()

	cap, err := NewCapability(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, rcpt, err := sess.RunAuthorCausalProbe(cap.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TLSRequestOK || !rcpt.InjectOK {
		t.Fatal("post-restart author causal required")
	}
	if *saw != secretNew {
		t.Fatalf("rotate not applied via MITM author: %q", RedactSecrets(*saw))
	}
}

func TestE2E_Mutation_PeerAttributionRequired(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "e2e-mut", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	c, err := net.DialTimeout("tcp", sess.Mitm.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("CONNECT api.x.ai:443 HTTP/1.1\r\nHost: api.x.ai:443\r\n\r\n"))
	buf := make([]byte, 128)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Fatalf("unregistered CONNECT not 403: %q", string(buf[:n]))
	}
}

func TestE2E_ExactEnvNoDuplicateKeys(t *testing.T) {
	env := ExactWorkerChildEnv(
		[]string{"PATH=/a", "XAI_API_KEY=sk-bad"},
		[]string{"PATH=/b", "XAI_API_KEY=", "FOO=1"},
		[]string{"FOO=2"},
	)
	if err := assertExactEnvNoSecrets(env); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range env {
		if e == "FOO=2" {
			found = true
		}
		if strings.HasPrefix(e, "XAI_API_KEY=") && e != "XAI_API_KEY=" {
			t.Fatal(e)
		}
	}
	if !found {
		t.Fatal("last key win failed")
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

func TestE2E_LiveRejectsTestVaultAuthority(t *testing.T) {
	// Even with a fake boundary, test vault / in-process authority must fail.
	restore := SetRequireOSBoundaryForTest(func() (OSBoundary, error) {
		return fakeBoundary{}, nil
	})
	defer restore()
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	_, _, _, err := StartAuthorLive(LiveConfig{
		Kind: "grok", SessionID: "e2e-tv", Prompt: "x", Authority: v, CausalAuthorOnly: true,
	})
	if err == nil {
		t.Fatal("test vault must not live-admit")
	}
	be, ok := err.(*BlockedError)
	if !ok || (be.Code != "test_vault_not_live" && be.Code != "authority_not_fac169_ipc") {
		t.Fatal(err)
	}
}

func TestE2E_HelperCannotSatisfyBrokerReached(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess")
	}
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "e2e-help", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	saw, cleanup := installLocalOrigin(t, sess.Mitm, "api.x.ai", "Bearer x")
	defer cleanup()
	cap, _ := NewCapability(sess.ID)
	// Author causal produces receipt for port P.
	res, rcpt, err := sess.RunAuthorCausalProbe(cap.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	_ = saw
	// Receipt already consumed by ProveAuthorCausalSession.
	if !rcpt.Consumed {
		t.Fatal("receipt should be consumed")
	}
	// Helper must not be usable for admission.
	if _, err := ProveAllowlistedHostViaWorker(sess.Mitm, "api.x.ai", "evil.example.invalid", sess.ID, cap.Nonce); err == nil {
		t.Fatal("helper must fail closed")
	}
	// Wrong peer port cannot consume.
	if _, ok := sess.Mitm.ConsumeReceiptFor(sess.ID, cap.Nonce, res.PeerPort+1, ""); ok {
		t.Fatal("wrong port consumed receipt")
	}
}

// An upstream that REJECTS the injected credential must not be recorded as a
// successful brokered call. TLSRequestOK was `resp.StatusCode > 0`, so 401/403/
// 500 all set it, and it gates ExitOK, ProveAuthorCausalSession and
// StartAuthorLive's proof check.
func TestE2E_UpstreamRejectionIsNotProof(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build")
	}
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer stale-rotated-out")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "e2e-401", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Origin behaves like a provider refusing a revoked key.
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	sess.Mitm.SetResolveHook(func(h string) (net.IP, error) { return net.IPv4(1, 2, 3, 4), nil })
	sess.Mitm.SetDialHook(func(h string, ip net.IP) (net.Conn, error) {
		return net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 3*time.Second)
	})

	cap, err := NewCapability(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := sess.RunAuthorCausalProbe(cap.Nonce)
	if err == nil {
		t.Fatal("a 401 from upstream must not satisfy the author causal proof")
	}
	if res == nil {
		t.Fatal("expected a result to inspect")
	}
	if res.TLSStatus != http.StatusUnauthorized {
		t.Fatalf("status %d want 401", res.TLSStatus)
	}
	if !res.TLSResponseReceived {
		t.Fatal("a response did parse; TLSResponseReceived should be true")
	}
	if res.TLSRequestOK {
		t.Fatal("MUTATION: upstream 401 recorded as a successful brokered call")
	}
	if res.ExitOK {
		t.Fatal("ExitOK must not survive an upstream rejection")
	}
}

type fakeBoundary struct{}

func (fakeBoundary) Mechanism() string       { return "test-fake" }
func (fakeBoundary) ProbeDigest() string     { return "fake" }
func (fakeBoundary) AdversarialProbe() error { return nil }

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
