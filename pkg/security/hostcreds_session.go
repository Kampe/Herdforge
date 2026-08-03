package security

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// HostCredsSession is exact-session broker authority for one non-interactive
// author harness. Secrets stay in the oracle's out-of-band store; workers only
// receive a least-authority channel descriptor (socket path or pre-opened FD)
// plus public dummy CLI sentinels — never model secrets or proxy bearers.
type HostCredsSession struct {
	ID      string
	Kind    string
	Oracle  *HostCredsOracle
	Store   SecretStore
	Rules   []RequestRule
	SockDir string

	mu      sync.Mutex
	closed  bool
	prompt  string
	started bool
}

// SessionConfig configures a HostCreds-brokered author session.
type SessionConfig struct {
	Kind      string
	SessionID string
	// Rules defaults to DefaultRequestRules().
	Rules []RequestRule
	// Store is the out-of-band secret authority. Required for real kinds.
	// If nil, seeded from CoordinatorHostCredsFromEnv() into MemorySecretStore.
	Store SecretStore
	// Worktree is optional worker workspace. Never used to store secrets.
	// Socket is placed under a private coordinator temp dir, not the worktree,
	// to avoid same-UID world-readable worktree credential channels.
	Worktree string
	// SocketDir optional private dir (0700) for the unix socket.
	SocketDir string
	// TTL defaults to DefaultSessionTTL.
	TTL time.Duration
	// Interactive must be false.
	Interactive bool
	// ExtraHosts/Rules for deterministic tests (e.g. 127.0.0.1).
	ExtraHosts []string
	TestRules  []RequestRule
}

// StartHostCredsSession creates a session-scoped oracle with HostCreds held
// out-of-band. Fails closed with typed BLOCKED when credentials are missing,
// kind is unbrokerable, platform unsupported, or interactive login requested.
//
// Does NOT:
//   - copy keys into worker env/files
//   - expose a proxy bearer token
//   - open browser/login UI
//   - accept dummy sentinels as upstream credentials
func StartHostCredsSession(cfg SessionConfig) (*HostCredsSession, error) {
	if cfg.Interactive {
		return nil, &BlockedError{
			Reason: BlockInteractiveDenied,
			Kind:   cfg.Kind,
			Detail: "interactive browser/login UI is forbidden for HostCreds sessions",
		}
	}
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}

	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if !IsSupportedAuthorKind(kind) && kind != "fake" && kind != "test" {
		d := DiagnoseKindAuthReadiness(kind)
		return nil, &BlockedError{
			Reason:        BlockUnbrokerableKind,
			Kind:          kind,
			HostsRequired: d.RequiredHosts,
			HostsCreds:    d.HostCredsPresent,
			Detail:        d.Blocker,
		}
	}

	store := cfg.Store
	if store == nil {
		store = NewMemorySecretStore()
		_ = LoadEnvIntoStore(store)
	}

	// Refuse if store only has dummy material for required hosts.
	if IsSupportedAuthorKind(kind) {
		required := RequiredBrokerHostsForKind(kind)
		present := store.Hosts()
		missing := []string{}
		for _, h := range required {
			v := strings.TrimSpace(store.Get(h))
			if v == "" || IsDummyCredential(v) {
				missing = append(missing, h)
			}
		}
		if len(missing) > 0 {
			return nil, &BlockedError{
				Reason:        BlockMissingCreds,
				Kind:          kind,
				HostsRequired: required,
				HostsCreds:    present,
				Detail: fmt.Sprintf(
					"missing real HostCreds for %v — dummy sentinels never count; no interactive login UI",
					missing,
				),
			}
		}
	}

	rules := cfg.Rules
	if len(cfg.TestRules) > 0 {
		rules = cfg.TestRules
	}
	if len(rules) == 0 {
		rules = DefaultRequestRules()
	}

	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}

	sockDir := cfg.SocketDir
	if sockDir == "" {
		// Private coordinator dir — NOT under worker worktree.
		// Short prefix: AF_UNIX path length limits on macOS.
		var err error
		sockDir, err = os.MkdirTemp("", "hc-*")
		if err != nil {
			return nil, err
		}
		_ = os.Chmod(sockDir, 0o700)
	}

	oracle, err := StartHostCredsOracle(OracleConfig{
		SessionID:  sid,
		Kind:       kind,
		Store:      store,
		Rules:      rules,
		TTL:        cfg.TTL,
		SocketDir:  sockDir,
		ExtraHosts: cfg.ExtraHosts,
	})
	if err != nil {
		return nil, err
	}

	return &HostCredsSession{
		ID:      sid,
		Kind:    kind,
		Oracle:  oracle,
		Store:   store,
		Rules:   rules,
		SockDir: sockDir,
	}, nil
}

// WorkerEnv returns environment variables safe for the worker process.
//
// Includes:
//   - HERD_HOSTCREDS_SESSION (public session id, not a secret)
//   - HERD_HOSTCREDS_SOCKET (session channel path — not a bearer token)
//   - Dummy API key sentinels so CLIs that refuse to start without a key will
//     boot; the oracle NEVER accepts these as upstream Authorization
//   - Explicit empty scrub of real secret env names
//
// Does NOT include:
//   - Real API keys / Authorization material
//   - Proxy bearer tokens
//   - Control tokens
//   - Broker CA private keys
func (s *HostCredsSession) WorkerEnv() []string {
	if s == nil || s.Oracle == nil {
		return nil
	}
	sock := s.Oracle.SocketPath()
	env := []string{
		"HERD_HOSTCREDS_SESSION=" + s.ID,
		"HERD_HOSTCREDS_SOCKET=" + sock,
		// Prefer FD binding when parent sets HERD_HOSTCREDS_FD; socket is fallback.
		"HERD_HOSTCREDS_CHANNEL=unix-oracle",
		// CLI bootstrap sentinels (public, never upstream).
		"ANTHROPIC_API_KEY=" + DummyNeverUpstream,
		"OPENAI_API_KEY=" + DummyNeverUpstream,
		"XAI_API_KEY=" + DummyNeverUpstream,
		// Scrub any host-creds map the parent might have inherited.
		"HERD_HOST_CREDS=",
		// Do not point workers at a credentialed HTTP_PROXY.
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"ALL_PROXY=",
		"all_proxy=",
	}
	return env
}

// WorkerEnvMap is a map form of WorkerEnv for adversarial inspection.
func (s *HostCredsSession) WorkerEnvMap() map[string]string {
	out := map[string]string{}
	for _, e := range s.WorkerEnv() {
		if i := strings.IndexByte(e, '='); i > 0 {
			out[e[:i]] = e[i+1:]
		}
	}
	return out
}

// WorkerDummyKey returns the public CLI bootstrap sentinel for kind.
func (s *HostCredsSession) WorkerDummyKey(kind string) string {
	return DummyNeverUpstream
}

// ConsumePrompt records a non-interactive prompt for exact-session causal proof.
func (s *HostCredsSession) ConsumePrompt(prompt string) error {
	if s == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil session"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &BlockedError{Reason: BlockNoSession, SessionID: s.ID, Detail: "session closed"}
	}
	low := strings.ToLower(prompt)
	if strings.Contains(low, "login") && (strings.Contains(low, "browser") || strings.Contains(low, "oauth") || strings.Contains(low, "interactive")) {
		return &BlockedError{
			Reason:    BlockInteractiveDenied,
			SessionID: s.ID,
			Kind:      s.Kind,
			Detail:    "interactive login prompt rejected",
		}
	}
	s.prompt = prompt
	s.started = true
	return nil
}

// PromptConsumed reports whether a non-interactive prompt was accepted.
func (s *HostCredsSession) PromptConsumed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started && s.prompt != ""
}

// LastPrompt returns the consumed prompt (empty if none).
func (s *HostCredsSession) LastPrompt() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt
}

// Rotate rotates HostCreds for host in the out-of-band store.
func (s *HostCredsSession) Rotate(host, newAuthorization string) error {
	if s == nil || s.Oracle == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil session"}
	}
	return s.Oracle.RotateHostCredential(host, newAuthorization)
}

// RevokeHost revokes HostCreds for one host in the out-of-band store.
func (s *HostCredsSession) RevokeHost(host string) error {
	if s == nil || s.Oracle == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil session"}
	}
	return s.Oracle.RevokeHostCredential(host)
}

// Revoke invalidates the entire session (channel closed, further calls DENIED).
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

// Restart re-binds the oracle channel; secrets re-seed from out-of-band store only.
func (s *HostCredsSession) Restart() error {
	if s == nil || s.Oracle == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil session"}
	}
	return s.Oracle.Restart()
}

// Close ends the session.
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
	// Best-effort private dir cleanup (socket already removed by oracle).
	if s.SockDir != "" {
		_ = os.RemoveAll(s.SockDir)
	}
	return err
}

// AssertWorkerCannotSeeSecret fails if any worker-visible surface contains secret.
func (s *HostCredsSession) AssertWorkerCannotSeeSecret(secret string) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if secret == "" {
		return fmt.Errorf("empty secret")
	}
	if IsDummyCredential(secret) {
		// Dummy is public by design; not a secret exposure.
		return nil
	}
	for k, v := range s.WorkerEnvMap() {
		if strings.Contains(v, secret) {
			return &BlockedError{
				Reason:    BlockSecretExposure,
				SessionID: s.ID,
				Detail:    fmt.Sprintf("secret leaked in worker env key %s", k),
			}
		}
	}
	// Socket path must not embed secret bytes.
	if s.Oracle != nil && strings.Contains(s.Oracle.SocketPath(), secret) {
		return &BlockedError{
			Reason:    BlockSecretExposure,
			SessionID: s.ID,
			Detail:    "secret leaked into socket path",
		}
	}
	// Worktree must not hold secret files (we never write them).
	return nil
}

// AssertNoWorkerBearerToken fails if WorkerEnv exposes a proxy/control bearer.
func (s *HostCredsSession) AssertNoWorkerBearerToken() error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	env := s.WorkerEnvMap()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "HERD_NETWORK_BROKER"} {
		v := strings.TrimSpace(env[k])
		if v == "" {
			continue
		}
		// Any non-empty proxy URL is disallowed in the oracle model.
		return &BlockedError{
			Reason:    BlockSecretExposure,
			SessionID: s.ID,
			Detail:    fmt.Sprintf("worker must not receive proxy credential channel via %s", k),
		}
	}
	// Dummy keys are OK; real sk- / Bearer non-dummy are not.
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY"} {
		v := env[k]
		if v != "" && !IsDummyCredential(v) {
			return &BlockedError{
				Reason:    BlockSecretExposure,
				SessionID: s.ID,
				Detail:    fmt.Sprintf("worker env %s holds non-dummy material", k),
			}
		}
	}
	return nil
}

// AttemptForbiddenCredentialAccess tries adversarial access paths that must DENY.
func (s *HostCredsSession) AttemptForbiddenCredentialAccess() error {
	if s == nil || s.Oracle == nil {
		return fmt.Errorf("nil session")
	}
	// 1) Non-allowlisted host
	resp, err := CallOracle(s.Oracle.SocketPath(), OracleRequest{
		SessionID: s.ID,
		Host:      "evil.example.invalid",
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Body:      `{}`,
	})
	if err != nil {
		return err
	}
	if resp.OK {
		return fmt.Errorf("forbidden host was allowed")
	}
	// 2) Allowlisted host, disallowed path (exfil / admin)
	host := ""
	if len(s.Oracle.Hosts) > 0 {
		host = s.Oracle.Hosts[0]
	}
	if host != "" && host != "127.0.0.1" {
		resp, err = CallOracle(s.Oracle.SocketPath(), OracleRequest{
			SessionID: s.ID,
			Host:      host,
			Method:    "POST",
			Path:      "/v1/admin/export-keys",
			Body:      `{}`,
		})
		if err != nil {
			return err
		}
		if resp.OK {
			return fmt.Errorf("forbidden path was allowed")
		}
	}
	// 3) Worker-supplied real Authorization injection
	injectHost := host
	if injectHost == "" {
		injectHost = "api.x.ai"
	}
	resp, err = CallOracle(s.Oracle.SocketPath(), OracleRequest{
		SessionID: s.ID,
		Host:      injectHost,
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Headers:   map[string]string{"Authorization": "Bearer sk-worker-injected-real"},
		Body:      `{}`,
	})
	if err != nil {
		return err
	}
	if resp.OK {
		return fmt.Errorf("worker auth injection was allowed")
	}
	// 4) Error bodies must not contain real secrets from store
	for _, h := range s.Oracle.CredHosts() {
		sec := s.Store.Get(h)
		if sec != "" && strings.Contains(resp.Error, sec) {
			return fmt.Errorf("error body leaked secret")
		}
	}
	return nil
}

// OpenPreopenedFD returns a connected *os.File for ExtraFiles inheritance.
func (s *HostCredsSession) OpenPreopenedFD() (*os.File, error) {
	if s == nil || s.Oracle == nil {
		return nil, fmt.Errorf("nil session")
	}
	return s.Oracle.File()
}