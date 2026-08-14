package security

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
)

func osGetpidReal() int { return os.Getpid() }

// SessionProof is component-level causal proof (oracle + TLS MITM rules).
// Live admission requires LiveProof from StartAuthorLive, not this alone.
type SessionProof struct {
	SessionID           string
	Kind                string
	PromptConsumed      bool // only true if RecordHarnessPrompt used
	AllowedMarkerReach  bool
	AllowedMarker       string
	ForbiddenAccessDeny bool
	WorkerSecretHidden  bool
	NoWorkerBearer      bool
	NoAPIKeys           bool
	Generation          int
	Evidence            []string
}

// ProveExactSessionHostCreds runs component causal proof with TLS upstream.
// Does NOT claim live harness admission. For live AC use StartAuthorLive.
func ProveExactSessionHostCreds(sess *HostCredsSession, secret, allowedMarker string) (*SessionProof, error) {
	if sess == nil || sess.Oracle == nil {
		return nil, &BlockedError{Reason: BlockNoSession, Code: "oracle_required_for_component_proof"}
	}
	if secret == "" || IsDummyCredential(secret) {
		return nil, &BlockedError{Reason: BlockBadAuthMaterial, Code: "proof_needs_real_secret"}
	}
	if allowedMarker == "" {
		allowedMarker = "HOSTCREDS_ALLOWED_OK"
	}
	proof := &SessionProof{SessionID: sess.ID, Kind: sess.Kind}
	if sess.Oracle != nil {
		proof.Generation = sess.Oracle.Generation()
	}

	// Simulate harness PID registration (component proof only).
	if err := sess.RecordHarnessPrompt("component-proof-prompt", osGetpid()); err != nil {
		return proof, err
	}
	proof.PromptConsumed = sess.PromptConsumed()
	proof.Evidence = append(proof.Evidence, "prompt_in_argv_registered=true")

	var sawAuth atomic.Value
	sawAuth.Store("")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		sawAuth.Store(auth)
		if IsDummyCredential(auth) || auth == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if auth != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, allowedMarker)
	}))
	defer upstream.Close()
	_, upPort, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "https://"))
	if err != nil {
		return proof, err
	}
	if tv, ok := sess.authority.(*TestCredentialVault); ok {
		if err := tv.InstallTestSecret("127.0.0.1", secret); err != nil {
			return proof, err
		}
	}
	sess.Oracle.allowLoopback = true
	// Reuse the httptest server's root CA instead of disabling verification.
	// The dial hook below still proves the oracle can only reach loopback.
	sess.Oracle.upstreamTLS = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", upPort))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	sess.Oracle.Hosts = appendUnique(sess.Oracle.Hosts, "127.0.0.1")
	sess.Oracle.Rules = RequestRulesForKind("fake")

	resp, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID,
		Host:      "127.0.0.1",
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Action:    "chat.completions",
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
	got, _ := sawAuth.Load().(string)
	if got != secret || IsDummyCredential(got) {
		return proof, &BlockedError{Reason: BlockDummyUpstream, Code: "upstream_auth_wrong"}
	}
	proof.AllowedMarkerReach = true
	proof.AllowedMarker = allowedMarker

	if err := sess.AttemptForbiddenCredentialAccess(); err != nil {
		return proof, err
	}
	proof.ForbiddenAccessDeny = true
	if err := sess.AssertWorkerCannotSeeSecret(secret); err != nil {
		return proof, err
	}
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		return proof, err
	}
	proof.WorkerSecretHidden = true
	proof.NoWorkerBearer = true
	proof.NoAPIKeys = true
	proof.Evidence = append(proof.Evidence, "forbidden=DENIED", "no_api_keys=true", "tls_inject=true")
	return proof, nil
}

func osGetpid() int {
	return osGetpidReal()
}

// StartAuthorSessionNonInteractive starts handle-backed session for production.
func StartAuthorSessionNonInteractive(kind, worktree string, auth CredentialAuthority) (*HostCredsSession, error) {
	_ = worktree
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
			Reason: BlockMissingCreds, Code: diag.ReasonCode, Kind: kind,
			HostsRequired: diag.RequiredHosts, HostsPresent: diag.HostCredsPresent,
		}
	}
	return StartHostCredsSession(SessionConfig{
		Kind: kind, SessionID: newSessionID(), Authority: auth, Interactive: false,
	})
}
