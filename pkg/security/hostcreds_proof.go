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

// SessionProof is the exact-session causal proof result (FAC-170).
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
	NoSecretExportAPI   bool
	Generation          int
	Evidence            []string
}

// ProveExactSessionHostCreds runs the causal proof against a live session + fake upstream.
func ProveExactSessionHostCreds(sess *HostCredsSession, secret, allowedMarker string) (*SessionProof, error) {
	if sess == nil || sess.Oracle == nil {
		return nil, &BlockedError{Reason: BlockNoSession, Code: "nil_session"}
	}
	if secret == "" || IsDummyCredential(secret) {
		return nil, &BlockedError{Reason: BlockBadAuthMaterial, Code: "proof_needs_real_secret"}
	}
	if allowedMarker == "" {
		allowedMarker = "HOSTCREDS_ALLOWED_OK"
	}

	proof := &SessionProof{
		SessionID:  sess.ID,
		Kind:       sess.Kind,
		Generation: sess.Oracle.Generation(),
	}

	if err := AssertNoPublicSecretExport(sess.Auth); err != nil {
		return proof, err
	}
	proof.NoSecretExportAPI = true
	proof.Evidence = append(proof.Evidence, "no_get_snapshot_api=true")

	if err := sess.ConsumePrompt(fmt.Sprintf("FAC-170 proof session=%s", sess.ID)); err != nil {
		return proof, err
	}
	proof.PromptConsumed = sess.PromptConsumed()
	proof.Evidence = append(proof.Evidence, "prompt_consumed=true")

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
	sess.Oracle.allowLoopback = true
	sess.Oracle.CaptureInjected = true
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", upPort))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}

	// Ensure vault has loopback secret via tests-only installer.
	if tv, ok := sess.Auth.(*TestCredentialVault); ok {
		if err := tv.InstallTestSecret("127.0.0.1", secret); err != nil {
			return proof, err
		}
	} else {
		return proof, &BlockedError{Reason: BlockAbuse, Code: "proof_requires_test_vault"}
	}
	// Rules for fake kind already include 127.0.0.1.
	sess.Oracle.mu.Lock()
	sess.Oracle.Hosts = appendUnique(sess.Oracle.Hosts, "127.0.0.1")
	sess.Oracle.Rules = RequestRulesForKind("fake")
	sess.Oracle.mu.Unlock()

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
		return proof, err
	}
	if !resp.OK || resp.StatusCode != http.StatusOK {
		return proof, &BlockedError{Reason: BlockAbuse, Code: "allowed_path_failed"}
	}
	if !strings.Contains(resp.Body, allowedMarker) {
		return proof, &BlockedError{Reason: BlockAbuse, Code: "marker_missing"}
	}
	gotAuth, _ := sawAuth.Load().(string)
	if gotAuth != secret || IsDummyCredential(gotAuth) {
		return proof, &BlockedError{Reason: BlockDummyUpstream, Code: "upstream_auth_wrong"}
	}
	proof.AllowedMarkerReach = true
	proof.AllowedMarker = allowedMarker
	proof.DummyNeverUpstream = true
	proof.Evidence = append(proof.Evidence, "allowed_marker=true", "dummy_never_upstream=true")

	if err := sess.AttemptForbiddenCredentialAccess(); err != nil {
		return proof, err
	}
	proof.ForbiddenAccessDeny = true
	proof.Evidence = append(proof.Evidence, "forbidden_access=DENIED")

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
			return proof, &BlockedError{Reason: BlockSecretExposure, SessionID: sess.ID, Code: "evidence_leak"}
		}
	}
	if sess.Oracle.Generation() != proof.Generation {
		return proof, &BlockedError{Reason: BlockAbuse, Code: "generation_drift"}
	}
	return proof, nil
}

// StartAuthorSessionNonInteractive starts a production session from handle authority.
func StartAuthorSessionNonInteractive(kind, worktree string, auth CredentialAuthority) (*HostCredsSession, error) {
	_ = worktree // worktree never holds secrets
	kind = strings.ToLower(strings.TrimSpace(kind))
	if auth == nil {
		var err error
		auth, err = NewHandleAuthorityFromEnv()
		if err != nil {
			return nil, err
		}
	}
	diag := DiagnoseKindAuthReadinessWith(kind, auth)
	if !diag.Brokerable {
		return nil, &BlockedError{
			Reason:        BlockMissingCreds,
			Code:          diag.ReasonCode,
			Kind:          kind,
			HostsRequired: diag.RequiredHosts,
			HostsPresent:  diag.HostCredsPresent,
		}
	}
	return StartHostCredsSession(SessionConfig{
		Kind:        kind,
		Authority:   auth,
		Interactive: false,
	})
}
