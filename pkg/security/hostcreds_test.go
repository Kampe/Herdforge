package security

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRequiredBrokerHostsForKind(t *testing.T) {
	if got := RequiredBrokerHostsForKind("claude"); got[0] != "api.anthropic.com" {
		t.Fatal(got)
	}
	if got := RequiredBrokerHostsForKind("grok"); got[0] != "api.x.ai" {
		t.Fatal(got)
	}
	if got := RequiredBrokerHostsForKind("codex"); got[0] != "api.openai.com" {
		t.Fatal(got)
	}
	if RequiredBrokerHostsForKind("opencode") != nil {
		t.Fatal("opencode must be out of scope")
	}
}

func TestDiagnoseKindAuthReadiness_NoAPIKeys_External(t *testing.T) {
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	for _, kind := range []string{"codex", "claude", "grok"} {
		d := DiagnoseKindAuthReadiness(kind)
		if d.Brokerable {
			t.Fatalf("%s: must not be brokerable without HostCreds", kind)
		}
		if d.Blocker == "" || !strings.Contains(d.Blocker, "BLOCKED") {
			t.Fatalf("blocker: %q", d.Blocker)
		}
		pkt := FormatKindAuthBlocker(d)
		if strings.Contains(strings.ToLower(pkt), "sk-") || strings.Contains(pkt, "Bearer sk") {
			t.Fatalf("packet leaked secret material: %s", pkt)
		}
		if len(d.RequiredHosts) == 0 {
			t.Fatalf("%s: expected required hosts", kind)
		}
	}
}

func TestDiagnoseKindAuthReadiness_WithAPIKey_OK(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-test-not-real-xxxxxxxx")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	d := DiagnoseKindAuthReadiness("grok")
	if !d.Brokerable || d.Class != KindAuthOK {
		t.Fatalf("expected OK brokerable, got class=%s brokerable=%v blocker=%s", d.Class, d.Brokerable, d.Blocker)
	}
	pkt := FormatKindAuthBlocker(d)
	if strings.Contains(pkt, "sk-test") {
		t.Fatal("must not echo API key")
	}
}

func TestDiagnoseKindAuthReadiness_OpenCodeBlocked(t *testing.T) {
	d := DiagnoseKindAuthReadiness("opencode")
	if d.Brokerable {
		t.Fatal("opencode out of scope")
	}
}

func TestRedactSecrets(t *testing.T) {
	in := "Authorization: Bearer sk-abc123XYZ and sk-other_token_here"
	out := RedactSecrets(in)
	if strings.Contains(out, "sk-abc") || strings.Contains(out, "sk-other") {
		t.Fatalf("not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", out)
	}
}

func TestStartHostCredsSession_MissingCreds_Blocked(t *testing.T) {
	store := NewMemorySecretStore()
	_, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Interactive: false,
	})
	if err == nil {
		t.Fatal("expected BLOCKED")
	}
	if !errors.Is(err, ErrHostCredsBlocked) {
		t.Fatalf("want ErrHostCredsBlocked, got %v", err)
	}
	be := err.(*BlockedError)
	if be.Reason != BlockMissingCreds {
		t.Fatalf("reason=%s", be.Reason)
	}
}

func TestStartHostCredsSession_DummyNotRealCreds(t *testing.T) {
	store := NewMemorySecretStore()
	// Even if someone tries to put dummy in store, Set rejects it.
	if err := store.Set("api.x.ai", DummyNeverUpstreamAuth); err == nil {
		t.Fatal("store must reject dummy")
	}
	// Env dummy also should not make session brokerable via empty store path.
	t.Setenv("XAI_API_KEY", DummyNeverUpstream)
	store2 := NewMemorySecretStore()
	_ = LoadEnvIntoStore(store2)
	// LoadEnv may try Set with Bearer dummy — must not succeed as real cred.
	if store2.Get("api.x.ai") != "" && IsDummyCredential(store2.Get("api.x.ai")) {
		t.Fatal("dummy must not sit in store as real cred")
	}
	_, err := StartHostCredsSession(SessionConfig{Kind: "grok", Store: store2, Interactive: false})
	if err == nil {
		t.Fatal("dummy must not enable session")
	}
}

func TestStartHostCredsSession_InteractiveDenied(t *testing.T) {
	_, err := StartHostCredsSession(SessionConfig{
		Kind:        "fake",
		Interactive: true,
		Store:       NewMemorySecretStore(),
	})
	if err == nil {
		t.Fatal("expected interactive denied")
	}
	if err.(*BlockedError).Reason != BlockInteractiveDenied {
		t.Fatal(err)
	}
}

func TestStartHostCredsSession_WorkerEnvNoSecretsNoBearer(t *testing.T) {
	store := NewMemorySecretStore()
	secret := "Bearer fac170-test-secret-VALUE-xyz"
	if err := store.Set("api.x.ai", secret); err != nil {
		t.Fatal(err)
	}
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Interactive: false,
	})
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
	// Dummy present for CLI bootstrap.
	if sess.WorkerEnvMap()["XAI_API_KEY"] != DummyNeverUpstream {
		t.Fatalf("expected dummy XAI key, got %q", sess.WorkerEnvMap()["XAI_API_KEY"])
	}
	// Socket channel present, no proxy URL with credentials.
	if sess.WorkerEnvMap()["HERD_HOSTCREDS_SOCKET"] == "" {
		t.Fatal("expected socket channel")
	}
	if !strings.Contains(sess.WorkerEnvMap()["HERD_HOSTCREDS_CHANNEL"], "oracle") {
		t.Fatal("expected oracle channel marker")
	}
}

func TestAllowlist_HostMethodPath(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer real-secret-111")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Interactive: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Host denied
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "evil.example.com", Method: "POST", Path: "/v1/chat/completions",
	})
	if err != nil || r.OK {
		t.Fatalf("host deny failed: %+v %v", r, err)
	}
	// Path denied
	r, err = CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "POST", Path: "/v1/admin/keys",
	})
	if err != nil || r.OK {
		t.Fatalf("path deny failed: %+v %v", r, err)
	}
	// Method denied
	r, err = CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "api.x.ai", Method: "DELETE", Path: "/v1/chat/completions",
	})
	if err != nil || r.OK {
		t.Fatalf("method deny failed: %+v %v", r, err)
	}
}

func TestRotateRevokeRestartExpiry(t *testing.T) {
	store := NewMemorySecretStore()
	oldSec := "Bearer old-secret-111"
	newSec := "Bearer new-secret-222"
	_ = store.Set("api.x.ai", oldSec)

	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Interactive: false,
		TTL:         time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	gen1 := sess.Oracle.Generation()
	if err := sess.Rotate("api.x.ai", newSec); err != nil {
		t.Fatal(err)
	}
	if store.Get("api.x.ai") != newSec {
		t.Fatal("store not rotated")
	}
	if err := sess.AssertWorkerCannotSeeSecret(newSec); err != nil {
		t.Fatal(err)
	}

	if err := sess.RevokeHost("api.x.ai"); err != nil {
		t.Fatal(err)
	}
	if store.Get("api.x.ai") != "" {
		t.Fatal("store still has secret after revoke")
	}

	_ = store.Set("api.x.ai", newSec)
	if err := sess.Restart(); err != nil {
		t.Fatal(err)
	}
	if sess.Oracle.Generation() <= gen1 {
		t.Fatalf("generation should bump: before=%d after=%d", gen1, sess.Oracle.Generation())
	}

	// Short TTL expiry
	s2, err := StartHostCredsSession(SessionConfig{
		Kind: "grok", Store: store, Interactive: false, TTL: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	time.Sleep(40 * time.Millisecond)
	if err := s2.Oracle.Alive(); err == nil {
		t.Fatal("expected expired")
	} else if be, ok := err.(*BlockedError); !ok || be.Reason != BlockExpired {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestExactSessionCausalProof(t *testing.T) {
	store := NewMemorySecretStore()
	secret := "Bearer proof-secret-fac170-AAA"
	_ = store.Set("api.x.ai", secret)

	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "fake",
		Store:       store,
		Interactive: false,
		ExtraHosts:  []string{"127.0.0.1"},
		TestRules: append(DefaultRequestRules(), RequestRule{
			Host: "127.0.0.1", Method: "POST", PathPrefix: "/v1/chat/completions", Action: "chat.completions",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	marker := "HOSTCREDS_ALLOWED_OK_FAC170"
	proof, err := ProveExactSessionHostCreds(sess, secret, marker)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.PromptConsumed || !proof.AllowedMarkerReach || !proof.ForbiddenAccessDeny ||
		!proof.WorkerSecretHidden || !proof.NoWorkerBearer || !proof.DummyNeverUpstream {
		t.Fatalf("incomplete proof: %+v", proof)
	}
	if proof.SessionID != sess.ID {
		t.Fatal("session mismatch")
	}
}

func TestStartAuthorSessionNonInteractive_BlockedWithoutCreds(t *testing.T) {
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	_, err := StartAuthorSessionNonInteractive("grok", t.TempDir(), NewMemorySecretStore())
	if err == nil {
		t.Fatal("expected blocked")
	}
	if !errors.Is(err, ErrHostCredsBlocked) {
		t.Fatalf("got %v", err)
	}
}

func TestStartAuthorSessionNonInteractive_GrokWithCreds(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer live-test-key-not-real")
	sess, err := StartAuthorSessionNonInteractive("grok", t.TempDir(), store)
	if err != nil {
		if be, ok := err.(*BlockedError); ok && be.Reason == BlockUnsupportedPlat {
			t.Skip(be.Detail)
		}
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.ConsumePrompt("non-interactive author turn"); err != nil {
		t.Fatal(err)
	}
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		t.Fatal(err)
	}
	if err := sess.AssertWorkerCannotSeeSecret("live-test-key-not-real"); err != nil {
		t.Fatal(err)
	}
}

func TestMatchRequestRule_Boundary(t *testing.T) {
	rules := []RequestRule{{Host: "api.x.ai", Method: "POST", PathPrefix: "/v1/chat/completions"}}
	if MatchRequestRule(rules, "api.x.ai", "POST", "/v1/chat/completions") == nil {
		t.Fatal("exact")
	}
	if MatchRequestRule(rules, "api.x.ai", "POST", "/v1/chat/completions/extra") == nil {
		t.Fatal("prefix/")
	}
	if MatchRequestRule(rules, "api.x.ai", "POST", "/v1/chat/completionss") != nil {
		t.Fatal("must not match chatty suffix")
	}
	if MatchRequestRule(rules, "api.x.ai", "GET", "/v1/chat/completions") != nil {
		t.Fatal("method")
	}
}

func TestPreopenedFD(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer fd-secret")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", Store: store, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	f, err := sess.OpenPreopenedFD()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Fd() == 0 {
		// fd 0 is stdin; ExtraFiles FD would be higher — still a valid File.
	}
}
