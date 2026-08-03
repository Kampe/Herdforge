package security

import (
	"errors"
	"fmt"
	"strings"
)

// FAC-170: Broker HostCreds for non-interactive sandboxed harness author sessions.
//
// Design invariants:
//   - Secrets live only in the coordinator/broker process (out-of-band).
//   - Workers receive proxy routing + public CA only — never Authorization bytes.
//   - Host allowlist is deny-by-default; non-allowlisted hosts get 403.
//   - Missing/unbrokerable credentials yield typed BLOCKED (no login UI fallback).
//   - Unsupported platforms fail closed with an explicit BLOCKED reason.

// ErrHostCredsBlocked is the typed fail-closed signal for missing, revoked,
// unbrokerable, or platform-unsupported HostCreds paths.
var ErrHostCredsBlocked = errors.New("FAC-170 BLOCKED: HostCreds")

// HostCredsBlockReason is a stable, non-secret reason code.
type HostCredsBlockReason string

const (
	BlockMissingCreds      HostCredsBlockReason = "missing_host_creds"
	BlockUnbrokerableKind  HostCredsBlockReason = "unbrokerable_kind"
	BlockHostDenied        HostCredsBlockReason = "host_not_allowlisted"
	BlockRevoked           HostCredsBlockReason = "credential_revoked"
	BlockUnsupportedPlat   HostCredsBlockReason = "platform_unsupported"
	BlockNoSession         HostCredsBlockReason = "session_missing"
	BlockInteractiveDenied HostCredsBlockReason = "interactive_login_forbidden"
	BlockSecretExposure    HostCredsBlockReason = "worker_secret_exposure_denied"
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
		e.Reason, e.Kind, e.SessionID, e.HostsRequired, e.HostsCreds, e.Detail,
	)
}

func (e *BlockedError) Is(target error) bool {
	return target == ErrHostCredsBlocked
}

// KindAuthClass classifies whether a harness kind can complete a model turn
// under scrubbed worker env + HostCreds-broker containment (no secrets).
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

// DefaultHostAllowlist is the production CONNECT allowlist for hosted author APIs.
// Loopback is NOT included by default (prevents arbitrary localhost access).
func DefaultHostAllowlist() []string {
	return []string{
		"api.openai.com",
		"api.anthropic.com",
		"api.x.ai",
		"api.groq.com",
		"generativelanguage.googleapis.com",
		"openrouter.ai",
		"api.openrouter.ai",
		"api.deepseek.com",
	}
}

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
func IsSupportedAuthorKind(kind string) bool {
	return len(RequiredBrokerHostsForKind(kind)) > 0
}
