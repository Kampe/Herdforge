package security

import (
	"strings"
	"testing"
)

// Mutation-style tests: removing the boundary/allowlist/redaction must fail.
// These assert the security properties that a vacuous implementation would skip.

func TestMutation_AllowlistBoundaryIsLoadBearing(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer mut-secret")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Allowlist:   []string{"api.x.ai"}, // only x.ai
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// If allowlist were removed, HostAllowed would return true for anything.
	if sess.Proxy.HostAllowed("api.openai.com") {
		t.Fatal("MUTATION DETECTED: non-allowlisted host accepted (allowlist boundary broken)")
	}
	if sess.Proxy.HostAllowed("127.0.0.1") {
		t.Fatal("MUTATION DETECTED: loopback accepted without explicit allowlist entry")
	}
	// CONNECT to denied host must be 403 — if dialAllowed ignored allowlist, this fails.
	if err := proveConnectDenied(sess.Proxy, "api.openai.com:443"); err != nil {
		t.Fatalf("MUTATION DETECTED: deny path not enforced: %v", err)
	}
}

func TestMutation_RedactionIsLoadBearing(t *testing.T) {
	t.Setenv("XAI_API_KEY", "sk-mutation-leaked-value-999")
	d := DiagnoseKindAuthReadiness("grok")
	pkt := FormatKindAuthBlocker(d)
	if strings.Contains(pkt, "sk-mutation") || strings.Contains(pkt, "leaked-value") {
		t.Fatalf("MUTATION DETECTED: diagnosis packet leaked secret: %s", pkt)
	}
	// RedactSecrets must actually strip
	raw := "Bearer sk-mutation-leaked-value-999"
	if RedactSecrets(raw) == raw {
		t.Fatal("MUTATION DETECTED: RedactSecrets is a no-op")
	}
}

func TestMutation_WorkerEnvScrubIsLoadBearing(t *testing.T) {
	store := NewMemorySecretStore()
	secret := "Bearer mutation-worker-env-secret"
	_ = store.Set("api.x.ai", secret)
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

	env := sess.WorkerEnvMap()
	// If scrub were removed, these would carry coordinator secrets.
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			t.Fatalf("MUTATION DETECTED: worker env %s=%q not scrubbed", k, v)
		}
	}
	for _, v := range env {
		if strings.Contains(v, "mutation-worker-env-secret") {
			t.Fatal("MUTATION DETECTED: secret present in worker env value")
		}
	}
}

func TestMutation_MissingCredsNoSilentFallback(t *testing.T) {
	// A broken implementation might start a session with empty HostCreds
	// and rely on interactive login. That must fail closed.
	store := NewMemorySecretStore()
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "claude",
		Store:       store,
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err == nil {
		_ = sess.Close()
		t.Fatal("MUTATION DETECTED: session started without HostCreds (silent fallback)")
	}
	be, ok := err.(*BlockedError)
	if !ok {
		t.Fatalf("want typed BlockedError, got %T %v", err, err)
	}
	if be.Reason != BlockMissingCreds {
		t.Fatalf("want missing_host_creds, got %s", be.Reason)
	}
}

func TestMutation_ControlTokenNotInWorkerEnv(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer x")
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

	ctrl := sess.Proxy.ControlToken
	if ctrl == "" {
		t.Fatal("expected control token on broker")
	}
	for k, v := range sess.WorkerEnvMap() {
		if strings.Contains(v, ctrl) {
			t.Fatalf("MUTATION DETECTED: control token in worker env %s", k)
		}
	}
	// Proxy token may appear in HTTP_PROXY userinfo — that is intentional and
	// is NOT a model credential. Control token must never appear in ProxyURL.
	if strings.Contains(sess.Proxy.ProxyURL(), ctrl) {
		t.Fatal("MUTATION DETECTED: control token embedded in proxy URL")
	}
}

func TestMutation_RevokeStopsInjection(t *testing.T) {
	store := NewMemorySecretStore()
	secret := "Bearer revoke-mut-secret"
	_ = store.Set("127.0.0.1", secret)
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "fake",
		Store:       store,
		Allowlist:   []string{"127.0.0.1", "api.x.ai"},
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if !sess.Proxy.HostCredentialPresent("127.0.0.1") {
		t.Fatal("expected present")
	}
	if err := sess.Revoke("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// After revoke, hostCred must be empty — injection cannot proceed.
	if sess.Proxy.hostCred("127.0.0.1") != "" {
		t.Fatal("MUTATION DETECTED: revoked credential still injectable")
	}
}
