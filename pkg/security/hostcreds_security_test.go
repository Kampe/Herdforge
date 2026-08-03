package security

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSecurity_ProxyTokenCannotDumpSecrets(t *testing.T) {
	store := NewMemorySecretStore()
	secret := "Bearer sec-dump-test-xyz"
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

	// Attempt various "dump" paths on proxy listener — all must fail closed.
	paths := []string{
		"/__herd_control/ping",
		"/__herd_control/host_creds",
		"/secrets",
		"/creds",
	}
	for _, path := range paths {
		c, err := net.DialTimeout("tcp", sess.Proxy.Addr(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		basic := base64.StdEncoding.EncodeToString([]byte("herd:" + sess.Proxy.Token))
		_, _ = fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n",
			path, sess.Proxy.Addr(), basic)
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		_ = c.Close()
		resp := string(buf[:n])
		if strings.Contains(resp, secret) || strings.Contains(resp, "sec-dump-test") {
			t.Fatalf("secret leaked via proxy path %s: %s", path, resp)
		}
		// Control on proxy must be 403; other paths not 200 with secrets.
		if path == "/__herd_control/ping" && strings.Contains(resp, "200") && strings.Contains(resp, `"ok":true`) {
			t.Fatalf("control served on proxy listener: %s", resp)
		}
	}
}

func TestSecurity_ForbiddenHostDeniedSameSession(t *testing.T) {
	store := NewMemorySecretStore()
	_ = store.Set("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "grok",
		Store:       store,
		Allowlist:   DefaultHostAllowlist(),
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	sid := sess.ID
	if err := sess.AttemptForbiddenCredentialAccess("not-on-allowlist.example"); err != nil {
		t.Fatal(err)
	}
	if sess.ID != sid {
		t.Fatal("session changed")
	}
}

func TestSecurity_InjectedAuthNotVisibleToWorker(t *testing.T) {
	// Upstream echoes Authorization header so we can prove broker injects it
	// without the worker sending it — while worker env still has no secret.
	secret := "Bearer inject-only-secret-ZZZ"
	var sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))

	store := NewMemorySecretStore()
	_ = store.Set("127.0.0.1", secret)
	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        "fake",
		Store:       store,
		Allowlist:   []string{"127.0.0.1"},
		Interactive: false,
		Worktree:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Worker sends NO Authorization — broker injects.
	body, status, err := proxyAbsoluteGET(sess.Proxy, "http://127.0.0.1:"+port+"/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 || body != "ok" {
		t.Fatalf("status=%d body=%q", status, body)
	}
	if sawAuth != secret {
		t.Fatalf("broker did not inject auth: saw %q", sawAuth)
	}
	if err := sess.AssertWorkerCannotSeeSecret(secret); err != nil {
		t.Fatal(err)
	}
}

func TestSecurity_DefaultAllowlistNoLoopback(t *testing.T) {
	for _, h := range DefaultHostAllowlist() {
		if h == "127.0.0.1" || h == "localhost" {
			t.Fatalf("default allowlist must not include loopback: %s", h)
		}
	}
	// Must include api.x.ai for grok HostCreds.
	found := false
	for _, h := range DefaultHostAllowlist() {
		if h == "api.x.ai" {
			found = true
		}
	}
	if !found {
		t.Fatal("api.x.ai required for grok")
	}
}

func TestSecurity_BlockedErrorIsTyped(t *testing.T) {
	err := &BlockedError{Reason: BlockMissingCreds, Kind: "grok", Detail: "test"}
	if !errors.Is(err, ErrHostCredsBlocked) {
		t.Fatal("Is(ErrHostCredsBlocked) failed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "FAC-170 BLOCKED") {
		t.Fatal(msg)
	}
}
