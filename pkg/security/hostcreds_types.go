package security

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// FAC-170: Broker HostCreds for non-interactive sandboxed harness author sessions.
//
// Guardrails (root early):
//   - Upstream secrets live ONLY in the coordinator/broker process (out-of-band store).
//   - Workers NEVER receive model secrets, proxy bearer tokens, control tokens, or
//     same-UID-readable proxy configs that act as delegated credentials.
//   - The broker is a session-bound request ORACLE (signing authority): worker
//     submits host+method+path intent over a least-authority channel (Unix socket
//     and/or pre-opened FD). Broker attaches Authorization and forwards.
//   - Allowlists: host + method + path (+ action). Deny-by-default.
//   - Expiry + revocation are mandatory; expired/revoked sessions fail closed.
//   - Direct provider network from the worker is out of policy (broker is the only path).
//   - Dummy CLI bootstrap keys (for tools that refuse to start without an env key)
//     are public sentinels and MUST NEVER be accepted as upstream Authorization.
//   - No OpenCode/Ollama-local kinds. No interactive browser/login UI.
//   - Abuse: broker must refuse arbitrary provider requests and must not exfiltrate
//     auth via redirects, DNS rebinding, CONNECT tunnels, logs, or error bodies.

// ErrHostCredsBlocked is the typed fail-closed signal for missing, revoked,
// unbrokerable, or platform-unsupported HostCreds paths.
var ErrHostCredsBlocked = errors.New("FAC-170 BLOCKED: HostCreds")

// DummyNeverUpstream is the public CLI bootstrap sentinel.
// Harness CLIs that refuse to start without an API key env var may be given this
// value. It is NOT a secret. The oracle MUST strip it and NEVER send it upstream.
const DummyNeverUpstream = "herd-dummy-never-upstream-fac170"

// DummyNeverUpstreamAuth is the Authorization form of the sentinel.
const DummyNeverUpstreamAuth = "Bearer " + DummyNeverUpstream

// HostCredsBlockReason is a stable, non-secret reason code.
type HostCredsBlockReason string

const (
	BlockMissingCreds      HostCredsBlockReason = "missing_host_creds"
	BlockUnbrokerableKind  HostCredsBlockReason = "unbrokerable_kind"
	BlockHostDenied        HostCredsBlockReason = "host_not_allowlisted"
	BlockMethodDenied      HostCredsBlockReason = "method_not_allowlisted"
	BlockPathDenied        HostCredsBlockReason = "path_not_allowlisted"
	BlockActionDenied      HostCredsBlockReason = "action_not_allowlisted"
	BlockRevoked           HostCredsBlockReason = "credential_revoked"
	BlockExpired           HostCredsBlockReason = "session_expired"
	BlockUnsupportedPlat   HostCredsBlockReason = "platform_unsupported"
	BlockNoSession         HostCredsBlockReason = "session_missing"
	BlockInteractiveDenied HostCredsBlockReason = "interactive_login_forbidden"
	BlockSecretExposure    HostCredsBlockReason = "worker_secret_exposure_denied"
	BlockDummyUpstream     HostCredsBlockReason = "dummy_key_never_upstream"
	BlockWorkerAuthInject  HostCredsBlockReason = "worker_auth_injection_denied"
	BlockAbuse             HostCredsBlockReason = "oracle_abuse_denied"
	BlockDirectNetwork     HostCredsBlockReason = "direct_provider_network_denied"
)

// BlockedError is a typed BLOCKED outcome (fail-closed, non-secret).
type BlockedError struct {
	Reason        HostCredsBlockReason
	Kind          string
	SessionID     string
	HostsRequired []string
	HostsCreds    []string // hosts only — never values
	Detail        string   // redacted free-text
}

func (e *BlockedError) Error() string {
	if e == nil {
		return "FAC-170 BLOCKED"
	}
	return fmt.Sprintf(
		"FAC-170 BLOCKED: reason=%s kind=%s session=%s hosts_required=%v hosts_creds=%v %s",
		e.Reason, e.Kind, e.SessionID, e.HostsRequired, e.HostsCreds, RedactSecrets(e.Detail),
	)
}

func (e *BlockedError) Is(target error) bool {
	return target == ErrHostCredsBlocked
}

// KindAuthClass classifies whether a harness kind can complete a model turn
// under scrubbed worker env + HostCreds-oracle containment (no secrets).
type KindAuthClass string

const (
	KindAuthOK       KindAuthClass = "ok"
	KindAuthExternal KindAuthClass = "external_credential_state"
	KindAuthConfig   KindAuthClass = "harness_or_env_config"
	KindAuthPlatform KindAuthClass = "platform_unsupported"
)

// KindAuthDiagnosis is a non-secret evidence packet for coordinator routing.
// Never contains credential bytes.
type KindAuthDiagnosis struct {
	Kind              string        `json:"kind"`
	Class             KindAuthClass `json:"class"`
	Brokerable        bool          `json:"brokerable"`
	RequiredHosts     []string      `json:"required_hosts"`
	HostCredsPresent  []string      `json:"host_creds_present"` // hosts only
	HostAuthFileHint  string        `json:"host_auth_file_hint,omitempty"`
	HostAuthModeHint  string        `json:"host_auth_mode_hint,omitempty"`
	Reason            string        `json:"reason"`
	Blocker           string        `json:"blocker"`
	RecommendedAction string        `json:"recommended_action"`
}

// Supported hosted author kinds for HostCreds brokering (OpenCode/Ollama out of scope).
const (
	AuthorKindGrok   = "grok"
	AuthorKindClaude = "claude"
	AuthorKindCodex  = "codex"
)

// RequestRule is one allowlisted (host, method, path-prefix, action) tuple.
// Action is an opaque label for audit (e.g. "chat.completions").
type RequestRule struct {
	Host       string // exact host (lowercased)
	Method     string // exact HTTP method (uppercased); empty = any method on path
	PathPrefix string // required path prefix; must start with /
	Action     string // optional action label
}

// DefaultHostAllowlist is the production host set for hosted author APIs.
// Loopback is NOT included by default.
func DefaultHostAllowlist() []string {
	return []string{
		"api.openai.com",
		"api.anthropic.com",
		"api.x.ai",
	}
}

// DefaultRequestRules is the least-privilege method/path allowlist for hosted authors.
// Deny-by-default: anything not matching a rule is refused by the oracle.
func DefaultRequestRules() []RequestRule {
	return []RequestRule{
		{Host: "api.x.ai", Method: "POST", PathPrefix: "/v1/chat/completions", Action: "chat.completions"},
		{Host: "api.x.ai", Method: "POST", PathPrefix: "/v1/messages", Action: "messages"},
		{Host: "api.x.ai", Method: "GET", PathPrefix: "/v1/models", Action: "models.list"},
		{Host: "api.anthropic.com", Method: "POST", PathPrefix: "/v1/messages", Action: "messages"},
		{Host: "api.anthropic.com", Method: "GET", PathPrefix: "/v1/models", Action: "models.list"},
		{Host: "api.openai.com", Method: "POST", PathPrefix: "/v1/chat/completions", Action: "chat.completions"},
		{Host: "api.openai.com", Method: "POST", PathPrefix: "/v1/responses", Action: "responses"},
		{Host: "api.openai.com", Method: "GET", PathPrefix: "/v1/models", Action: "models.list"},
	}
}

// DefaultSessionTTL is the maximum lifetime of a HostCreds session.
const DefaultSessionTTL = 30 * time.Minute

// RequiredBrokerHostsForKind returns upstream hosts HostCreds must cover.
func RequiredBrokerHostsForKind(kind string) []string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case AuthorKindClaude:
		return []string{"api.anthropic.com"}
	case AuthorKindCodex:
		return []string{"api.openai.com"}
	case AuthorKindGrok:
		return []string{"api.x.ai"}
	default:
		return nil
	}
}

// IsSupportedAuthorKind reports whether kind is a HostCreds-brokered hosted author.
// OpenCode and Ollama-local kinds are explicitly out of scope.
func IsSupportedAuthorKind(kind string) bool {
	return len(RequiredBrokerHostsForKind(kind)) > 0
}

// IsDummyCredential reports whether v is the public CLI bootstrap sentinel
// (or Authorization form of it). Dummy material must never go upstream.
func IsDummyCredential(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if v == DummyNeverUpstream || v == DummyNeverUpstreamAuth {
		return true
	}
	// Bearer <dummy>
	if strings.HasPrefix(strings.ToLower(v), "bearer ") {
		tok := strings.TrimSpace(v[7:])
		return tok == DummyNeverUpstream
	}
	return false
}

// MatchRequestRule returns the matching rule or nil if denied.
func MatchRequestRule(rules []RequestRule, host, method, path string) *RequestRule {
	host = strings.ToLower(strings.TrimSpace(host))
	method = strings.ToUpper(strings.TrimSpace(method))
	if path == "" {
		path = "/"
	}
	// Reject path traversal / scheme smuggling in path.
	if strings.Contains(path, "..") || strings.Contains(path, "://") || strings.ContainsAny(path, "\r\n\x00") {
		return nil
	}
	// Strip query for prefix match; rejections still apply to full path smuggling above.
	pathOnly := path
	if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	for i := range rules {
		r := &rules[i]
		if strings.ToLower(r.Host) != host {
			continue
		}
		if r.Method != "" && strings.ToUpper(r.Method) != method {
			continue
		}
		prefix := r.PathPrefix
		if prefix == "" {
			prefix = "/"
		}
		// Exact or boundary-safe prefix: /v1/chat matches /v1/chat and /v1/chat/...
		// but not /v1/chatty.
		if pathOnly == prefix || strings.HasPrefix(pathOnly, prefix+"/") {
			return r
		}
	}
	return nil
}
