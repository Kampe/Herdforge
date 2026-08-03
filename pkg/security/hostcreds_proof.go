package security

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	NoWorkerBearer      bool
	DummyNeverUpstream  bool
	Generation          int
	Evidence            []string
}

// ProveExactSessionHostCreds runs the FAC-170 causal proof against a live
// HostCredsSession oracle + local allowlisted upstream that requires Authorization.
//
// Proves in ONE session:
//  1. non-interactive prompt consumed
//  2. allowlisted host+method+path succeeds with broker-attached Authorization
//     (worker sent only dummy sentinel; allowed marker reachable)
//  3. forbidden credential-access attempts DENIED (host/path/auth inject)
//  4. credential bytes never appear in worker env; no proxy bearer
//  5. dummy key never accepted as upstream Authorization
func ProveExactSessionHostCreds(sess *HostCredsSession, secret, allowedMarker string) (*SessionProof, error) {
	if sess == nil || sess.Oracle == nil {
		return nil, &BlockedError{Reason: BlockNoSession, Detail: "nil session for proof"}
	}
	if secret == "" || IsDummyCredential(secret) {
		return nil, fmt.Errorf("proof requires non-empty real secret (not dummy)")
	}
	if allowedMarker == "" {
		allowedMarker = "HOSTCREDS_ALLOWED_OK"
	}

	proof := &SessionProof{
		SessionID:  sess.ID,
		Kind:       sess.Kind,
		Generation: sess.Oracle.Generation(),
	}

	// 1) Consume non-interactive prompt.
	prompt := fmt.Sprintf("FAC-170 non-interactive proof prompt session=%s marker=%s", sess.ID, allowedMarker)
	if err := sess.ConsumePrompt(prompt); err != nil {
		return proof, err
	}
	proof.PromptConsumed = sess.PromptConsumed()
	proof.Evidence = append(proof.Evidence, "prompt_consumed=true")

	// 2) Local upstream that requires the exact real Authorization secret.
	var sawAuth atomic.Value
	sawAuth.Store("")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		sawAuth.Store(auth)
		if IsDummyCredential(auth) {
			http.Error(w, "dummy never upstream", http.StatusUnauthorized)
			return
		}
		if auth != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, allowedMarker)
	}))
	defer upstream.Close()

	_, upPort, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		return proof, err
	}
	sess.Oracle.forceHTTP = true
	sess.Oracle.CaptureUpstreamAuth = true
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", upPort))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		if host == "127.0.0.1" {
			return net.ParseIP("127.0.0.1"), nil
		}
		return resolveAndPinIP(host)
	}

	// Allow 127.0.0.1 only for this proof's rules.
	sess.Oracle.mu.Lock()
	sess.Oracle.Hosts = appendUnique(sess.Oracle.Hosts, "127.0.0.1")
	sess.Oracle.Rules = append(sess.Oracle.Rules, RequestRule{
		Host: "127.0.0.1", Method: "POST", PathPrefix: "/v1/chat/completions", Action: "chat.completions",
	})
	sess.Oracle.mu.Unlock()
	if err := sess.Store.Set("127.0.0.1", secret); err != nil {
		return proof, err
	}

	// Worker request: dummy Authorization only — oracle must replace with real secret.
	resp, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID,
		Host:      "127.0.0.1",
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Action:    "chat.completions",
		Headers:   map[string]string{"Authorization": DummyNeverUpstreamAuth},
		Body:      `{"model":"test"}`,
	})
	if err != nil {
		return proof, fmt.Errorf("allowed path request: %w", err)
	}
	if !resp.OK || resp.StatusCode != http.StatusOK {
		return proof, fmt.Errorf("allowed path failed: ok=%v status=%d err=%s", resp.OK, resp.StatusCode, resp.Error)
	}
	if !strings.Contains(resp.Body, allowedMarker) {
		return proof, fmt.Errorf("allowed marker not reachable: body=%q", resp.Body)
	}
	gotAuth, _ := sawAuth.Load().(string)
	if gotAuth != secret {
		return proof, fmt.Errorf("upstream saw wrong auth (dummy leak or missing inject): %q", RedactSecrets(gotAuth))
	}
	if IsDummyCredential(gotAuth) {
		return proof, &BlockedError{Reason: BlockDummyUpstream, Detail: "dummy reached upstream"}
	}
	proof.AllowedMarkerReach = true
	proof.AllowedMarker = allowedMarker
	proof.DummyNeverUpstream = true
	proof.Evidence = append(proof.Evidence, "allowed_marker=true", "dummy_never_upstream=true")

	// 3) Forbidden access denied.
	if err := sess.AttemptForbiddenCredentialAccess(); err != nil {
		return proof, err
	}
	proof.ForbiddenAccessDeny = true
	proof.Evidence = append(proof.Evidence, "forbidden_access=DENIED")

	// 4) Worker isolation.
	if err := sess.AssertWorkerCannotSeeSecret(secret); err != nil {
		return proof, err
	}
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		return proof, err
	}
	proof.WorkerSecretHidden = true
	proof.NoWorkerBearer = true
	proof.Evidence = append(proof.Evidence, "worker_secret_hidden=true", "no_worker_bearer=true")

	for _, e := range proof.Evidence {
		if strings.Contains(e, secret) {
			return proof, &BlockedError{Reason: BlockSecretExposure, SessionID: sess.ID, Detail: "proof evidence leaked secret"}
		}
	}
	if sess.Oracle.Generation() != proof.Generation {
		return proof, fmt.Errorf("generation changed mid-proof")
	}
	if sess.ID != proof.SessionID {
		return proof, fmt.Errorf("session id mismatch")
	}
	return proof, nil
}

// StartAuthorSessionNonInteractive is the production entry for hosted authors.
// Returns typed BLOCKED when HostCreds are missing/unbrokerable — never
// falls back to interactive login UI or worker-visible credential delegation.
func StartAuthorSessionNonInteractive(kind, worktree string, store SecretStore) (*HostCredsSession, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if store == nil {
		store = NewMemorySecretStore()
		_ = LoadEnvIntoStore(store)
	}
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}
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
		v := strings.TrimSpace(store.Get(h))
		if v == "" || IsDummyCredential(v) {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
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
