package security

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// HostCredsSession is exact-session broker authority for one non-interactive author.
// Secrets live only inside the unexported authority; harness transport is TLS MITM.
type HostCredsSession struct {
	ID   string
	Kind string

	// Mitm is the stock-CLI transport (HTTPS_PROXY CONNECT). Public for tests
	// of allow/deny only — never holds secrets.
	Mitm *TLSMitmProxy
	// Oracle is optional capability channel for component tests; harnesses use Mitm.
	Oracle *HostCredsProxyOracle

	authority CredentialAuthority // unexported — never SecretStore/Get/Snapshot
	rules     []RequestRule
	maxReqs   int
	ttl       time.Duration
	expires   time.Time
	ownCADir  string
	ownSock   bool

	mu             sync.Mutex
	closed         bool
	prompt         string
	promptInArgv   bool // set true only when harness process started with prompt
	authorPID      int
	actionsUsed    int
}

// HostCredsProxyOracle is a thin alias documentation type for the capability oracle.
// Prefer Mitm for real harnesses.
type HostCredsProxyOracle = HostCredsOracle

// SessionConfig configures a HostCreds session (one kind, least privilege).
type SessionConfig struct {
	Kind          string
	SessionID     string // required non-empty for production
	Authority     CredentialAuthority
	TTL           time.Duration
	MaxActions    int  // default 16
	Interactive   bool
	AllowLoopback bool // component tests only
	// CADir for public MITM CA PEM (worker-readable public cert only).
	CADir string
	// SocketDir only for optional unix oracle component tests.
	SocketDir string
	// EnableOracle starts the unix capability oracle (tests). Live harness uses Mitm.
	EnableOracle bool
}

// StartHostCredsSession creates a kind-scoped session with MITM transport.
func StartHostCredsSession(cfg SessionConfig) (*HostCredsSession, error) {
	if cfg.Interactive {
		return nil, &BlockedError{Reason: BlockInteractiveDenied, Code: "interactive", Kind: cfg.Kind}
	}
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if !IsSupportedAuthorKind(kind) && kind != "fake" && kind != "test" {
		return nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "kind", Kind: kind}
	}
	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		// Production requires explicit session id for least privilege binding.
		if kind != "fake" && kind != "test" {
			return nil, &BlockedError{Reason: BlockNoSession, Code: "session_id_required", Kind: kind}
		}
		sid = newSessionID()
	}
	auth := cfg.Authority
	if auth == nil {
		return nil, &BlockedError{Reason: BlockMissingCreds, Code: "authority_required", Kind: kind}
	}
	if auth.Class() != "test" && EnvRawAPIKeysPresent() && len(auth.Hosts()) == 0 {
		return nil, &BlockedError{Reason: BlockEnvNotAuthority, Code: "raw_env_keys_not_oob", Kind: kind}
	}

	// Exact per-kind rules only — never a global multi-provider rule set.
	rules := RequestRulesForKind(kind)
	if len(rules) == 0 {
		return nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "no_rules", Kind: kind}
	}

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
				Reason: BlockMissingCreds, Code: "missing_hosts", Kind: kind,
				HostsRequired: required, HostsPresent: auth.Hosts(),
			}
		}
	}

	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	maxA := cfg.MaxActions
	if maxA <= 0 {
		maxA = 16
	}

	caDir := cfg.CADir
	ownCA := false
	if caDir == "" {
		var err error
		caDir, err = os.MkdirTemp("", "hc-ca-*")
		if err != nil {
			return nil, err
		}
		ownCA = true
	}

	mitm, err := StartTLSMitmProxy(sid, auth, rules, caDir, maxA)
	if err != nil {
		if ownCA {
			_ = os.RemoveAll(caDir)
		}
		return nil, err
	}

	sess := &HostCredsSession{
		ID:        sid,
		Kind:      kind,
		Mitm:      mitm,
		authority: auth,
		rules:     rules,
		maxReqs:   maxA,
		ttl:       ttl,
		expires:   time.Now().Add(ttl),
		ownCADir:  "",
	}
	if ownCA {
		sess.ownCADir = caDir
	}

	if cfg.EnableOracle || kind == "fake" || kind == "test" {
		orc, oerr := StartHostCredsOracle(OracleConfig{
			SessionID:     sid,
			Kind:          kind,
			Authority:     auth,
			Rules:         rules,
			TTL:           ttl,
			SocketDir:     cfg.SocketDir,
			AllowLoopback: cfg.AllowLoopback || kind == "fake" || kind == "test",
		})
		if oerr != nil {
			_ = mitm.Close()
			if ownCA {
				_ = os.RemoveAll(caDir)
			}
			return nil, oerr
		}
		sess.Oracle = orc
		sess.ownSock = cfg.SocketDir == ""
	}

	return sess, nil
}

// WorkerEnv is for stock hosted CLIs: HTTPS MITM proxy + public CA only.
// Does NOT set real or dummy API keys (explicit empty to block inheritance).
func (s *HostCredsSession) WorkerEnv() []string {
	if s == nil || s.Mitm == nil {
		return nil
	}
	return HarnessProxyEnv(s.Mitm, s.ID)
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

// RecordHarnessPrompt marks that a real OS process was started with this prompt
// in argv (not a mere Go string assignment). Live path must call this after Start.
func (s *HostCredsSession) RecordHarnessPrompt(prompt string, authorPID int) error {
	if s == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &BlockedError{Reason: BlockNoSession, SessionID: s.ID, Code: "closed"}
	}
	if time.Now().After(s.expires) {
		return &BlockedError{Reason: BlockExpired, SessionID: s.ID, Code: "expired"}
	}
	low := strings.ToLower(prompt)
	if strings.Contains(low, "login") && (strings.Contains(low, "browser") || strings.Contains(low, "oauth")) {
		return &BlockedError{Reason: BlockInteractiveDenied, SessionID: s.ID, Kind: s.Kind, Code: "login_prompt"}
	}
	if authorPID <= 0 {
		return &BlockedError{Reason: BlockAbuse, SessionID: s.ID, Code: "author_pid_required"}
	}
	s.prompt = prompt
	s.promptInArgv = true
	s.authorPID = authorPID
	if s.Mitm != nil {
		s.Mitm.AllowPID(authorPID)
	}
	return nil
}

// ConsumePrompt is deprecated for acceptance — only records intent without a process.
// Live AC requires RecordHarnessPrompt after real process start.
// Kept for diagnose paths; does NOT set PromptConsumed for live admission.
func (s *HostCredsSession) ConsumePrompt(prompt string) error {
	if s == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &BlockedError{Reason: BlockNoSession, SessionID: s.ID, Code: "closed"}
	}
	s.prompt = prompt
	// intentionally does NOT set promptInArgv
	return nil
}

// PromptConsumed is true only when a harness process was started with the prompt in argv.
func (s *HostCredsSession) PromptConsumed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptInArgv && s.prompt != "" && s.authorPID > 0
}

func (s *HostCredsSession) LastPrompt() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt
}

func (s *HostCredsSession) AuthorPID() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorPID
}

func (s *HostCredsSession) RotateFromHandle(host, handle string) error {
	if s == nil || s.authority == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	if time.Now().After(s.expires) {
		return &BlockedError{Reason: BlockExpired, SessionID: s.ID, Code: "expired"}
	}
	return s.authority.RotateFromHandle(host, handle)
}

func (s *HostCredsSession) RevokeHost(host string) error {
	if s == nil || s.authority == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	return s.authority.Revoke(host)
}

func (s *HostCredsSession) Revoke() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.Close()
}

// Restart safely replaces listeners: new first, then close old.
func (s *HostCredsSession) Restart() error {
	if s == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil"}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &BlockedError{Reason: BlockNoSession, SessionID: s.ID, Code: "closed"}
	}
	if time.Now().After(s.expires) {
		s.mu.Unlock()
		return &BlockedError{Reason: BlockExpired, SessionID: s.ID, Code: "expired"}
	}
	auth := s.authority
	rules := s.rules
	sid := s.ID
	maxA := s.maxReqs
	caDir := s.ownCADir
	if caDir == "" && s.Mitm != nil {
		caDir = filepathDir(s.Mitm.CAPEMPath())
	}
	oldMitm := s.Mitm
	oldOrc := s.Oracle
	s.mu.Unlock()

	if auth != nil && auth.Durable() {
		if ha, ok := auth.(*HandleAuthority); ok {
			if err := ha.ReResolveAll(); err != nil {
				return err
			}
		}
	}

	newMitm, err := StartTLSMitmProxy(sid, auth, rules, caDir, maxA)
	if err != nil {
		return err // old still running
	}
	// Swap then close old.
	s.mu.Lock()
	s.Mitm = newMitm
	if s.authorPID > 0 {
		newMitm.AllowPID(s.authorPID)
	}
	s.mu.Unlock()
	if oldMitm != nil {
		_ = oldMitm.Close()
	}
	if oldOrc != nil {
		// Oracle restart if present
		if rerr := oldOrc.Restart(); rerr != nil {
			// non-fatal for harness path
			_ = rerr
		}
	}
	return nil
}

func (s *HostCredsSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	mitm := s.Mitm
	orc := s.Oracle
	ownCA := s.ownCADir
	ownSock := s.ownSock
	s.Mitm = nil
	s.Oracle = nil
	s.mu.Unlock()

	var first error
	if mitm != nil {
		if err := mitm.Close(); err != nil && first == nil {
			first = err
		}
	}
	if orc != nil {
		sockDir := orc.sockDir
		if err := orc.Close(); err != nil && first == nil {
			first = err
		}
		// Only remove oracle-owned temp sock dir, never caller SocketDir unless ownSock.
		if ownSock && sockDir != "" {
			_ = os.RemoveAll(sockDir)
		}
	}
	// Public CA dir may be removed if we created it; PEM is not secret.
	if ownCA != "" {
		_ = os.RemoveAll(ownCA)
	}
	return first
}

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
		// No API key material at all.
		if (k == "XAI_API_KEY" || k == "OPENAI_API_KEY" || k == "ANTHROPIC_API_KEY") && strings.TrimSpace(v) != "" {
			return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "key_env_nonempty:" + k}
		}
	}
	// Proxy URL must not contain secret or userinfo credentials.
	if s.Mitm != nil {
		u := s.Mitm.ProxyURL()
		if strings.Contains(u, secret) || strings.Contains(u, "@") {
			return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "proxy_url"}
		}
	}
	return nil
}

func (s *HostCredsSession) AssertNoWorkerBearerToken() error {
	if s == nil {
		return fmt.Errorf("nil")
	}
	env := s.WorkerEnvMap()
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		v := env[k]
		if strings.Contains(v, "@") || strings.Contains(strings.ToLower(v), "bearer") {
			return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "proxy_bearer"}
		}
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY"} {
		if strings.TrimSpace(env[k]) != "" {
			return &BlockedError{Reason: BlockSecretExposure, SessionID: s.ID, Code: "api_key_planted:" + k}
		}
	}
	return nil
}

func (s *HostCredsSession) AttemptForbiddenCredentialAccess() error {
	if s == nil || s.Mitm == nil {
		return fmt.Errorf("nil")
	}
	if err := ProveMITMExactHost(s.Mitm, "evil.example.invalid"); err != nil {
		return err
	}
	// Cross-provider: grok session must not allow api.openai.com
	if s.Kind == "grok" {
		if err := ProveMITMExactHost(s.Mitm, "api.openai.com"); err != nil {
			return err
		}
	}
	if s.Oracle != nil {
		resp, err := CallOracle(s.Oracle.SocketPath(), OracleRequest{
			SessionID: s.ID, // required
			Host:      "evil.example.invalid", Method: "POST", Path: "/v1/chat/completions",
		})
		if err != nil {
			return err
		}
		if resp.OK {
			return fmt.Errorf("oracle allowed evil host")
		}
		// SessionID required
		resp, err = CallOracle(s.Oracle.SocketPath(), OracleRequest{
			SessionID: "", Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions",
		})
		if err != nil {
			return err
		}
		if resp.OK {
			return fmt.Errorf("oracle allowed empty session_id")
		}
	}
	return nil
}

func (s *HostCredsSession) OpenPreopenedFD() (*os.File, error) {
	if s == nil || s.Oracle == nil {
		return nil, &BlockedError{Reason: BlockAbuse, Code: "oracle_fd_optional"}
	}
	return s.Oracle.File()
}

func filepathDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return p
	}
	return p[:i]
}
