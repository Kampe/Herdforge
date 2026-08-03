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
	if RequiredBrokerHostsForKind("opencode") != nil {
		t.Fatal("opencode out of scope")
	}
	// No global eight-host list API.
	rules := RequestRulesForKind("grok")
	for _, r := range rules {
		if r.Host != "api.x.ai" {
			t.Fatalf("grok rules must not include %s", r.Host)
		}
	}
}

func TestDiagnose_RawEnvNotBrokerable(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-raw-env-not-authority")
	t.Setenv(envHostCredsHandles, "")
	_ = os.Unsetenv(envHostCredsHandles)
	d := DiagnoseKindAuthReadiness("grok")
	if d.Brokerable {
		t.Fatal("raw env must not make brokerable")
	}
	if d.ReasonCode != "env_not_production_authority" && d.ReasonCode != "missing_handle_creds" {
		// Either is acceptable; must not be ok.
		if d.Class == KindAuthOK {
			t.Fatal(d)
		}
	}
	pkt := FormatKindAuthBlocker(d)
	if strings.Contains(pkt, "sk-raw") {
		t.Fatal("leaked key")
	}
}

func TestDiagnose_OpenCodeBlocked(t *testing.T) {
	d := DiagnoseKindAuthReadiness("opencode")
	if d.Brokerable {
		t.Fatal()
	}
}

func TestBlockedError_NoFreeDetail(t *testing.T) {
	be := &BlockedError{Reason: BlockMissingCreds, Code: "missing:api.x.ai", Kind: "grok"}
	// Compile-time: no Detail field usage — Error is stable codes only.
	if strings.Contains(be.Error(), "sk-") {
		t.Fatal()
	}
	if !errors.Is(be, ErrHostCredsBlocked) {
		t.Fatal()
	}
}

func TestAuthority_NoGetSnapshot(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer test-secret-aaa")
	// Interface has no Get/Snapshot — only Has/Hosts/Inject.
	if !v.Has("api.x.ai") {
		t.Fatal()
	}
	if err := AssertNoPublicSecretExport(v); err != nil {
		t.Fatal(err)
	}
	hdr := make(http.Header)
	if err := v.InjectAuthorization("api.x.ai", hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("Authorization") != "Bearer test-secret-aaa" {
		t.Fatal("inject internal only")
	}
	// Callers outside package cannot Get — verified by interface shape in integration tests.
}

func TestMemoryIsTestOnly_NotDurable(t *testing.T) {
	v := NewTestCredentialVault()
	if v.Durable() {
		t.Fatal("test vault must not claim durable")
	}
	if v.Class() != "test" {
		t.Fatal()
	}
	ha := NewHandleAuthority()
	if !ha.Durable() {
		t.Fatal("handle authority must be durable")
	}
}

func TestStartSession_MissingCreds(t *testing.T) {
	v := NewTestCredentialVault()
	_, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err == nil {
		t.Fatal()
	}
	if !errors.Is(err, ErrHostCredsBlocked) {
		t.Fatal(err)
	}
}

func TestStartSession_WorkerEnvNoSecrets(t *testing.T) {
	v := NewTestCredentialVault()
	secret := "Bearer fac170-secret-xyz"
	_ = v.InstallTestSecret("api.x.ai", secret)
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AssertWorkerCannotSeeSecret(secret); err != nil {
		t.Fatal(err)
	}
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		t.Fatal(err)
	}
	if sess.WorkerEnvMap()["XAI_API_KEY"] != DummyNeverUpstream {
		t.Fatal()
	}
}

func TestNormalizeHost(t *testing.T) {
	h, err := NormalizeHost("API.X.AI.", false)
	if err != nil || h != "api.x.ai" {
		t.Fatalf("%q %v", h, err)
	}
	h, err = NormalizeHost("api.x.ai:443", false)
	if err != nil || h != "api.x.ai" {
		t.Fatal(h, err)
	}
	if _, err := NormalizeHost("api.x.ai:8443", false); err == nil {
		t.Fatal("non-default port must deny")
	}
	if _, err := NormalizeHost("http://api.x.ai", false); err == nil {
		t.Fatal()
	}
	if _, err := NormalizeHost("evil\r\nHost: x", false); err == nil {
		t.Fatal()
	}
	h, err = NormalizeHost("127.0.0.1", true)
	if err != nil || h != "127.0.0.1" {
		t.Fatal(h, err)
	}
	if _, err := NormalizeHost("127.0.0.1", false); err == nil {
		t.Fatal("loopback denied without flag")
	}
}

func TestValidateAuthorization_CRLF(t *testing.T) {
	if err := ValidateAuthorizationMaterial("Bearer good-token"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAuthorizationMaterial("Bearer x\r\nX-Inject: 1"); err == nil {
		t.Fatal("CRLF must fail")
	}
	if err := ValidateAuthorizationMaterial(DummyNeverUpstreamAuth); err == nil {
		t.Fatal("dummy must fail as material")
	}
}

func TestExactSessionCausalProof(t *testing.T) {
	v := NewTestCredentialVault()
	secret := "Bearer proof-secret-fac170-AAA"
	_ = v.InstallTestSecret("127.0.0.1", secret)
	sess, err := StartHostCredsSession(SessionConfig{
		Kind: "fake", Authority: v, Interactive: false, AllowLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	proof, err := ProveExactSessionHostCreds(sess, secret, "HOSTCREDS_ALLOWED_OK_FAC170")
	if err != nil {
		t.Fatal(err)
	}
	if !proof.PromptConsumed || !proof.AllowedMarkerReach || !proof.ForbiddenAccessDeny ||
		!proof.WorkerSecretHidden || !proof.NoWorkerBearer || !proof.DummyNeverUpstream || !proof.NoSecretExportAPI {
		t.Fatalf("%+v", proof)
	}
}

func TestRotateRevokeRestart(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer old-secret-111")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Authority: v, Interactive: false, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	gen1 := sess.Oracle.Generation()

	if err := v.RotateTestSecret("api.x.ai", "Bearer new-secret-222"); err != nil {
		t.Fatal(err)
	}
	if !v.Has("api.x.ai") {
		t.Fatal()
	}
	if err := sess.AssertWorkerCannotSeeSecret("Bearer new-secret-222"); err != nil {
		t.Fatal(err)
	}

	if err := sess.RevokeHost("api.x.ai"); err != nil {
		t.Fatal(err)
	}
	if v.Has("api.x.ai") {
		t.Fatal("revoked")
	}
	_ = v.InstallTestSecret("api.x.ai", "Bearer new-secret-222")
	if err := sess.Restart(); err != nil {
		t.Fatal(err)
	}
	if sess.Oracle.Generation() <= gen1 {
		t.Fatal("generation")
	}
	if !sess.Oracle.HostCredentialPresent("api.x.ai") {
		t.Fatal("restart keeps authority object for test vault")
	}

	// Durable handle authority restart re-resolves via mock.
	ha := NewHandleAuthority()
	ha.resolve = func(handle string) (string, error) {
		if handle == "keychain:test-xai" {
			return "rotated-token-zzz", nil
		}
		return "", errors.New("nope")
	}
	if err := ha.InstallFromHandle("api.x.ai", "keychain:test-xai"); err != nil {
		t.Fatal(err)
	}
	// Rotate handle material
	call := 0
	ha.resolve = func(handle string) (string, error) {
		call++
		return "after-restart-token", nil
	}
	if err := ha.ReResolveAll(); err != nil {
		t.Fatal(err)
	}
	if call == 0 {
		t.Fatal("expected re-resolve")
	}
	hdr := make(http.Header)
	if err := ha.InjectAuthorization("api.x.ai", hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("Authorization") != "Bearer after-restart-token" {
		t.Fatal(hdr.Get("Authorization"))
	}
}

func TestParseHandlesEnv_RejectsSecrets(t *testing.T) {
	m := ParseHandlesEnv("api.x.ai=Bearer sk-evil;api.openai.com=op://V/i/f")
	if _, ok := m["api.x.ai"]; ok {
		t.Fatal("must reject bearer as handle")
	}
	if m["api.openai.com"] != "op://V/i/f" {
		t.Fatal(m)
	}
}

func TestRedactSecrets(t *testing.T) {
	out := RedactSecrets("Authorization: Bearer sk-abc123XYZ")
	if strings.Contains(out, "sk-abc") {
		t.Fatal(out)
	}
}
