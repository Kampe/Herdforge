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
// Error is a stable reason code string only (not free-form secret material).
type OracleResponse struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
	Error      string `json:"error,omitempty"` // stable code only
	Action     string `json:"action,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

// HostCredsOracle is the session-bound signing authority.
// Secrets are injected only via CredentialAuthority.InjectAuthorization.
type HostCredsOracle struct {
	SessionID string
	Kind      string
	Rules     []RequestRule
	Hosts     []string
	ExpiresAt time.Time

	mu         sync.Mutex
	authority  CredentialAuthority
	revoked    bool
	closed     bool
	ln         net.Listener
	sockPath   string
	sockDir    string
	generation int

	// Test hooks only.
	CaptureInjected bool
	lastInjected    bool // true if InjectAuthorization succeeded (not the secret)
	dialHook        func(network, addr string) (net.Conn, error)
	resolveHook     func(host string) (net.IP, error)
	// tlsConfig for upstream client (tests inject RootCAs for loopback TLS).
	// Production leaves nil (system roots). Credentialed path NEVER uses plaintext HTTP.
	upstreamTLS *tls.Config
	allowLoopback bool
}

// OracleConfig configures a HostCredsOracle.
type OracleConfig struct {
	SessionID     string
	Kind          string
	Authority     CredentialAuthority
	Rules         []RequestRule
	TTL           time.Duration
	SocketDir     string
	AllowLoopback bool // tests only
}

// StartHostCredsOracle creates a session-bound oracle on a Unix socket.
func StartHostCredsOracle(cfg OracleConfig) (*HostCredsOracle, error) {
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}
	if cfg.Authority == nil {
		return nil, &BlockedError{Reason: BlockMissingCreds, Code: "authority_required"}
	}
	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	rules := cfg.Rules
	if len(rules) == 0 {
		rules = RequestRulesForKind(kind)
	}
	if len(rules) == 0 {
		return nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "no_rules", Kind: kind}
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	sockDir := cfg.SocketDir
	if sockDir == "" {
		var err error
		sockDir, err = os.MkdirTemp("", "hc-*")
		if err != nil {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(sockDir, 0o700); err != nil {
			return nil, err
		}
		_ = os.Chmod(sockDir, 0o700)
	}
	sockPath := filepath.Join(sockDir, "o.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("oracle unix listen: %w", err)
	}
	_ = os.Chmod(sockPath, 0o600)

	hosts := hostSetFromRules(rules)
	o := &HostCredsOracle{
		SessionID:     sid,
		Kind:          kind,
		Rules:         append([]RequestRule(nil), rules...),
		Hosts:         hosts,
		ExpiresAt:     time.Now().Add(ttl),
		authority:     cfg.Authority,
		ln:            ln,
		sockPath:      sockPath,
		sockDir:       sockDir,
		generation:    1,
		allowLoopback: cfg.AllowLoopback,
	}
	go o.serve()
	return o, nil
}

func (o *HostCredsOracle) SocketPath() string {
	if o == nil {
		return ""
	}
	return o.sockPath
}

func (o *HostCredsOracle) File() (*os.File, error) {
	if o == nil || o.ln == nil {
		return nil, fmt.Errorf("oracle not listening")
	}
	c, err := net.DialTimeout("unix", o.sockPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	if uc, ok := c.(*net.UnixConn); ok {
		f, err := uc.File()
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		_ = c.Close()
		return f, nil
	}
	_ = c.Close()
	return nil, fmt.Errorf("oracle: unix conn File() unavailable")
}

func (o *HostCredsOracle) Generation() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.generation
}

func (o *HostCredsOracle) Alive() error {
	if o == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil_oracle"}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return &BlockedError{Reason: BlockNoSession, SessionID: o.SessionID, Code: "closed"}
	}
	if o.revoked {
		return &BlockedError{Reason: BlockRevoked, SessionID: o.SessionID, Code: "revoked"}
	}
	if time.Now().After(o.ExpiresAt) {
		return &BlockedError{Reason: BlockExpired, SessionID: o.SessionID, Code: "expired"}
	}
	return nil
}

func (o *HostCredsOracle) Revoke() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	o.revoked = true
	o.mu.Unlock()
	return o.Close()
}

func (o *HostCredsOracle) RotateFromHandle(host, handle string) error {
	if o == nil || o.authority == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil_oracle"}
	}
	return o.authority.RotateFromHandle(host, handle)
}

func (o *HostCredsOracle) RevokeHost(host string) error {
	if o == nil || o.authority == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil_oracle"}
	}
	return o.authority.Revoke(host)
}

func (o *HostCredsOracle) HostCredentialPresent(host string) bool {
	if o == nil || o.authority == nil {
		return false
	}
	return o.authority.Has(host)
}

func (o *HostCredsOracle) CredHosts() []string {
	if o == nil || o.authority == nil {
		return nil
	}
	return o.authority.Hosts()
}

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

// Restart re-binds the channel and re-resolves durable handles when available.
func (o *HostCredsOracle) Restart() error {
	if o == nil {
		return &BlockedError{Reason: BlockNoSession, Code: "nil_oracle"}
	}
	o.mu.Lock()
	if o.revoked {
		o.mu.Unlock()
		return &BlockedError{Reason: BlockRevoked, SessionID: o.SessionID, Code: "revoked"}
	}
	if time.Now().After(o.ExpiresAt) {
		o.mu.Unlock()
		return &BlockedError{Reason: BlockExpired, SessionID: o.SessionID, Code: "expired"}
	}
	oldLn := o.ln
	oldPath := o.sockPath
	dir := o.sockDir
	o.generation++
	gen := o.generation
	auth := o.authority
	o.mu.Unlock()

	// Durable authorities re-resolve from handles (secret never from worker).
	if auth != nil && auth.Durable() {
		if ha, ok := auth.(*HandleAuthority); ok {
			if err := ha.ReResolveAll(); err != nil {
				return err
			}
		}
	}

	if oldLn != nil {
		_ = oldLn.Close()
	}
	if oldPath != "" {
		_ = os.Remove(oldPath)
	}
	sockPath := filepath.Join(dir, fmt.Sprintf("o%d.sock", gen))
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

// Execute is the sole signing path.
func (o *HostCredsOracle) Execute(req OracleRequest) OracleResponse {
	if err := o.Alive(); err != nil {
		return oracleErr(err)
	}
	if req.SessionID != "" && req.SessionID != o.SessionID {
		return oracleErr(&BlockedError{Reason: BlockAbuse, SessionID: o.SessionID, Code: "session_mismatch"})
	}

	host, err := NormalizeHost(req.Host, o.allowLoopback)
	if err != nil {
		return oracleErr(err)
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path, err := normalizePathForMatch(req.Path)
	if err != nil {
		return oracleErr(err)
	}
	if !o.hostAllowed(host) {
		return oracleErr(&BlockedError{Reason: BlockHostDenied, SessionID: o.SessionID, Code: "host:" + host})
	}
	rule := MatchRequestRule(o.Rules, host, method, path)
	if rule == nil {
		if hostMethodAllowed(o.Rules, host, method) {
			return oracleErr(&BlockedError{Reason: BlockPathDenied, SessionID: o.SessionID, Code: "path"})
		}
		if hostPathAllowed(o.Rules, host, path) {
			return oracleErr(&BlockedError{Reason: BlockMethodDenied, SessionID: o.SessionID, Code: "method"})
		}
		return oracleErr(&BlockedError{Reason: BlockActionDenied, SessionID: o.SessionID, Code: "rule"})
	}
	if req.Action != "" && rule.Action != "" && req.Action != rule.Action {
		return oracleErr(&BlockedError{Reason: BlockActionDenied, SessionID: o.SessionID, Code: "action"})
	}
	if len(req.Body) > MaxOracleBodyBytes {
		return oracleErr(&BlockedError{Reason: BlockAbuse, SessionID: o.SessionID, Code: "body_too_large"})
	}

	// Worker Authorization: only dummy sentinel allowed.
	if req.Headers != nil {
		for k, v := range req.Headers {
			if strings.EqualFold(k, "Authorization") {
				if v != "" && !IsDummyCredential(v) {
					return oracleErr(&BlockedError{Reason: BlockWorkerAuthInject, SessionID: o.SessionID, Code: "worker_auth"})
				}
				// Reject CRLF even on dummy path if malformed.
				if strings.ContainsAny(v, "\r\n\x00") {
					return oracleErr(&BlockedError{Reason: BlockBadAuthMaterial, SessionID: o.SessionID, Code: "auth_header_injection"})
				}
			}
			// Header injection via any worker header name/value.
			if strings.ContainsAny(k, "\r\n\x00") || strings.ContainsAny(v, "\r\n\x00") {
				return oracleErr(&BlockedError{Reason: BlockAbuse, SessionID: o.SessionID, Code: "header_injection"})
			}
		}
	}

	// Build upstream headers; inject via authority (never returns secret).
	upHdr := make(http.Header)
	if err := o.authority.InjectAuthorization(host, upHdr); err != nil {
		return oracleErr(err)
	}
	if o.CaptureInjected {
		o.mu.Lock()
		o.lastInjected = upHdr.Get("Authorization") != ""
		o.mu.Unlock()
	}

	// Copy safe worker headers (never Authorization).
	for k, v := range req.Headers {
		kl := strings.ToLower(k)
		switch kl {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "host":
			continue
		default:
			if strings.TrimSpace(v) != "" {
				upHdr.Set(k, v)
			}
		}
	}

	status, body, ferr := o.forward(host, method, path, upHdr, req.Body)
	if ferr != nil {
		return OracleResponse{OK: false, SessionID: o.SessionID, Error: blockCode(ferr)}
	}
	// Redact any accidental secret echo in body without knowing the secret:
	// strip Bearer tokens shape.
	body = RedactSecrets(body)
	return OracleResponse{
		OK:         true,
		StatusCode: status,
		Body:       body,
		Action:     rule.Action,
		SessionID:  o.SessionID,
	}
}

func (o *HostCredsOracle) forward(host, method, path string, hdr http.Header, body string) (int, string, error) {
	var ip net.IP
	var err error
	if o.resolveHook != nil {
		ip, err = o.resolveHook(host)
	} else {
		ip, err = resolveAndPinIP(host)
	}
	if err != nil {
		if parsed := net.ParseIP(host); parsed != nil && parsed.IsLoopback() && o.allowLoopback {
			ip = parsed
		} else {
			return 0, "", &BlockedError{Reason: BlockAbuse, Code: "dns_denied"}
		}
	}
	allowLoop := o.allowLoopback || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	if err := validateDialIP(ip, allowLoop); err != nil {
		return 0, "", &BlockedError{Reason: BlockAbuse, Code: "dns_rebind"}
	}

	// Credentialed traffic: TLS only, exact port 443 (loopback tests still TLS on
	// the dialHook target — never inject Authorization over plaintext HTTP).
	port := "443"
	addr := net.JoinHostPort(ip.String(), port)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var conn net.Conn
	if o.dialHook != nil {
		// dialHook may target a TLS test server on an ephemeral port; still TLS.
		conn, err = o.dialHook("tcp", addr)
	} else {
		d := &net.Dialer{Timeout: 5 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return 0, "", &BlockedError{Reason: BlockAbuse, Code: "upstream_dial"}
	}
	defer conn.Close()

	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if o.upstreamTLS != nil {
		tlsCfg = o.upstreamTLS.Clone()
		if tlsCfg.ServerName == "" {
			tlsCfg.ServerName = host
		}
		if tlsCfg.MinVersion == 0 {
			tlsCfg.MinVersion = tls.VersionTLS12
		}
	}
	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return 0, "", &BlockedError{Reason: BlockAbuse, Code: "tls_handshake"}
	}
	defer tlsConn.Close()
	rw := io.ReadWriter(tlsConn)

	uPath := path
	if !strings.HasPrefix(uPath, "/") {
		uPath = "/" + uPath
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host+uPath, strings.NewReader(body))
	if err != nil {
		return 0, "", &BlockedError{Reason: BlockAbuse, Code: "build_request"}
	}
	req.Host = host
	req.Header = hdr.Clone()
	// Ensure Content-Type default for JSON APIs.
	if req.Header.Get("Content-Type") == "" && body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	if err := req.Write(rw); err != nil {
		return 0, "", &BlockedError{Reason: BlockAbuse, Code: "upstream_write"}
	}
	br := bufio.NewReader(rw)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return 0, "", &BlockedError{Reason: BlockAbuse, Code: "upstream_read"}
	}
	defer resp.Body.Close()

	// Do NOT follow redirects — prevents auth exfil via Location.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxOracleBodyBytes))
	if err != nil {
		return 0, "", &BlockedError{Reason: BlockAbuse, Code: "upstream_body"}
	}
	return resp.StatusCode, string(raw), nil
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
	br := bufio.NewReader(c)
	httpReq, err := http.ReadRequest(br)
	if err != nil {
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
	if httpReq.Method != http.MethodPost || httpReq.URL.Path != "/v1/oracle" {
		writeJSON(403, OracleResponse{OK: false, Error: "only_post_oracle", SessionID: o.SessionID})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(httpReq.Body, MaxOracleBodyBytes+1024))
	if err != nil {
		writeJSON(400, OracleResponse{OK: false, Error: "bad_body", SessionID: o.SessionID})
		return
	}
	var req OracleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(400, OracleResponse{OK: false, Error: "bad_json", SessionID: o.SessionID})
		return
	}
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

// CallOracle POSTs intent over the Unix socket (no bearer token).
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
		return OracleResponse{}, fmt.Errorf("oracle response decode")
	}
	return out, nil
}

func oracleErr(err error) OracleResponse {
	return OracleResponse{OK: false, Error: blockCode(err), SessionID: sessionOf(err)}
}

func blockCode(err error) string {
	if be, ok := err.(*BlockedError); ok {
		if be.Code != "" {
			return string(be.Reason) + ":" + be.Code
		}
		return string(be.Reason)
	}
	return "error"
}

func sessionOf(err error) string {
	if be, ok := err.(*BlockedError); ok {
		return be.SessionID
	}
	return ""
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
		if strings.EqualFold(r.Host, host) && strings.EqualFold(r.Method, method) {
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
		if r.PathExact != "" && path == r.PathExact {
			return true
		}
		if r.PathPrefix != "" && (path == r.PathPrefix || strings.HasPrefix(path, r.PathPrefix+"/")) {
			return true
		}
	}
	return false
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess-%d", os.Getpid())
	}
	return "sess-" + hex.EncodeToString(b[:])
}
