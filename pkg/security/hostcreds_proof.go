package security

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// SessionProof is the exact-session live causal proof result (FAC-170).
// All fields bind to one SessionID — no cross-session aggregation.
type SessionProof struct {
	SessionID           string
	Kind                string
	PromptConsumed      bool
	AllowedMarkerReach  bool
	AllowedMarker       string
	ForbiddenAccessDeny bool
	WorkerSecretHidden  bool
	Generation          int
	Evidence            []string
}

// ProveExactSessionHostCreds runs the FAC-170 causal proof against a live
// HostCredsSession + local allowlisted upstream that requires Authorization.
//
// Proves in ONE session:
//  1. non-interactive prompt consumed
//  2. allowlisted host request succeeds with broker-injected Authorization
//     (allowed marker reachable)
//  3. forbidden credential-access attempt DENIED
//  4. credential bytes never appear in worker env/fs/proxy URL
func ProveExactSessionHostCreds(sess *HostCredsSession, secret, allowedMarker string) (*SessionProof, error) {
	if sess == nil || sess.Proxy == nil {
		return nil, &BlockedError{Reason: BlockNoSession, Detail: "nil session for proof"}
	}
	if secret == "" {
		return nil, fmt.Errorf("proof requires non-empty secret")
	}
	if allowedMarker == "" {
		allowedMarker = "HOSTCREDS_ALLOWED_OK"
	}

	proof := &SessionProof{
		SessionID:  sess.ID,
		Kind:       sess.Kind,
		Generation: sess.Proxy.Generation(),
	}

	// 1) Consume non-interactive prompt.
	prompt := fmt.Sprintf("FAC-170 non-interactive proof prompt session=%s marker=%s", sess.ID, allowedMarker)
	if err := sess.ConsumePrompt(prompt); err != nil {
		return proof, err
	}
	proof.PromptConsumed = sess.PromptConsumed()
	proof.Evidence = append(proof.Evidence, "prompt_consumed="+sess.LastPrompt())

	// 2) Local allowlisted upstream that requires the exact Authorization secret.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, allowedMarker)
	}))
	defer upstream.Close()

	// Seed loopback into allowlist for this proof (production defaults exclude it).
	upURL := upstream.URL // http://127.0.0.1:PORT
	hostPort := strings.TrimPrefix(upURL, "http://")
	host, port, _ := net.SplitHostPort(hostPort)
	if host == "" {
		host = "127.0.0.1"
	}

	// Ensure 127.0.0.1 is allowlisted for this session's proxy.
	found := false
	for _, h := range sess.Proxy.AllowHosts {
		if h == "127.0.0.1" || h == "localhost" {
			found = true
			break
		}
	}
	if !found {
		sess.Proxy.AllowHosts = append(sess.Proxy.AllowHosts, "127.0.0.1")
	}
	if err := sess.Proxy.SetHostCredential("127.0.0.1", secret); err != nil {
		return proof, fmt.Errorf("seed loopback HostCreds: %w", err)
	}

	// Request through the broker proxy WITHOUT sending Authorization from "worker".
	body, status, err := proxyAbsoluteGET(sess.Proxy, fmt.Sprintf("http://127.0.0.1:%s/marker", port), "")
	if err != nil {
		return proof, fmt.Errorf("allowed path request: %w", err)
	}
	if status != http.StatusOK {
		return proof, fmt.Errorf("allowed path status %d body=%q", status, body)
	}
	if !strings.Contains(body, allowedMarker) {
		return proof, fmt.Errorf("allowed marker not reachable: body=%q", body)
	}
	proof.AllowedMarkerReach = true
	proof.AllowedMarker = allowedMarker
	proof.Evidence = append(proof.Evidence, "allowed_marker="+allowedMarker, "upstream_host="+host)

	// 3) Forbidden credential access must be denied.
	if err := sess.AttemptForbiddenCredentialAccess("evil.example.invalid"); err != nil {
		return proof, err
	}
	proof.ForbiddenAccessDeny = true
	proof.Evidence = append(proof.Evidence, "forbidden_access=DENIED")

	// 4) Worker cannot see secret.
	if err := sess.AssertWorkerCannotSeeSecret(secret); err != nil {
		return proof, err
	}
	// Also ensure proof evidence strings don't embed the secret.
	for _, e := range proof.Evidence {
		if strings.Contains(e, secret) {
			return proof, &BlockedError{
				Reason:    BlockSecretExposure,
				SessionID: sess.ID,
				Detail:    "proof evidence leaked secret",
			}
		}
	}
	proof.WorkerSecretHidden = true
	proof.Evidence = append(proof.Evidence, "worker_secret_hidden=true")

	// Bind generation still matches same session.
	if sess.Proxy.Generation() != proof.Generation {
		return proof, fmt.Errorf("generation changed mid-proof (not same session incarnation)")
	}
	if sess.ID != proof.SessionID {
		return proof, fmt.Errorf("session id mismatch")
	}

	return proof, nil
}

// proxyAbsoluteGET performs an absolute-form GET through the HostCreds proxy
// with the agent proxy token. authorization is optional worker-supplied auth
// (empty means broker must inject).
func proxyAbsoluteGET(p *HostCredsProxy, absoluteURL, authorization string) (body string, status int, err error) {
	c, err := net.DialTimeout("tcp", p.Addr(), 2*time.Second)
	if err != nil {
		return "", 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	basic := base64.StdEncoding.EncodeToString([]byte("herd:" + p.Token))
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n",
		absoluteURL, hostOf(absoluteURL), basic)
	if authorization != "" {
		req += "Authorization: " + authorization + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return "", 0, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, nil
}

func hostOf(rawURL string) string {
	// crude host extract for absolute URL
	u := strings.TrimPrefix(rawURL, "http://")
	u = strings.TrimPrefix(u, "https://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

func proveProxyTokenCannotReadControl(p *HostCredsProxy) error {
	c, err := net.DialTimeout("tcp", p.ControlAddr(), 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	// Use PROXY token (agent-visible) against control — must be rejected.
	basic := base64.StdEncoding.EncodeToString([]byte("herd:" + p.Token))
	_, _ = fmt.Fprintf(c, "GET /__herd_control/ping HTTP/1.1\r\nHost: %s\r\nAuthorization: Basic %s\r\nConnection: close\r\n\r\n",
		p.ControlAddr(), basic)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	// Expect 407 or 401/403 — not 200.
	if strings.Contains(line, "200") {
		return fmt.Errorf("proxy token accepted on control (want deny): %q", strings.TrimSpace(line))
	}
	return nil
}

func proveConnectDenied(p *HostCredsProxy, hostport string) error {
	c, err := net.DialTimeout("tcp", p.Addr(), 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	basic := base64.StdEncoding.EncodeToString([]byte("herd:" + p.Token))
	_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n",
		hostport, hostport, basic)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, "403") {
		return fmt.Errorf("want 403 got %q", strings.TrimSpace(line))
	}
	return nil
}

// StartAuthorSessionNonInteractive is the production entry for hosted authors.
// Returns typed BLOCKED when HostCreds are missing/unbrokerable — never
// falls back to interactive login UI.
//
// When store is non-nil it is the out-of-band authority (preferred). When nil,
// coordinator env is loaded into a temporary MemorySecretStore.
func StartAuthorSessionNonInteractive(kind, worktree string, store SecretStore) (*HostCredsSession, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if store == nil {
		store = NewMemorySecretStore()
		_ = LoadEnvIntoStore(store)
	}

	// Platform fail-closed.
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}

	// Prefer store-backed diagnosis when an explicit store is provided so
	// coordinator out-of-band secrets (not only process env) are honored.
	required := RequiredBrokerHostsForKind(kind)
	if len(required) == 0 {
		d := DiagnoseKindAuthReadiness(kind)
		return nil, &BlockedError{
			Reason:        BlockUnbrokerableKind,
			Kind:          kind,
			HostsRequired: d.RequiredHosts,
			HostsCreds:    HostsPresent(store.Snapshot()),
			Detail:        RedactSecrets(d.Blocker),
		}
	}
	missing := []string{}
	for _, h := range required {
		if strings.TrimSpace(store.Get(h)) == "" {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		// Fall back to env diagnosis packet shape for coordinator routing.
		d := DiagnoseKindAuthReadiness(kind)
		hostsCreds := HostsPresent(store.Snapshot())
		if len(hostsCreds) == 0 {
			hostsCreds = d.HostCredsPresent
		}
		return nil, &BlockedError{
			Reason:        BlockMissingCreds,
			Kind:          kind,
			HostsRequired: required,
			HostsCreds:    hostsCreds,
			Detail:        RedactSecrets(d.Blocker),
		}
	}

	return StartHostCredsSession(SessionConfig{
		Kind:        kind,
		Store:       store,
		Worktree:    worktree,
		Interactive: false,
	})
}
