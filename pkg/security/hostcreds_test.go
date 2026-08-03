package security

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if RequiredBrokerHostsForKind("nope") != nil {
		t.Fatal("unknown")
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
		if d.Class != KindAuthExternal && d.Class != KindAuthConfig && d.Class != KindAuthPlatform {
			t.Fatalf("%s class=%s", kind, d.Class)
		}
		if d.Blocker == "" || !strings.Contains(d.Blocker, "BLOCKED") {
			t.Fatalf("blocker: %q", d.Blocker)
		}
		pkt := FormatKindAuthBlocker(d)
		if strings.Contains(strings.ToLower(pkt), "sk-") || strings.Contains(pkt, "Bearer sk") {
			t.Fatalf("packet leaked secret material: %s", pkt)
		}
		// hosts_required present, hosts_creds empty
		if len(d.RequiredHosts) == 0 {
			t.Fatalf("%s: expected required hosts", kind)
		}
		if len(d.HostCredsPresent) != 0 {
			t.Fatalf("%s: expected no present creds, got %v", kind, d.HostCredsPresent)
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
	if !containsHost(d.HostCredsPresent, "api.x.ai") {
		t.Fatalf("hosts_creds should list api.x.ai, got %v", d.HostCredsPresent)
	}
}

func TestDiagnoseKindAuthReadiness_OpenCodeBlocked(t *testing.T) {
	d := DiagnoseKindAuthReadiness("opencode")
	if d.Brokerable {
		t.Fatal("opencode out of scope")
	}
	if d.Class != KindAuthConfig {
		t.Fatalf("class=%s", d.Class)
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
	// no creds
	_, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected BLOCKED")
	}
	if !errors.Is(err, ErrHostCredsBlocked) {
		t.Fatalf("want ErrHostCredsBlocked, got %v", err)
	}
	be, ok := err.(*BlockedError)
	if !ok {
		t.Fatalf("want *BlockedError, got %T", err)
	}
	if be.Reason != BlockMissingCreds {
		t.Fatalf("reason=%s", be.Reason)
	}
	if !containsHost(be.HostsRequired, "api.x.ai") {
		t.Fatalf("hosts_required=%v", be.HostsRequired)
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
	be := err.(*BlockedError)
	if be.Reason != BlockInteractiveDenied {
		t.Fatalf("reason=%s", be.Reason)
	}
}

func TestStartHostCredsSession_WorkerEnvNoSecrets(t *testing.T) {
	store := NewMemorySecretStore()
	secret := "Bearer fac170-test-secret-VALUE-xyz"
	if err := store.Set("api.x.ai", secret); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Worktree:    wt,
		Interactive: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.AssertWorkerCannotSeeSecret(secret); err != nil {
		t.Fatal(err)
	}
	if err := sess.AssertWorkerCannotSeeSecret("fac170-test-secret-VALUE-xyz"); err != nil {
		t.Fatal(err)
	}
	// CA file exists and is public.
	if sess.CAPath == "" {
		t.Fatal("expected CA path")
	}
	if _, err := os.Stat(sess.CAPath); err != nil {
		t.Fatal(err)
	}
	// Contain dir must not store secret files.
	entries, _ := os.ReadDir(filepath.Join(wt, ".herd", "contain"))
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(wt, ".herd", "contain", e.Name()))
		if strings.Contains(string(b), "fac170-test-secret") {
			t.Fatalf("secret in worktree file %s", e.Name())
		}
	}
}

func TestAllowlist_DenyNonAllowlisted(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Allowlist:   []string{"api.x.ai"},
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if sess.Proxy.HostAllowed("evil.example.com") {
		t.Fatal("evil must not be allowed")
	}
	if !sess.Proxy.HostAllowed("api.x.ai") {
		t.Fatal("api.x.ai must be allowed")
	}
	// Setting cred for non-allowlisted host fails.
	if err := sess.Proxy.SetHostCredential("evil.example.com", "Bearer bad"); err == nil {
		t.Fatal("expected deny for non-allowlisted host cred")
	}
}

func TestRotateRevokeRestart(t *testing.T) {
	store := NewMemorySecretStore()
	oldSec := "Bearer old-secret-111"
	newSec := "Bearer new-secret-222"
	_ = store.Set("api.x.ai", oldSec)

	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if !sess.Proxy.HostCredentialPresent("api.x.ai") {
		t.Fatal("expected cred present")
	}
	gen1 := sess.Proxy.Generation()

	// Rotate
	if err := sess.Rotate("api.x.ai", newSec); err != nil {
		t.Fatal(err)
	}
	if store.Get("api.x.ai") != newSec {
		t.Fatal("store not rotated")
	}
	if err := sess.AssertWorkerCannotSeeSecret(newSec); err != nil {
		t.Fatal(err)
	}
	if err := sess.AssertWorkerCannotSeeSecret(oldSec); err != nil {
		t.Fatal(err)
	}

	// Revoke
	if err := sess.Revoke("api.x.ai"); err != nil {
		t.Fatal(err)
	}
	if sess.Proxy.HostCredentialPresent("api.x.ai") {
		t.Fatal("expected revoked")
	}
	if store.Get("api.x.ai") != "" {
		t.Fatal("store still has secret after revoke")
	}

	// Re-seed store and restart — creds come from out-of-band store, not worker.
	_ = store.Set("api.x.ai", newSec)
	if err := sess.Restart(); err != nil {
		t.Fatal(err)
	}
	if sess.Proxy.Generation() <= gen1 {
		t.Fatalf("generation should bump: before=%d after=%d", gen1, sess.Proxy.Generation())
	}
	if !sess.Proxy.HostCredentialPresent("api.x.ai") {
		t.Fatal("restart should re-seed from store")
	}
	if err := sess.AssertWorkerCannotSeeSecret(newSec); err != nil {
		t.Fatal(err)
	}
}

func TestExactSessionCausalProof(t *testing.T) {
	store := NewMemorySecretStore()
	secret := "Bearer proof-secret-fac170-AAA"
	_ = store.Set("api.x.ai", secret)

	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "fake",
		Store:       store,
		Allowlist:   []string{"api.x.ai", "127.0.0.1"},
		Interactive: false,
		Worktree:    t.TempDir(),
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
	if !proof.PromptConsumed || !proof.AllowedMarkerReach || !proof.ForbiddenAccessDeny || !proof.WorkerSecretHidden {
		t.Fatalf("incomplete proof: %+v", proof)
	}
	if proof.SessionID != sess.ID {
		t.Fatal("session mismatch")
	}
	if proof.AllowedMarker != marker {
		t.Fatal(proof.AllowedMarker)
	}
	// Evidence must not contain secret.
	joined := strings.Join(proof.Evidence, " ")
	if strings.Contains(joined, secret) || strings.Contains(joined, "proof-secret") {
		t.Fatalf("evidence leaked secret: %v", proof.Evidence)
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
	// Packet redacted
	if strings.Contains(err.Error(), "sk-") {
		t.Fatal(err)
	}
}

func TestStartAuthorSessionNonInteractive_GrokWithCreds(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer live-test-key-not-real")
	sess, err := StartAuthorSessionNonInteractive("grok", t.TempDir(), store)
	if err != nil {
		// Platform block is acceptable only on unsupported GOOS.
		if be, ok := err.(*BlockedError); ok && be.Reason == BlockUnsupportedPlat {
			t.Skip(be.Detail)
		}
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.ConsumePrompt("non-interactive author turn"); err != nil {
		t.Fatal(err)
	}
	if !sess.PromptConsumed() {
		t.Fatal("prompt not consumed")
	}
	if err := sess.AssertWorkerCannotSeeSecret("live-test-key-not-real"); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorHostCredsFromEnv_HERD_HOST_CREDS(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("XAI_API_KEY")
	t.Setenv("HERD_HOST_CREDS", "api.x.ai=Bearer from-map;api.openai.com=Bearer openai-map")
	creds := CoordinatorHostCredsFromEnv()
	if creds["api.x.ai"] != "Bearer from-map" {
		t.Fatal(creds)
	}
	if creds["api.openai.com"] != "Bearer openai-map" {
		t.Fatal(creds)
	}
}

func containsHost(hosts []string, want string) bool {
	for _, h := range hosts {
		if strings.EqualFold(h, want) {
			return true
		}
	}
	return false
}
