package security

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxOracleBodyBytes caps request bodies accepted from workers (abuse bound).
const MaxOracleBodyBytes = 1 << 20 // 1 MiB

// OracleRequest is a worker-submitted intent. It must NOT contain real secrets.
// Authorization, if present, may only be the dummy sentinel (stripped).
type OracleRequest struct {
	SessionID string            `json:"session_id"`
	Host      string            `json:"host"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Action    string            `json:"action,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
}

// OracleResponse is returned to the worker. Never includes upstream secrets.
type OracleResponse struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
	Error      string `json:"error,omitempty"` // redacted
	Action     string `json:"action,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

// HostCredsOracle is the session-bound signing authority.
//
// Channel: Unix domain socket (mode 0600 under a private dir). Preferred
// production binding is a pre-opened FD inherited by the worker (ExtraFiles);
// the socket path is not a shared multi-worker credential.
//
// There is NO worker-visible proxy bearer token. Same-UID path readability is
// mitigated by session id binding + expiry + single-session ownership + FD mode.
type HostCredsOracle struct {
	SessionID string
	Kind      string
	Rules     []RequestRule
	Hosts     []string // host allowlist derived from rules + extras
	ExpiresAt time.Time

	mu        sync.Mutex
	store     SecretStore // out-of-band; never worker-readable
	revoked   bool
	closed    bool
	ln        net.Listener
	sockPath  string
	sockDir   string
	generation int
	// lastUpstreamAuth is test-only capture of what would be sent (never worker).
	// Only set when CaptureUpstreamAuth is true (tests).
	CaptureUpstreamAuth bool
	lastUpstreamAuth    string
	// dialHook allows tests to intercept upstream dials (loopback fake).
	// When set, network/addr may be ignored; hook must dial the fake upstream.
	dialHook func(network, addr string) (net.Conn, error)
	// resolveHook allows tests to pin DNS.
	resolveHook func(host string) (net.IP, error)
	// forceHTTP disables TLS for loopback test upstreams.
	forceHTTP bool
}

// OracleConfig configures a HostCredsOracle.
type OracleConfig struct {
	SessionID  string
	Kind       string
	Store      SecretStore
	Rules      []RequestRule
	TTL        time.Duration
	SocketDir  string // private dir for unix socket; created 0700
	// ExtraHosts for test loopback only (must also have matching Rules).
	ExtraHosts []string
}

// StartHostCredsOracle creates a session-bound oracle listening on a Unix socket.
func StartHostCredsOracle(cfg OracleConfig) (*HostCredsOracle, error) {
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("oracle: out-of-band SecretStore required")
	}
	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}
	rules := cfg.Rules
	if len(rules) == 0 {
		rules = DefaultRequestRules()
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	sockDir := cfg.SocketDir
	if sockDir == "" {
		// Keep path short: macOS sun_path is ~104 bytes.
		var err error
		sockDir, err = os.MkdirTemp("", "hc-*")
		if err != nil {
			return nil, fmt.Errorf("oracle sock dir: %w", err)
		}
	} else {
		if err := os.MkdirAll(sockDir, 0o700); err != nil {
			return nil, err
		}
		_ = os.Chmod(sockDir, 0o700)
	}

	// Short socket name (path length limits). Session binding is in protocol, not filename.
	sockPath := filepath.Join(sockDir, "o.sock")
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("oracle unix listen: %w", err)
	}
	_ = os.Chmod(sockPath, 0o600)

	hosts := hostSetFromRules(rules)
	for _, h := range cfg.ExtraHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			hosts = appendUnique(hosts, h)
		}
	}

	o := &HostCredsOracle{
		SessionID:  sid,
		Kind:       strings.ToLower(strings.TrimSpace(cfg.Kind)),
		Rules:      append([]RequestRule(nil), rules...),
		Hosts:      hosts,
		ExpiresAt:  time.Now().Add(ttl),
		store:      cfg.Store,
		ln:         ln,
		sockPath:   sockPath,
		sockDir:    sockDir,
		generation: 1,
	}
	go o.serve()
	return o, nil
}

// SocketPath returns the Unix socket path (session-private). This is a channel
// endpoint, not a bearer credential. Prefer File() / pre-opened FD for spawn.
func (o *HostCredsOracle) SocketPath() string {
	if o == nil {
		return ""
	}
	return o.sockPath
}

// File returns a *os.File connected to the oracle for pre-opened FD inheritance.
// Caller should pass via cmd.ExtraFiles and close its copy after Start.
// Preferred production binding: worker speaks only on this FD.
func (o *HostCredsOracle) File() (*os.File, error) {
	if o == nil || o.ln == nil {
		return nil, fmt.Errorf("oracle not listening")
	}
	// Dial our own socket to produce a connected FD for the worker side.
	c, err := net.DialTimeout("unix", o.sockPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	// Convert to *os.File for ExtraFiles. On Unix, UnixConn supports File().
	if uc, ok := c.(*net.UnixConn); ok {
		f, err := uc.File()
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		// File() duplicates; close the conn copy.
		_ = c.Close()
		return f, nil
	}
	_ = c.Close()
	return nil, fmt.Errorf("oracle: unix conn File() unavailable")
}

// Generation returns restart generation counter.
func (o *HostCredsOracle) Generation() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.generation
}

// LastUpstreamAuth returns the last Authorization the oracle attached (test only).
func (o *HostCredsOracle) LastUpstreamAuth() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastUpstreamAuth
}

// Alive reports whether the session is usable (not closed/revoked/expired).
func (o *HostCredsOracle) Alive() error {
	if o == nil {
		return &BlockedError{Reason: BlockNoSession, Detail: "nil oracle"}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return &BlockedError{Reason: BlockNoSession, SessionID: o.SessionID, Detail: "oracle closed"}
	}
	if o.revoked {
		return &BlockedError{Reason: BlockRevoked, SessionID: o.SessionID, Detail: "session revoked"}
	}
	if time.Now().After(o.ExpiresAt) {
		return &BlockedError{Reason: BlockExpired, SessionID: o.SessionID, Detail: "session expired"}
	}
	return nil
}

// Revoke immediately invalidates the session (fails closed for further requests).
func (o *HostCredsOracle) Revoke() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	o.revoked = true
	o.mu.Unlock()
	return o.Close()
}

// RotateHostCredential updates the out-of-band store (never worker-visible).
func (o *HostCredsOracle) RotateHostCredential(host, authorization string) error {
	if o == nil || o.store == nil {
		return fmt.Errorf("nil oracle/store")
	}
	if IsDummyCredential(authorization) {
		return &BlockedError{Reason: BlockDummyUpstream, Detail: "cannot store dummy as real HostCreds"}
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if !o.hostAllowed(host) {
		return &BlockedError{Reason: BlockHostDenied, SessionID: o.SessionID, Detail: "host not allowlisted"}
	}
	return o.store.Set(host, authorization)
}

// RevokeHostCredential removes host creds from the out-of-band store.
func (o *HostCredsOracle) RevokeHostCredential(host string) error {
	if o == nil || o.store == nil {
		return fmt.Errorf("nil oracle/store")
	}
	return o.store.Delete(host)
}

// HostCredentialPresent reports presence only (no value).
func (o *HostCredsOracle) HostCredentialPresent(host string) bool {
	if o == nil || o.store == nil {
		return false
	}
	return strings.TrimSpace(o.store.Get(host)) != ""
}

// CredHosts returns host names with credentials (never values).
func (o *HostCredsOracle) CredHosts() []string {
	if o == nil || o.store == nil {
		return nil
	}
	return o.store.Hosts()
}

// Close shuts down the oracle and removes the socket.
func (o *HostCredsOracle) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	ln := o.ln
	o.ln = nil
	path := o.sockPath
	o.mu.Unlock()
	var first error
	if ln != nil {
		if err := ln.Close(); err != nil {
			first = err
		}
	}
	if path != "" {
		_ = os.Remove(path)
	}
	return first
}

// Restart re-listens on a new socket generation; secrets re-seed from store only.
func (o *HostCredsOracle) Restart() error {
	if o == nil {
		return fmt.Errorf("nil oracle")
	}
	o.mu.Lock()
	if o.revoked {
		o.mu.Unlock()
		return &BlockedError{Reason: BlockRevoked, SessionID: o.SessionID, Detail: "cannot restart revoked session"}
	}
	if time.Now().After(o.ExpiresAt) {
		o.mu.Unlock()
		return &BlockedError{Reason: BlockExpired, SessionID: o.SessionID, Detail: "cannot restart expired session"}
	}
	oldLn := o.ln
	oldPath := o.sockPath
	dir := o.sockDir
	sid := o.SessionID
	o.generation++
	gen := o.generation
	o.mu.Unlock()

	if oldLn != nil {
		_ = oldLn.Close()
	}
	if oldPath != "" {
		_ = os.Remove(oldPath)
	}
	// Short path for AF_UNIX sun_path limits (macOS ~104 bytes).
	sockPath := filepath.Join(dir, fmt.Sprintf("o%d.sock", gen))
	_ = sid // session binding is protocol-level, not path-level
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(sockPath, 0o600)
	o.mu.Lock()
	o.ln = ln
	o.sockPath = sockPath
	o.closed = false
	o.mu.Unlock()
	go o.serve()
	return nil
}

// Execute handles one oracle request in-process (used by tests and FD clients).
// This is the sole signing path: validate → attach secret → forward → redact errors.
func (o *HostCredsOracle) Execute(req OracleRequest) OracleResponse {
	if err := o.Alive(); err != nil {
		return oracleErr(err)
	}
	// Session binding: request session_id must match oracle (empty allowed only
	// on pre-opened exclusive FD channels where the FD itself binds the session).
	if req.SessionID != "" && req.SessionID != o.SessionID {
		return oracleErr(&BlockedError{
			Reason: BlockAbuse, SessionID: o.SessionID,
			Detail: "session_id mismatch",
		})
	}
	host := strings.ToLower(strings.TrimSpace(req.Host))
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := req.Path
	if path == "" {
		path = "/"
	}
	if !o.hostAllowed(host) {
		return oracleErr(&BlockedError{
			Reason: BlockHostDenied, SessionID: o.SessionID,
			Detail: "host not allowlisted",
		})
	}
	rule := MatchRequestRule(o.Rules, host, method, path)
	if rule == nil {
		// Distinguish method vs path denial for clearer BLOCKED packets.
		if hostMethodAllowed(o.Rules, host, method) {
			return oracleErr(&BlockedError{Reason: BlockPathDenied, SessionID: o.SessionID, Detail: "path not allowlisted"})
		}
		if hostPathAllowed(o.Rules, host, path) {
			return oracleErr(&BlockedError{Reason: BlockMethodDenied, SessionID: o.SessionID, Detail: "method not allowlisted"})
		}
		return oracleErr(&BlockedError{Reason: BlockActionDenied, SessionID: o.SessionID, Detail: "request not allowlisted"})
	}
	if req.Action != "" && rule.Action != "" && req.Action != rule.Action {
		return oracleErr(&BlockedError{Reason: BlockActionDenied, SessionID: o.SessionID, Detail: "action mismatch"})
	}
	if len(req.Body) > MaxOracleBodyBytes {
		return oracleErr(&BlockedError{Reason: BlockAbuse, SessionID: o.SessionID, Detail: "body too large"})
	}

	// Worker Authorization handling: only dummy sentinel allowed; real secrets
	// from worker are rejected (injection / confused-deputy).
	workerAuth := ""
	if req.Headers != nil {
		for k, v := range req.Headers {
			if strings.EqualFold(k, "Authorization") {
				workerAuth = v
				break
			}
		}
	}
	if workerAuth != "" && !IsDummyCredential(workerAuth) {
		return oracleErr(&BlockedError{
			Reason: BlockWorkerAuthInject, SessionID: o.SessionID,
			Detail: "worker must not supply real Authorization; use dummy sentinel only",
		})
	}

	// Resolve out-of-band secret. Dummy must never be used upstream.
	secret := strings.TrimSpace(o.store.Get(host))
	if secret == "" {
		return oracleErr(&BlockedError{
			Reason: BlockMissingCreds, SessionID: o.SessionID,
			HostsRequired: []string{host}, HostsCreds: o.CredHosts(),
			Detail: "no HostCreds for host in out-of-band store",
		})
	}
	if IsDummyCredential(secret) {
		return oracleErr(&BlockedError{
			Reason: BlockDummyUpstream, SessionID: o.SessionID,
			Detail: "store contains dummy sentinel — refusing upstream",
		})
	}

	if o.CaptureUpstreamAuth {
		o.mu.Lock()
		o.lastUpstreamAuth = secret
		o.mu.Unlock()
	}

	// Forward without following redirects; pin DNS; never put secret in errors.
	status, body, err := o.forward(host, method, path, secret, req.Headers, req.Body)
	if err != nil {
		return OracleResponse{
			OK:        false,
			SessionID: o.SessionID,
			Error:     RedactSecrets(err.Error()),
		}
	}
	return OracleResponse{
		OK:         true,
		StatusCode: status,
		Body:       body,
		Action:     rule.Action,
		SessionID:  o.SessionID,
	}
}

func (o *HostCredsOracle) forward(host, method, path, authorization string, headers map[string]string, body string) (int, string, error) {
	// DNS pin — reject private/rebind for non-loopback hosts.
	var ip net.IP
	var err error
	if o.resolveHook != nil {
		ip, err = o.resolveHook(host)
	} else {
		ip, err = resolveAndPinIP(host)
	}
	if err != nil {
		// Allow explicit loopback only when host is a loopback IP string and allowlisted.
		if parsed := net.ParseIP(host); parsed != nil && parsed.IsLoopback() && o.hostAllowed(host) {
			ip = parsed
		} else {
			return 0, "", fmt.Errorf("dns denied")
		}
	}
	// Always validate resolved IP (hooks cannot smuggle private/rebind targets).
	allowLoop := net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	if err := validateDialIP(ip, allowLoop || o.forceHTTP); err != nil {
		return 0, "", fmt.Errorf("dns rebind denied")
	}

	// Prefer HTTPS to real providers; loopback tests use HTTP via dialHook/forceHTTP.
	useTLS := !o.forceHTTP && !(ip != nil && ip.IsLoopback())
	port := "443"
	if !useTLS {
		port = "80"
	}
	addr := net.JoinHostPort(ip.String(), port)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var conn net.Conn
	if o.dialHook != nil {
		conn, err = o.dialHook("tcp", addr)
	} else {
		d := &net.Dialer{Timeout: 5 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return 0, "", fmt.Errorf("upstream dial failed")
	}
	defer conn.Close()

	var rw io.ReadWriter = conn
	if useTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, "", fmt.Errorf("tls handshake failed")
		}
		defer tlsConn.Close()
		rw = tlsConn
	}

	// Build request: NEVER copy worker Authorization.
	uPath := path
	if !strings.HasPrefix(uPath, "/") {
		uPath = "/" + uPath
	}
	scheme := "https"
	if !useTLS {
		scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+host+uPath, strings.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("build request failed")
	}
	req.Host = host
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	// Copy safe worker headers only (no Authorization, no Cookie, no Proxy-*).
	for k, v := range headers {
		kl := strings.ToLower(k)
		switch kl {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "host":
			continue
		default:
			if strings.TrimSpace(v) != "" {
				req.Header.Set(k, v)
			}
		}
	}

	// Manual write/read so we can refuse redirects without following them.
	if err := req.Write(rw); err != nil {
		return 0, "", fmt.Errorf("upstream write failed")
	}
	br := bufio.NewReader(rw)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return 0, "", fmt.Errorf("upstream read failed")
	}
	defer resp.Body.Close()

	// Do NOT follow redirects — Location could point at attacker with Authorization
	// re-sent by a naive client. Strip sensitive headers from any error path.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Return status without Location body that might echo secrets.
		return resp.StatusCode, "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxOracleBodyBytes))
	if err != nil {
		return 0, "", fmt.Errorf("upstream body read failed")
	}
	// Never return bodies that echo Authorization (paranoid redaction).
	out := RedactSecrets(string(raw))
	if strings.Contains(out, authorization) {
		out = strings.ReplaceAll(out, authorization, "[REDACTED]")
	}
	return resp.StatusCode, out, nil
}

func (o *HostCredsOracle) hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, h := range o.Hosts {
		if h == host {
			return true
		}
	}
	return false
}

func (o *HostCredsOracle) serve() {
	for {
		o.mu.Lock()
		ln := o.ln
		o.mu.Unlock()
		if ln == nil {
			return
		}
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go o.handleConn(c)
	}
}

func (o *HostCredsOracle) handleConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	// HTTP/1.1 over the Unix socket — single request, no persistent proxy semantics.
	br := bufio.NewReader(c)
	httpReq, err := http.ReadRequest(br)
	if err != nil {
		// Also accept raw JSON line (length-prefixed free form for FD clients).
		return
	}
	writeJSON := func(code int, v any) {
		raw, _ := json.Marshal(v)
		status := "OK"
		if code >= 400 {
			status = "ERR"
		}
		_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			code, status, len(raw), raw)
	}

	// Only the oracle endpoint is served. No CONNECT. No arbitrary proxying.
	if httpReq.Method != http.MethodPost || httpReq.URL.Path != "/v1/oracle" {
		writeJSON(403, OracleResponse{OK: false, Error: "only POST /v1/oracle allowed", SessionID: o.SessionID})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(httpReq.Body, MaxOracleBodyBytes+1024))
	if err != nil {
		writeJSON(400, OracleResponse{OK: false, Error: "bad body", SessionID: o.SessionID})
		return
	}
	var req OracleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(400, OracleResponse{OK: false, Error: "bad json", SessionID: o.SessionID})
		return
	}
	// Default session bind when omitted on exclusive socket.
	if req.SessionID == "" {
		req.SessionID = o.SessionID
	}
	resp := o.Execute(req)
	code := 200
	if !resp.OK {
		code = 403
	}
	writeJSON(code, resp)
}

// CallOracle is a helper for tests/clients: POST intent over the Unix socket.
// No bearer token is used — channel access is the authority boundary.
func CallOracle(sockPath string, req OracleRequest) (OracleResponse, error) {
	c, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return OracleResponse{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	payload, _ := json.Marshal(req)
	_, err = fmt.Fprintf(c, "POST /v1/oracle HTTP/1.1\r\nHost: hostcreds\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(payload), payload)
	if err != nil {
		return OracleResponse{}, err
	}
	br := bufio.NewReader(c)
	httpResp, err := http.ReadResponse(br, nil)
	if err != nil {
		return OracleResponse{}, err
	}
	defer httpResp.Body.Close()
	body, _ := io.ReadAll(httpResp.Body)
	var out OracleResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return OracleResponse{}, fmt.Errorf("oracle response: %w", err)
	}
	return out, nil
}

func oracleErr(err error) OracleResponse {
	if be, ok := err.(*BlockedError); ok {
		return OracleResponse{
			OK:        false,
			SessionID: be.SessionID,
			Error:     RedactSecrets(be.Error()),
		}
	}
	return OracleResponse{OK: false, Error: RedactSecrets(err.Error())}
}

func hostSetFromRules(rules []RequestRule) []string {
	var out []string
	for _, r := range rules {
		out = appendUnique(out, strings.ToLower(r.Host))
	}
	return out
}

func appendUnique(ss []string, v string) []string {
	for _, s := range ss {
		if s == v {
			return ss
		}
	}
	return append(ss, v)
}

func hostMethodAllowed(rules []RequestRule, host, method string) bool {
	for _, r := range rules {
		if strings.EqualFold(r.Host, host) && (r.Method == "" || strings.EqualFold(r.Method, method)) {
			return true
		}
	}
	return false
}

func hostPathAllowed(rules []RequestRule, host, path string) bool {
	for _, r := range rules {
		if !strings.EqualFold(r.Host, host) {
			continue
		}
		prefix := r.PathPrefix
		if path == prefix || strings.HasPrefix(path, prefix+"/") || strings.HasPrefix(path, prefix+"?") {
			return true
		}
	}
	return false
}

func sanitizeSessionFile(sid string) string {
	var b strings.Builder
	for _, r := range sid {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		return "sess"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess-%d", os.Getpid())
	}
	return "sess-" + hex.EncodeToString(b[:])
}

// DirectProviderHosts is the set of hosts the worker must not reach directly.
// Coordinator sandbox policy should deny these in the worker network namespace.
func DirectProviderHosts() []string {
	return DefaultHostAllowlist()
}
