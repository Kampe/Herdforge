package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// HostCredsSession is exact-session broker authority for one non-interactive
// author harness. Secrets stay in the proxy; workers only get WorkerEnv().
type HostCredsSession struct {
	ID        string
	Kind      string
	Proxy     *HostCredsProxy
	Store     SecretStore // out-of-band source of truth for restart re-seed
	Allowlist []string
	CAPath    string // public CA path under worktree (not secret)

	mu      sync.Mutex
	closed  bool
	prompt  string // last non-interactive prompt (causal proof)
	started bool
}

// SessionConfig configures a HostCreds-brokered author session.
type SessionConfig struct {
	Kind      string
	SessionID string // optional; generated if empty
	// Allowlist defaults to DefaultHostAllowlist(); tests may add 127.0.0.1.
	Allowlist []string
	// Store is the out-of-band secret authority. If nil, a MemorySecretStore
	// is seeded from CoordinatorHostCredsFromEnv().
	Store SecretStore
	// Worktree is where the public CA PEM is written (worker-readable).
	// Never used to store secrets.
	Worktree string
	// Interactive must be false. Interactive login UI is forbidden.
	Interactive bool
}

// StartHostCredsSession creates a session-scoped broker with HostCreds seeded
// from the out-of-band store. Fails closed with typed BLOCKED when:
//   - platform unsupported
//   - kind unbrokerable / missing HostCreds
//   - interactive login requested
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
		// "fake"/"test" kinds are for deterministic causal proofs only.
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

	allow := cfg.Allowlist
	if len(allow) == 0 {
		allow = DefaultHostAllowlist()
	}

	// For real author kinds, require HostCreds covering RequiredBrokerHosts.
	if IsSupportedAuthorKind(kind) {
		required := RequiredBrokerHostsForKind(kind)
		present := store.Hosts()
		missing := []string{}
		for _, h := range required {
			if strings.TrimSpace(store.Get(h)) == "" {
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
					"missing HostCreds for %v — no silent fallback, no interactive login UI",
					missing,
				),
			}
		}
	}

	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}

	proxy, err := StartHostCredsProxy(allow, sid)
	if err != nil {
		return nil, err
	}
	if err := proxy.SeedFromStore(store); err != nil {
		_ = proxy.Close()
		return nil, err
	}
	if err := proxy.EnsureCA(); err != nil {
		_ = proxy.Close()
		return nil, err
	}

	sess := &HostCredsSession{
		ID:        sid,
		Kind:      kind,
		Proxy:     proxy,
		Store:     store,
		Allowlist: append([]string(nil), allow...),
	}

	if cfg.Worktree != "" {
		caPath, err := writeAgentCAPEM(cfg.Worktree, proxy.CAPEM())
		if err != nil {
			_ = proxy.Close()
			return nil, err
		}
		sess.CAPath = caPath
	}

	return sess, nil
}

// WorkerEnv returns environment variables safe for the worker process.
// Never includes model credential bytes or control token.
func (s *HostCredsSession) WorkerEnv() []string {
	if s == nil || s.Proxy == nil {
		return nil
	}
	env := s.Proxy.WorkerProxyEnv()
	if s.CAPath != "" {
		env = append(env,
			"SSL_CERT_FILE="+s.CAPath,
			"SSL_CERT_DIR="+filepath.Dir(s.CAPath),
		)
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

// ConsumePrompt records a non-interactive prompt for exact-session causal proof.
// Interactive login prompts are rejected.
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

// Rotate rotates HostCreds for host in both out-of-band store and live proxy.
func (s *HostCredsSession) Rotate(host, newAuthorization string) error {
	if s == nil || s.Proxy == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil session"}
	}
	if s.Store != nil {
		if err := s.Store.Set(host, newAuthorization); err != nil {
			return err
		}
	}
	return s.Proxy.RotateHostCredential(host, newAuthorization)
}

// Revoke revokes HostCreds for host in store and live proxy.
func (s *HostCredsSession) Revoke(host string) error {
	if s == nil || s.Proxy == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil session"}
	}
	if s.Store != nil {
		_ = s.Store.Delete(host)
	}
	return s.Proxy.RevokeHostCredential(host)
}

// Restart tears down the proxy and starts a new one with the same session ID,
// re-seeding from the out-of-band store (never from worker state).
func (s *HostCredsSession) Restart() error {
	if s == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil session"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &BlockedError{Reason: BlockNoSession, SessionID: s.ID, Detail: "session closed"}
	}
	oldGen := 0
	if s.Proxy != nil {
		oldGen = s.Proxy.Generation()
		_ = s.Proxy.Close()
	}
	proxy, err := StartHostCredsProxy(s.Allowlist, s.ID)
	if err != nil {
		return err
	}
	// Force generation > old for causal binding after restart.
	proxy.mu.Lock()
	proxy.generation = oldGen + 1
	proxy.mu.Unlock()
	if s.Store != nil {
		if err := proxy.SeedFromStore(s.Store); err != nil {
			_ = proxy.Close()
			return err
		}
	}
	if err := proxy.EnsureCA(); err != nil {
		_ = proxy.Close()
		return err
	}
	s.Proxy = proxy
	return nil
}

// Close ends the session and wipes broker credential material.
func (s *HostCredsSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.Proxy != nil {
		return s.Proxy.Close()
	}
	return nil
}

// AssertWorkerCannotSeeSecret fails if any worker-visible surface contains secret.
// Surfaces: worker env values, CA file content, worktree contain dir.
func (s *HostCredsSession) AssertWorkerCannotSeeSecret(secret string) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if secret == "" {
		return fmt.Errorf("empty secret")
	}
	// Env
	for k, v := range s.WorkerEnvMap() {
		if strings.Contains(v, secret) {
			return &BlockedError{
				Reason:    BlockSecretExposure,
				SessionID: s.ID,
				Detail:    fmt.Sprintf("secret leaked in worker env key %s", k),
			}
		}
		// Common secret env names must be empty.
		switch k {
		case "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS":
			if strings.TrimSpace(v) != "" {
				return &BlockedError{
					Reason:    BlockSecretExposure,
					SessionID: s.ID,
					Detail:    fmt.Sprintf("worker env %s must be empty", k),
				}
			}
		}
	}
	// CA PEM must not contain the Authorization secret.
	if s.CAPath != "" {
		b, err := os.ReadFile(s.CAPath)
		if err == nil && strings.Contains(string(b), secret) {
			return &BlockedError{
				Reason:    BlockSecretExposure,
				SessionID: s.ID,
				Detail:    "secret leaked into CA PEM file",
			}
		}
	}
	// Proxy URL may contain proxy token but never model secret.
	if s.Proxy != nil && strings.Contains(s.Proxy.ProxyURL(), secret) {
		return &BlockedError{
			Reason:    BlockSecretExposure,
			SessionID: s.ID,
			Detail:    "secret leaked into proxy URL",
		}
	}
	return nil
}

// AttemptForbiddenCredentialAccess tries adversarial access paths that must DENY:
//  1. Reading host cred via proxy token on control channel
//  2. CONNECT to non-allowlisted host
// Returns nil only when all forbidden attempts are correctly denied.
func (s *HostCredsSession) AttemptForbiddenCredentialAccess(forbiddenHost string) error {
	if s == nil || s.Proxy == nil {
		return fmt.Errorf("nil session")
	}
	// 1) Control path with PROXY token (not control token) must fail.
	if err := proveProxyTokenCannotReadControl(s.Proxy); err != nil {
		return fmt.Errorf("proxy-token control access not denied: %w", err)
	}
	// 2) Non-allowlisted host CONNECT must be 403.
	if forbiddenHost == "" {
		forbiddenHost = "evil.example.invalid"
	}
	if s.Proxy.HostAllowed(forbiddenHost) {
		return fmt.Errorf("forbidden host unexpectedly allowlisted: %s", forbiddenHost)
	}
	if err := proveConnectDenied(s.Proxy, forbiddenHost+":443"); err != nil {
		return fmt.Errorf("forbidden host not denied: %w", err)
	}
	return nil
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess-%d", os.Getpid())
	}
	return "sess-" + hex.EncodeToString(b[:])
}

func writeAgentCAPEM(worktree string, pem []byte) (string, error) {
	if len(pem) == 0 {
		return "", fmt.Errorf("writeAgentCAPEM: empty pem")
	}
	dir := filepath.Join(worktree, ".herd", "contain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "broker-ca.pem")
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
