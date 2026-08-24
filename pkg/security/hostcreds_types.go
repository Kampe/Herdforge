package security

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// FAC-170: Broker HostCreds for non-interactive sandboxed harness author sessions.
//
// Rejected designs (do not reintroduce):
//   - CoordinatorHostCredsFromEnv / HERD_HOST_CREDS as production authority
//     (same-UID workers can read parent env/proc; scrub is not a kernel boundary)
//   - Public Get/Snapshot returning Authorization material (exfiltration APIs)
//   - MemorySecretStore as production durability (tests-only vault only)
//   - Global multi-provider host allowlists (per-kind exact host+method+path+action)
//   - Free-form BlockedError.Detail (stable codes only)

// ErrHostCredsBlocked is the typed fail-closed signal.
var ErrHostCredsBlocked = errors.New("FAC-170 BLOCKED: HostCreds")

// DummyNeverUpstream is the public CLI bootstrap sentinel (NOT a secret).
// CLIs that refuse to start without an API key may receive this value.
// The oracle MUST strip it and NEVER send it upstream.
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
	BlockBadHost           HostCredsBlockReason = "host_invalid"
	BlockBadAuthMaterial   HostCredsBlockReason = "auth_material_invalid"
	BlockEnvNotAuthority   HostCredsBlockReason = "env_not_production_authority"
	BlockHandleUnresolved  HostCredsBlockReason = "secret_handle_unresolved"
	BlockNotDurable        HostCredsBlockReason = "authority_not_durable"
)

// BlockedError is fail-closed and non-secret. No free-form Detail field.
// Code is a stable machine subcode (e.g. "host:api.x.ai"); never raw secrets.
type BlockedError struct {
	Reason        HostCredsBlockReason `json:"reason"`
	Code          string               `json:"code,omitempty"`
	Kind          string               `json:"kind,omitempty"`
	SessionID     string               `json:"session_id,omitempty"`
	HostsRequired []string             `json:"hosts_required,omitempty"`
	HostsPresent  []string             `json:"hosts_present,omitempty"` // names only
}

func (e *BlockedError) Error() string {
	if e == nil {
		return "FAC-170 BLOCKED"
	}
	return fmt.Sprintf(
		"FAC-170 BLOCKED: reason=%s code=%s kind=%s session=%s hosts_required=%v hosts_present=%v",
		e.Reason, e.Code, e.Kind, e.SessionID, e.HostsRequired, e.HostsPresent,
	)
}

func (e *BlockedError) Is(target error) bool {
	return target == ErrHostCredsBlocked
}

// KindAuthClass classifies harness kind readiness (no secrets).
type KindAuthClass string

const (
	KindAuthOK       KindAuthClass = "ok"
	KindAuthExternal KindAuthClass = "external_credential_state"
	KindAuthConfig   KindAuthClass = "harness_or_env_config"
	KindAuthPlatform KindAuthClass = "platform_unsupported"
)

// KindAuthDiagnosis is a non-secret evidence packet.
type KindAuthDiagnosis struct {
	Kind              string        `json:"kind"`
	Class             KindAuthClass `json:"class"`
	Brokerable        bool          `json:"brokerable"`
	RequiredHosts     []string      `json:"required_hosts"`
	HostCredsPresent  []string      `json:"host_creds_present"` // names only
	AuthorityClass    string        `json:"authority_class"`    // keychain|op|test|none
	HostAuthFileHint  string        `json:"host_auth_file_hint,omitempty"`
	HostAuthModeHint  string        `json:"host_auth_mode_hint,omitempty"`
	ReasonCode        string        `json:"reason_code"`
	Blocker           string        `json:"blocker"` // stable template, no secrets
	RecommendedAction string        `json:"recommended_action"`
}

const (
	AuthorKindGrok   = "grok"
	AuthorKindClaude = "claude"
	AuthorKindCodex  = "codex"
	AuthorKindAGY    = "agy"
)

// RequestRule is exact allowlisted host + method + path + action (deny-by-default).
// Path is exact (not a global prefix family): optional trailing path segments
// only when PathExact is false and PathPrefix is set with boundary rules.
type RequestRule struct {
	Host       string // exact normalized host
	Method     string // exact HTTP method
	PathExact  string // if non-empty, path must equal this (query stripped)
	PathPrefix string // if PathExact empty: boundary-safe prefix
	Action     string // required action label when non-empty on request
}

// DefaultSessionTTL is the maximum lifetime of a HostCreds session.
const DefaultSessionTTL = 30 * time.Minute

// RequiredBrokerHostsForKind returns the exact host(s) for one kind — not a global list.
//
// This answers "which host would a brokered key be FOR", which is a different
// question from "does this kind need one". Harness-authenticated kinds are
// exempted by DiagnoseKindAuthReadiness via harnessAuthenticated, not by
// blanking this map: the broker's own host machinery still needs a real
// mapping, and emptying it reported every kind as unbrokerable_kind.
func RequiredBrokerHostsForKind(kind string) []string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case AuthorKindClaude:
		// FAC-576: none. Claude is used through its HARNESS on this fleet and
		// never through an API key, so there is no host credential to broker.
		// Returning api.anthropic.com made worker admission demand a
		// handle-backed credential for a host the harness never contacts with a
		// user key, so a fully logged-in claude reported
		// brokerable=false / hosts_creds=[] / authority=none and no reviewer
		// could launch. That is a category error, not a missing credential.
		return nil
	case AuthorKindCodex:
		return []string{"api.openai.com"}
	case AuthorKindGrok:
		return []string{"api.x.ai"}
	default:
		return nil
	}
}

// RequestRulesForKind returns exact method/path/action rules for one kind only.
func RequestRulesForKind(kind string) []RequestRule {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case AuthorKindGrok:
		return []RequestRule{
			{Host: "api.x.ai", Method: "POST", PathExact: "/v1/chat/completions", Action: "chat.completions"},
			{Host: "api.x.ai", Method: "GET", PathExact: "/v1/models", Action: "models.list"},
		}
	case AuthorKindClaude:
		return []RequestRule{
			{Host: "api.anthropic.com", Method: "POST", PathExact: "/v1/messages", Action: "messages"},
			{Host: "api.anthropic.com", Method: "GET", PathExact: "/v1/models", Action: "models.list"},
		}
	case AuthorKindCodex:
		return []RequestRule{
			{Host: "api.openai.com", Method: "POST", PathExact: "/v1/chat/completions", Action: "chat.completions"},
			{Host: "api.openai.com", Method: "POST", PathExact: "/v1/responses", Action: "responses"},
			{Host: "api.openai.com", Method: "GET", PathExact: "/v1/models", Action: "models.list"},
		}
	case "fake", "test":
		// Deterministic proof only — loopback exact path.
		return []RequestRule{
			{Host: "127.0.0.1", Method: "POST", PathExact: "/v1/chat/completions", Action: "chat.completions"},
			{Host: "127.0.0.1", Method: "GET", PathExact: "/v1/models", Action: "models.list"},
		}
	default:
		return nil
	}
}

// DirectProviderHostsForKind is the worker direct-network deny set for one kind.
func DirectProviderHostsForKind(kind string) []string {
	return RequiredBrokerHostsForKind(kind)
}

// IsSupportedAuthorKind reports hosted author kinds (OpenCode out of scope).
func IsSupportedAuthorKind(kind string) bool {
	return len(RequiredBrokerHostsForKind(kind)) > 0
}

// IsDummyCredential reports public CLI bootstrap sentinel material.
func IsDummyCredential(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if v == DummyNeverUpstream || v == DummyNeverUpstreamAuth {
		return true
	}
	if strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[7:]) == DummyNeverUpstream
	}
	return false
}

// MatchRequestRule returns the matching rule or nil (deny).
func MatchRequestRule(rules []RequestRule, host, method, path string) *RequestRule {
	host = strings.ToLower(strings.TrimSpace(host))
	method = strings.ToUpper(strings.TrimSpace(method))
	if path == "" {
		path = "/"
	}
	if strings.Contains(path, "..") || strings.Contains(path, "://") || strings.ContainsAny(path, "\r\n\x00") {
		return nil
	}
	pathOnly := path
	if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	for i := range rules {
		r := &rules[i]
		if strings.ToLower(r.Host) != host {
			continue
		}
		if strings.ToUpper(r.Method) != method {
			continue
		}
		if r.PathExact != "" {
			if pathOnly == r.PathExact {
				return r
			}
			continue
		}
		prefix := r.PathPrefix
		if prefix == "" {
			continue
		}
		if pathOnly == prefix || strings.HasPrefix(pathOnly, prefix+"/") {
			return r
		}
	}
	return nil
}
