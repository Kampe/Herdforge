package security

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRequiredBrokerHostsForKind_PerKindOnly(t *testing.T) {
	if got := RequiredBrokerHostsForKind("grok"); len(got) != 1 || got[0] != "api.x.ai" {
		t.Fatal(got)
	}
	for _, r := range RequestRulesForKind("grok") {
		if r.Host != "api.x.ai" {
			t.Fatal(r)
		}
	}
}

func TestDiagnose_RawEnvNotBrokerable(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-raw-env-not-authority")
	_ = os.Unsetenv(envHostCredsHandles)
	d := DiagnoseKindAuthReadiness("grok")
	if d.Brokerable {
		t.Fatal()
	}
	if strings.Contains(FormatKindAuthBlocker(d), "sk-raw") {
		t.Fatal()
	}
}

func TestWorkerEnv_NoAPIKeys(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer secret-xyz")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "s1", Authority: v, Interactive: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		t.Fatal(err)
	}
	env := sess.WorkerEnvMap()
	if env["XAI_API_KEY"] != "" || env["OPENAI_API_KEY"] != "" {
		t.Fatalf("api keys must be empty, got %q", env["XAI_API_KEY"])
	}
	if !strings.HasPrefix(env["HTTPS_PROXY"], "http://127.0.0.1:") {
		t.Fatal(env["HTTPS_PROXY"])
	}
	if strings.Contains(env["HTTPS_PROXY"], "@") {
		t.Fatal("proxy must not carry credentials")
	}
	if env["SSL_CERT_FILE"] == "" {
		t.Fatal("need public CA")
	}
}

func TestSession_GrokCannotCallOpenAI(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	_ = v.InstallTestSecret("api.openai.com", "Bearer y") // present but must not be usable by grok session
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "s2", Authority: v, Interactive: false, EnableOracle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := ProveMITMExactHost(sess.Mitm, "api.openai.com"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionIDRequired(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	_, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "", Authority: v})
	if err == nil {
		t.Fatal("session id required")
	}
}

func TestOracle_SessionIDRequired(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "s3", Authority: v, EnableOracle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: "", Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("empty session must fail")
	}
}

func TestExactSessionComponentProof(t *testing.T) {
	v := NewTestCredentialVault()
	secret := "Bearer proof-secret-fac170-AAA"
	_ = v.InstallTestSecret("127.0.0.1", secret)
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "fake", SessionID: "s-proof", Authority: v, AllowLoopback: true, EnableOracle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	proof, err := ProveExactSessionHostCreds(sess, secret, "HOSTCREDS_ALLOWED_OK")
	if err != nil {
		t.Fatal(err)
	}
	if !proof.PromptConsumed || !proof.AllowedMarkerReach || !proof.ForbiddenAccessDeny || !proof.NoAPIKeys {
		t.Fatalf("%+v", proof)
	}
}

func TestRotateRestartSafe(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer old")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", SessionID: "s-rot", Authority: v, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	oldAddr := sess.Mitm.Addr()
	_ = v.RotateTestSecret("api.x.ai", "Bearer new")
	if err := sess.Restart(); err != nil {
		t.Fatal(err)
	}
	if sess.Mitm.Addr() == "" {
		t.Fatal("mitm dead after restart")
	}
	// old may differ
	_ = oldAddr
	if !v.Has("api.x.ai") {
		t.Fatal()
	}
}

func TestAuthority_NoGetSnapshot(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer test-secret-aaa")
	if err := AssertNoPublicSecretExport(v); err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	if err := v.InjectAuthorization("api.x.ai", hdr); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedError_Stable(t *testing.T) {
	be := &BlockedError{Reason: BlockMissingCreds, Code: "missing:api.x.ai"}
	if !errors.Is(be, ErrHostCredsBlocked) {
		t.Fatal()
	}
	if strings.Contains(be.Error(), "sk-") {
		t.Fatal()
	}
}

func TestRedactSecrets_APIKeyJSON(t *testing.T) {
	in := `{"error":{"message":"bad","type":"auth"},"api_key":"sk-abc123secrettoken"}`
	out := RedactSecrets(in)
	if strings.Contains(out, "sk-abc") {
		t.Fatal(out)
	}
}

func TestParseHandles_RejectSecrets(t *testing.T) {
	m := ParseHandlesEnv("api.x.ai=Bearer sk-x;api.openai.com=op://a/b/c")
	if _, ok := m["api.x.ai"]; ok {
		t.Fatal()
	}
	if m["api.openai.com"] != "op://a/b/c" {
		t.Fatal(m)
	}
}

func TestLive_RefusesWithoutFAC169(t *testing.T) {
	restore := SetRequireOSBoundaryForTest(nil)
	defer restore()
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "grok", SessionID: "L1", Prompt: "x"})
	if err == nil {
		t.Fatal()
	}
	if be, ok := err.(*BlockedError); !ok || be.Code != "fac169_required" {
		t.Fatal(err)
	}
}

func TestLive_RefusesFake(t *testing.T) {
	// Boundary passes so the kind gate is the only thing that can reject;
	// a bare err != nil check was satisfied by the fac169_required default.
	restore := SetRequireOSBoundaryForTest(func() (OSBoundary, error) {
		return fakeBoundary{}, nil
	})
	defer restore()

	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "fake", Prompt: "x"})
	be, ok := err.(*BlockedError)
	if !ok {
		t.Fatalf("want *BlockedError, got %T (%v)", err, err)
	}
	if be.Reason != BlockUnbrokerableKind || be.Code != "live_kind" {
		t.Fatalf("reason=%q code=%q — fake must die on the kind gate", be.Reason, be.Code)
	}
}
