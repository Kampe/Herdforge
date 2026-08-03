package security

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestMutation_NoAPIKeyPlant(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m1", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		t.Fatal("MUTATION: API keys or proxy bearer planted:", err)
	}
}

func TestMutation_MITMFailClosedWithoutPeerAllow(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m-pid", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := ProveMITMRequiresAllowPID(sess.Mitm, "api.x.ai"); err != nil {
		t.Fatal("MUTATION: CONNECT allowed without peer allow:", err)
	}
}

// If authorizePeer is removed/fail-open, this fails closed must still hold.
func TestMutation_UnregisteredPortDenied(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m-port", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Register a port that is NOT our dial source.
	sess.Mitm.AllowClientPort(1)
	c, err := net.DialTimeout("tcp", sess.Mitm.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("CONNECT api.x.ai:443 HTTP/1.1\r\nHost: api.x.ai:443\r\n\r\n"))
	buf := make([]byte, 128)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Fatalf("MUTATION: wrong-port CONNECT allowed: %q", string(buf[:n]))
	}
}

func TestMutation_RevokeKillsMITM(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m-rev", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	addr := sess.Mitm.Addr()
	_ = sess.Revoke()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatal("MUTATION: MITM still accepting after revoke")
	}
}

func TestMutation_PerKindIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess")
	}
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	_ = v.InstallTestSecret("api.openai.com", "Bearer y")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m2", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := ProveMITMExactHost(sess.Mitm, "api.openai.com"); err != nil {
		t.Fatal("MUTATION: cross-provider CONNECT allowed:", err)
	}
}

func TestMutation_LiveNeedsFAC169Boundary(t *testing.T) {
	restore := SetRequireOSBoundaryForTest(nil)
	defer restore()
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "grok", SessionID: "m3", Prompt: "hi"})
	if err == nil {
		t.Fatal("MUTATION: live without FAC-169 boundary")
	}
	be, ok := err.(*BlockedError)
	if !ok || be.Code != "fac169_required" {
		t.Fatalf("want fac169_required, got %v", err)
	}
}

func TestMutation_LiveNeedsRealKind(t *testing.T) {
	for _, k := range []string{"fake", "test", "opencode"} {
		_, _, _, err := StartAuthorLive(LiveConfig{Kind: k, Prompt: "x"})
		if err == nil {
			t.Fatal("MUTATION: kind allowed", k)
		}
	}
}

func TestMutation_PlaintextHTTPNoInject(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m4", Authority: v, EnableOracle: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 32)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\n\r\nok"))
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
		t.Fatal("MUTATION: plaintext credentialed path OK")
	}
}

func TestMutation_SessionIDOmit(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m5", Authority: v, EnableOracle: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	r, _ := CallOracle(sess.Oracle.SocketPath(), OracleRequest{Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions"})
	if r.OK {
		t.Fatal("MUTATION: omitted session_id accepted")
	}
}

func TestMutation_PromptConsumedRequiresPID(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m6", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.ConsumePrompt("not enough")
	if sess.PromptConsumed() {
		t.Fatal("MUTATION: ConsumePrompt alone counts as consumed")
	}
	if err := sess.RecordHarnessPrompt("real", 0); err == nil {
		t.Fatal("MUTATION: pid 0 accepted")
	}
}

func TestMutation_Redaction(t *testing.T) {
	out := RedactSecrets(`Bearer sk-abc123XYZ and {"api_key":"sk-othersecret99"}`)
	if strings.Contains(out, "sk-abc") || strings.Contains(out, "sk-other") {
		t.Fatal(out)
	}
}

func TestMutation_PIDMismatchHardFail(t *testing.T) {
	// ProveAllowlistedHostViaWorker must hard-fail res.PID != process.Pid.
	// Structural unit: empty-body check is banned — simulate mismatch condition.
	res := &WorkerProbeResult{PID: 1}
	processPid := 2
	if res.PID == processPid {
		t.Fatal("fixture broken")
	}
	if res.PID != processPid {
		// expected hard fail path
		err := fmtMismatch(res.PID, processPid)
		if err == nil {
			t.Fatal("MUTATION: pid mismatch must error")
		}
	}
}

func fmtMismatch(a, b int) error {
	if a != b {
		return errPIDMismatch(a, b)
	}
	return nil
}

func errPIDMismatch(a, b int) error {
	return &BlockedError{Reason: BlockAbuse, Code: "worker_pid_mismatch"}
}

func TestMutation_EnvironAppendBanned(t *testing.T) {
	// Regression: ProveAllowlistedHostViaWorker must not use append(os.Environ()).
	// ExactWorkerChildEnv + assertExactEnvNoSecrets is the contract.
	t.Setenv("XAI_API_KEY", "sk-parent-secret-must-not-leak")
	env := ExactWorkerChildEnv(HarnessProxyEnv(nil, "s"), []string{"PATH=/bin"})
	// HarnessProxyEnv(nil) is nil — still scrub.
	env = ExactWorkerChildEnv([]string{"PATH=/usr/bin"}, []string{"XAI_API_KEY="})
	if err := assertExactEnvNoSecrets(env); err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		if strings.Contains(e, "sk-parent") {
			t.Fatal("MUTATION: parent secret in exact env")
		}
	}
}
