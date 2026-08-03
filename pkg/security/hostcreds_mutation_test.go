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
	if testing.Short() {
		t.Skip("subprocess")
	}
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

func TestMutation_UnregisteredPortDenied(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m-port", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
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

func TestMutation_InProcessAuthorityNotLive(t *testing.T) {
	restore := SetRequireOSBoundaryForTest(func() (OSBoundary, error) {
		return fakeBoundary{}, nil
	})
	defer restore()
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "grok", SessionID: "m-auth", Authority: v})
	if err == nil {
		t.Fatal("MUTATION: in-process test vault live-admitted")
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

func TestMutation_EnvironAppendBanned(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-parent-secret-must-not-leak")
	env := ExactWorkerChildEnv([]string{"PATH=/usr/bin"}, []string{"XAI_API_KEY="})
	if err := assertExactEnvNoSecrets(env); err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		if strings.Contains(e, "sk-parent") {
			t.Fatal("MUTATION: parent secret in exact env")
		}
	}
}

func TestMutation_HelperProbeNotAdmission(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m-help", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.RunWorkerForbiddenAndAllowProbe("n"); err == nil {
		t.Fatal("MUTATION: helper probe admitted")
	}
	if _, err := ProveAllowlistedHostViaWorker(sess.Mitm, "api.x.ai", "evil", sess.ID, "n"); err == nil {
		t.Fatal("MUTATION: ProveAllowlistedHostViaWorker admitted")
	}
}
