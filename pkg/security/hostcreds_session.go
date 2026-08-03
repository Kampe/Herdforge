package security

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// HostCredsSession is exact-session broker authority for one non-interactive author.
// Secrets live only inside CredentialAuthority; workers get channel + dummy sentinels.
type HostCredsSession struct {
	ID     string
	Kind   string
	Oracle *HostCredsOracle
	Auth   CredentialAuthority

	mu      sync.Mutex
	closed  bool
	prompt  string
	started bool
	sockDir string
}

// SessionConfig configures a HostCreds session.
type SessionConfig struct {
	Kind        string
	SessionID   string
	Authority   CredentialAuthority // required for production; tests use TestCredentialVault
	TTL         time.Duration
	SocketDir   string
	Interactive bool
	// AllowLoopback enables 127.0.0.1 hosts for deterministic proofs only.
	AllowLoopback bool
	// Rules overrides per-kind rules (tests only).
	Rules []RequestRule
}

// StartHostCredsSession creates a session-scoped oracle.
// Production: Authority must be handle-backed (keychain/op). Raw env API keys
// are never loaded as production credentials.
func StartHostCredsSession(cfg SessionConfig) (*HostCredsSession, error) {
	if cfg.Interactive {
		return nil, &BlockedError{Reason: BlockInteractiveDenied, Code: "interactive", Kind: cfg.Kind}
	}
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}

	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if !IsSupportedAuthorKind(kind) && kind != "fake" && kind != "test" {
		return nil, &BlockedError{
			Reason:        BlockUnbrokerableKind,
			Code:          "kind",
			Kind:          kind,
			HostsRequired: RequiredBrokerHostsForKind(kind),
		}
	}

	auth := cfg.Authority
	if auth == nil {
		return nil, &BlockedError{Reason: BlockMissingCreds, Code: "authority_required", Kind: kind}
	}

	// Refuse raw env as production authority.
	if auth.Class() != "test" && EnvRawAPIKeysPresent() {
		// Handles may still work, but warn-as-block if no handles resolved.
		if len(auth.Hosts()) == 0 {
			return nil, &BlockedError{
				Reason: BlockEnvNotAuthority,
				Code:   "raw_env_keys_not_oob",
				Kind:   kind,
			}
		}
	}

	rules := cfg.Rules
	if len(rules) == 0 {
		rules = RequestRulesForKind(kind)
	}
	if len(rules) == 0 {
		return nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "no_rules", Kind: kind}
	}

	// Required hosts must be present in authority for supported kinds.
	if IsSupportedAuthorKind(kind) {
		required := RequiredBrokerHostsForKind(kind)
		var missing []string
		for _, h := range required {
			if !auth.Has(h) {
				missing = append(missing, h)
			}
		}
		if len(missing) > 0 {
			return nil, &BlockedError{
				Reason:        BlockMissingCreds,
				Code:          "missing_hosts",
				Kind:          kind,
				HostsRequired: required,
				HostsPresent:  auth.Hosts(),
			}
		}
	}

	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}

	oracle, err := StartHostCredsOracle(OracleConfig{
		SessionID:     sid,
		Kind:          kind,
		Authority:     auth,
		Rules:         rules,
		TTL:           cfg.TTL,
		SocketDir:     cfg.SocketDir,
		AllowLoopback: cfg.AllowLoopback || kind == "fake" || kind == "test",
	})
	if err != nil {
		return nil, err
	}

	return &HostCredsSession{
		ID:      sid,
		Kind:    kind,
		Oracle:  oracle,
		Auth:    auth,
		sockDir: oracle.sockDir,
	}, nil
}

// WorkerEnv returns worker-safe env: channel descriptors + dummy CLI sentinels.
// Never includes real secrets, proxy URLs, or control tokens.
func (s *HostCredsSession) WorkerEnv() []string {
	if s == nil || s.Oracle == nil {
		return nil
	}
	return []string{
		"HERD_HOSTCREDS_SESSION=" + s.ID,
		"HERD_HOSTCREDS_SOCKET=" + s.Oracle.SocketPath(),
		"HERD_HOSTCREDS_CHANNEL=unix-oracle",
		"ANTHROPIC_API_KEY=" + DummyNeverUpstream,
		"OPENAI_API_KEY=" + DummyNeverUpstream,
		"XAI_API_KEY=" + DummyNeverUpstream,
		"HERD_HOST_CREDS=",
		"HERD_HOSTCREDS_HANDLES=", // workers never get handles either
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"ALL_PROXY=",
		"all_proxy=",
	}
}

func (s *HostCredsSession) WorkerEnvMap() map[string]string {
	out := map[string]string{}
	for _, e := range s.WorkerEnv() {
		if i := strings.IndexByte(e, '='); i > 0 {
			out[e[:i]] = e[i+1:]
		}
	}
	return out
}

func (s *HostCredsSession) ConsumePrompt(prompt string) error {
	if s == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &BlockedError{Reason: BlockNoSession, SessionID: s.ID, Code: "closed"}
	}
	low := strings.ToLower(prompt)
	if strings.Contains(low, "login") && (strings.Contains(low, "browser") || strings.Contains(low, "oauth") || strings.Contains(low, "interactive")) {
		return &BlockedError{Reason: BlockInteractiveDenied, SessionID: s.ID, Kind: s.Kind, Code: "login_prompt"}
	}
	s.prompt = prompt
	s.started = true
	return nil
}

func (s *HostCredsSession) PromptConsumed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started && s.prompt != ""
}

func (s *HostCredsSession) LastPrompt() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt
}

func (s *HostCredsSession) RotateFromHandle(host, handle string) error {
	if s == nil || s.Oracle == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	return s.Oracle.RotateFromHandle(host, handle)
}

func (s *HostCredsSession) RevokeHost(host string) error {
	if s == nil || s.Oracle == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	return s.Oracle.RevokeHost(host)
}

func (s *HostCredsSession) Revoke() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.Oracle != nil {
		return s.Oracle.Revoke()
	}
	return nil
}

func (s *HostCredsSession) Restart() error {
	if s == nil || s.Oracle == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	return s.Oracle.Restart()
}

func (s *HostCredsSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	var err error
	if s.Oracle != nil {
		err = s.Oracle.Close()
	}
	if s.sockDir != "" {
		_ = os.RemoveAll(s.sockDir)
	}
	return err
}

// AssertWorkerCannotSeeSecret checks worker env does not contain secret bytes.
// There is no Get API to leak through.
func (s *HostCredsSession) AssertWorkerCannotSeeSecret(secret string) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if secret == "" || IsDummyCredential(secret) {
		return nil
	}
	for k, v := range s.WorkerEnvMap() {
		if strings.Contains(v, secret) {
			return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "env:" + k}
		}
	}
	if s.Oracle != nil && strings.Contains(s.Oracle.SocketPath(), secret) {
		return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "socket_path"}
	}
	return nil
}

func (s *HostCredsSession) AssertNoWorkerBearerToken() error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	env := s.WorkerEnvMap()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "HERD_NETWORK_BROKER"} {
		if strings.TrimSpace(env[k]) != "" {
			return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "proxy_env:" + k}
		}
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY"} {
		if v := env[k]; v != "" && !IsDummyCredential(v) {
			return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "nondummy:" + k}
		}
	}
	return nil
}

// AssertNoPublicSecretExport fails if authority exposes Get/Snapshot-like APIs.
// Enforced by type system: CredentialAuthority has no Get/Snapshot methods.
func AssertNoPublicSecretExport(auth CredentialAuthority) error {
	if auth == nil {
		return fmt.Errorf("nil authority")
	}
	// Reflect-free: if Hosts returns values that look like secrets, fail.
	for _, h := range auth.Hosts() {
		if strings.Contains(h, "Bearer ") || strings.HasPrefix(h, "sk-") {
			return &BlockedError{Reason: BlockSecretExposure, Code: "hosts_look_like_secrets"}
		}
	}
	return nil
}

func (s *HostCredsSession) AttemptForbiddenCredentialAccess() error {
	if s == nil || s.Oracle == nil {
		return fmt.Errorf("nil session")
	}
	// 1) Non-allowlisted host
	resp, err := CallOracle(s.Oracle.SocketPath(), OracleRequest{
		SessionID: s.ID, Host: "evil.example.invalid", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil {
		return err
	}
	if resp.OK {
		return fmt.Errorf("forbidden host allowed")
	}
	// 2) Wrong path on allowlisted host
	host := ""
	if len(s.Oracle.Hosts) > 0 {
		host = s.Oracle.Hosts[0]
	}
	if host != "" && host != "127.0.0.1" {
		resp, err = CallOracle(s.Oracle.SocketPath(), OracleRequest{
			SessionID: s.ID, Host: host, Method: "POST", Path: "/v1/admin/export-keys", Body: `{}`,
		})
		if err != nil {
			return err
		}
		if resp.OK {
			return fmt.Errorf("forbidden path allowed")
		}
	}
	// 3) Worker real Authorization injection
	injectHost := host
	if injectHost == "" {
		injectHost = "api.x.ai"
	}
	resp, err = CallOracle(s.Oracle.SocketPath(), OracleRequest{
		SessionID: s.ID, Host: injectHost, Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"Authorization": "Bearer sk-worker-injected-real"},
		Body:    `{}`,
	})
	if err != nil {
		return err
	}
	if resp.OK {
		return fmt.Errorf("worker auth injection allowed")
	}
	// 4) Header CRLF injection
	resp, err = CallOracle(s.Oracle.SocketPath(), OracleRequest{
		SessionID: s.ID, Host: injectHost, Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"X-Evil": "v\r\nAuthorization: Bearer stolen"},
		Body:    `{}`,
	})
	if err != nil {
		return err
	}
	if resp.OK {
		return fmt.Errorf("header injection allowed")
	}
	return nil
}

func (s *HostCredsSession) OpenPreopenedFD() (*os.File, error) {
	if s == nil || s.Oracle == nil {
		return nil, fmt.Errorf("nil session")
	}
	return s.Oracle.File()
}
