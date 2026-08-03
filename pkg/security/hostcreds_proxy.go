package security

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HostCredsProxy is a localhost HTTP CONNECT proxy that:
//   - allows only allowlisted hosts
//   - injects coordinator-held Authorization for HostCreds hosts (MITM)
//   - never exposes credential bytes on the agent-visible proxy token path
//   - serves a separate control listener for coordinator rotate/revoke/inject
type HostCredsProxy struct {
	AllowHosts   []string
	Token        string // agent-visible proxy token (NOT a model secret)
	ControlToken string // coordinator-only
	SessionID    string

	mu         sync.Mutex
	hostCreds  map[string]string // host → Authorization (broker memory only)
	revoked    map[string]bool
	ln         net.Listener
	ctrlLn     net.Listener
	addr       string
	ctrlAddr   string
	closed     bool
	ca         *brokerCA
	generation int // bumps on restart/rotate for causal binding
}

// StartHostCredsProxy listens on 127.0.0.1:0 with the given host allowlist.
func StartHostCredsProxy(allowHosts []string, sessionID string) (*HostCredsProxy, error) {
	if err := platformSupportsHostCredsBroker(); err != nil {
		return nil, err
	}
	proxyTok, err := newBrokerToken()
	if err != nil {
		return nil, err
	}
	ctrlTok, err := newBrokerToken()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("hostcreds proxy listen: %w", err)
	}
	ctrlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("hostcreds control listen: %w", err)
	}
	p := &HostCredsProxy{
		AllowHosts:   append([]string(nil), allowHosts...),
		Token:        proxyTok,
		ControlToken: ctrlTok,
		SessionID:    sessionID,
		hostCreds:    map[string]string{},
		revoked:      map[string]bool{},
		ln:           ln,
		ctrlLn:       ctrlLn,
		addr:         ln.Addr().String(),
		ctrlAddr:     ctrlLn.Addr().String(),
		generation:   1,
	}
	go p.serveProxy()
	go p.serveControl()
	return p, nil
}

// Addr returns the proxy listen address.
func (p *HostCredsProxy) Addr() string {
	if p == nil {
		return ""
	}
	return p.addr
}

// ControlAddr returns the coordinator control listen address.
func (p *HostCredsProxy) ControlAddr() string {
	if p == nil {
		return ""
	}
	return p.ctrlAddr
}

// ProxyURL embeds proxy token only (never ControlToken or model secrets).
func (p *HostCredsProxy) ProxyURL() string {
	if p == nil {
		return ""
	}
	u := &url.URL{
		Scheme: "http",
		User:   url.UserPassword("herd", p.Token),
		Host:   p.Addr(),
	}
	return u.String()
}

// WorkerProxyEnv returns env vars safe to hand a worker process.
// Never includes model credentials or control token.
func (p *HostCredsProxy) WorkerProxyEnv() []string {
	if p == nil {
		return nil
	}
	u := p.ProxyURL()
	return []string{
		"HTTP_PROXY=" + u,
		"HTTPS_PROXY=" + u,
		"http_proxy=" + u,
		"https_proxy=" + u,
		"HERD_NETWORK_BROKER=" + u,
		"HERD_HOSTCREDS_SESSION=" + p.SessionID,
		// Explicitly scrub common model key names from inheritance risk:
		// workers should never see these even if parent had them.
		"ANTHROPIC_API_KEY=",
		"OPENAI_API_KEY=",
		"XAI_API_KEY=",
		"HERD_HOST_CREDS=",
	}
}

// CAPEM returns the public CA PEM for agent TLS trust (not secret).
func (p *HostCredsProxy) CAPEM() []byte {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ca == nil {
		return nil
	}
	return append([]byte(nil), p.ca.certPEM...)
}

// EnsureCA initializes the ephemeral MITM CA.
func (p *HostCredsProxy) EnsureCA() error {
	if p == nil {
		return fmt.Errorf("nil proxy")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ca != nil {
		return nil
	}
	ca, err := newBrokerCA()
	if err != nil {
		return err
	}
	p.ca = ca
	return nil
}

// Generation returns the broker generation (increments on restart).
func (p *HostCredsProxy) Generation() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.generation
}

// SetHostCredential installs Authorization for host (coordinator only).
// Denied if host is not on the allowlist. Never written to worker env.
func (p *HostCredsProxy) SetHostCredential(host, authorization string) error {
	if p == nil {
		return fmt.Errorf("nil proxy")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	authorization = strings.TrimSpace(authorization)
	if host == "" || authorization == "" {
		return fmt.Errorf("host and authorization required")
	}
	if !p.HostAllowed(host) {
		return &BlockedError{
			Reason:    BlockHostDenied,
			SessionID: p.SessionID,
			Detail:    fmt.Sprintf("host %q not on allowlist", host),
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hostCreds == nil {
		p.hostCreds = map[string]string{}
	}
	p.hostCreds[host] = authorization
	delete(p.revoked, host)
	if p.ca == nil {
		if ca, err := newBrokerCA(); err == nil {
			p.ca = ca
		}
	}
	return nil
}

// RotateHostCredential replaces Authorization for host (old value discarded).
func (p *HostCredsProxy) RotateHostCredential(host, newAuthorization string) error {
	return p.SetHostCredential(host, newAuthorization)
}

// RevokeHostCredential removes credentials for host; subsequent inject is empty.
func (p *HostCredsProxy) RevokeHostCredential(host string) error {
	if p == nil {
		return fmt.Errorf("nil proxy")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.hostCreds, host)
	if p.revoked == nil {
		p.revoked = map[string]bool{}
	}
	p.revoked[host] = true
	return nil
}

// HostCredentialPresent reports whether host has a non-empty credential (no value).
func (p *HostCredsProxy) HostCredentialPresent(host string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.TrimSpace(p.hostCreds[strings.ToLower(host)]) != ""
}

// CredHosts returns hosts with credentials present (names only).
func (p *HostCredsProxy) CredHosts() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.hostCreds))
	for h, v := range p.hostCreds {
		if strings.TrimSpace(v) != "" {
			out = append(out, h)
		}
	}
	return out
}

// hostCred returns coordinator-held Authorization (case-insensitive). Empty if revoked/missing.
func (p *HostCredsProxy) hostCred(host string) string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := strings.ToLower(host)
	if p.revoked[h] {
		return ""
	}
	return p.hostCreds[h]
}

// HostAllowed reports whether hostport is on the allowlist.
func (p *HostCredsProxy) HostAllowed(hostport string) bool {
	if p == nil {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, a := range p.AllowHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// SeedFromStore copies secrets from out-of-band store into broker memory.
// Only allowlisted hosts are accepted.
func (p *HostCredsProxy) SeedFromStore(store SecretStore) error {
	if p == nil || store == nil {
		return fmt.Errorf("proxy and store required")
	}
	for h, a := range store.Snapshot() {
		if !p.HostAllowed(h) {
			continue
		}
		if err := p.SetHostCredential(h, a); err != nil {
			return err
		}
	}
	return nil
}

// Close stops proxy and control listeners.
func (p *HostCredsProxy) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	// Wipe credential material on close (defense in depth).
	for h := range p.hostCreds {
		p.hostCreds[h] = ""
		delete(p.hostCreds, h)
	}
	var first error
	if p.ln != nil {
		if err := p.ln.Close(); err != nil && first == nil {
			first = err
		}
	}
	if p.ctrlLn != nil {
		if err := p.ctrlLn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (p *HostCredsProxy) serveProxy() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handleProxy(c)
	}
}

func (p *HostCredsProxy) serveControl() {
	for {
		c, err := p.ctrlLn.Accept()
		if err != nil {
			return
		}
		go p.handleControl(c)
	}
}

func (p *HostCredsProxy) proxyAuthorized(req *http.Request) bool {
	if p.Token == "" {
		return false
	}
	user, pass, bearer, ok := extractBasicOrBearer(req)
	if !ok {
		return false
	}
	if bearer != "" {
		return bearer == p.Token
	}
	return user == "herd" && pass == p.Token
}

func (p *HostCredsProxy) controlAuthorized(req *http.Request) bool {
	if p.ControlToken == "" {
		return false
	}
	user, pass, bearer, ok := extractBasicOrBearer(req)
	if !ok {
		return false
	}
	if bearer != "" {
		return bearer == p.ControlToken
	}
	return user == "herd-ctrl" && pass == p.ControlToken
}

func (p *HostCredsProxy) handleProxy(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	// Control paths never served on agent proxy.
	if isControlPath(req) {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	if !p.proxyAuthorized(req) {
		_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nConnection: close\r\n\r\n")
		return
	}
	target := req.Host
	if target == "" && req.URL != nil {
		target = req.URL.Host
	}
	if req.Method == http.MethodConnect {
		_ = p.dialAllowed(c, target)
		return
	}
	// Absolute-form HTTP forward (used by tests / some clients).
	if req.URL == nil || req.URL.Scheme == "" {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return
	}
	host := req.URL.Host
	if !p.HostAllowed(host) {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	hOnly, port, err := net.SplitHostPort(host)
	if err != nil {
		hOnly = host
		if strings.EqualFold(req.URL.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	// Loopback absolute-form is allowed only when explicitly allowlisted
	// (tests seed 127.0.0.1). Production allowlist excludes it.
	dest := net.JoinHostPort(hOnly, port)
	if ip := net.ParseIP(hOnly); ip != nil && ip.IsLoopback() {
		// ok when allowlisted
	} else {
		resolved, rerr := resolveAndPinIP(hOnly)
		if rerr != nil {
			_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
			return
		}
		dest = net.JoinHostPort(resolved.String(), port)
	}
	outReq, err := http.NewRequest(req.Method, req.URL.String(), req.Body)
	if err != nil {
		return
	}
	outReq.Header = req.Header.Clone()
	outReq.Header.Del("Proxy-Authorization")
	outReq.Header.Del("Proxy-Connection")
	outReq.Host = hOnly
	if cred := p.hostCred(hOnly); cred != "" && outReq.Header.Get("Authorization") == "" {
		outReq.Header.Set("Authorization", cred)
	}
	// Rewrite URL to pinned dest while keeping Host header as original name.
	outReq.URL.Host = dest
	tr := &http.Transport{
		Proxy:   nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, dest)
		},
		// For HTTP (not TLS) tests against loopback.
		DisableKeepAlives: true,
	}
	if strings.EqualFold(req.URL.Scheme, "https") {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hOnly}
	}
	resp, err := tr.RoundTrip(outReq)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer resp.Body.Close()
	_ = resp.Write(c)
}

func (p *HostCredsProxy) dialAllowed(client net.Conn, target string) error {
	if !p.HostAllowed(target) {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return fmt.Errorf("forbidden")
	}
	host := target
	port := "443"
	if h, pt, err := net.SplitHostPort(target); err == nil {
		host, port = h, pt
	}
	// Credentialed MITM path when HostCreds present.
	if cred := p.hostCred(host); cred != "" {
		return p.dialCredentialed(client, host, port, cred)
	}
	// Transparent CONNECT for allowlisted hosts without HostCreds.
	ip, err := resolveAndPinIP(host)
	if err != nil {
		// Allow explicit loopback only when allowlisted and parseable as IP.
		if parsed := net.ParseIP(host); parsed != nil && parsed.IsLoopback() && p.HostAllowed(host) {
			ip = parsed
		} else {
			_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
			return err
		}
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), 5*time.Second)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return err
	}
	defer up.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return err
	}
	_ = client.SetDeadline(time.Time{})
	_ = up.SetDeadline(time.Time{})
	pipe(client, up)
	return nil
}

// dialCredentialed terminates TLS from agent (agent trusts broker CA), injects
// Authorization, opens TLS to upstream. HostCreds never appear in agent env.
func (p *HostCredsProxy) dialCredentialed(client net.Conn, host, port, authorization string) error {
	if err := p.EnsureCA(); err != nil {
		return err
	}
	p.mu.Lock()
	ca := p.ca
	p.mu.Unlock()
	leaf, err := ca.leafFor(host)
	if err != nil {
		return err
	}
	var upRaw net.Conn
	if parsed := net.ParseIP(host); parsed != nil && parsed.IsLoopback() {
		upRaw, err = net.DialTimeout("tcp", net.JoinHostPort(host, port), 5*time.Second)
	} else {
		ip, rerr := resolveAndPinIP(host)
		if rerr != nil {
			_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
			return rerr
		}
		upRaw, err = net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), 5*time.Second)
	}
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return err
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = upRaw.Close()
		return err
	}
	_ = client.SetDeadline(time.Time{})

	clientTLS := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
	})
	if err := clientTLS.Handshake(); err != nil {
		_ = upRaw.Close()
		return err
	}
	upTLS := tls.Client(upRaw, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		// For test loopback servers with self-signed certs we still use
		// InsecureSkipVerify only when host is loopback — production hosts
		// use normal verification against system roots.
		InsecureSkipVerify: net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback(),
	})
	if err := upTLS.Handshake(); err != nil {
		_ = clientTLS.Close()
		_ = upRaw.Close()
		return err
	}
	br := bufio.NewReader(clientTLS)
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return err
	}
	if req.Header.Get("Authorization") == "" && authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.RequestURI = ""
	if err := req.Write(upTLS); err != nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return err
	}
	if n := br.Buffered(); n > 0 {
		buf := make([]byte, n)
		_, _ = br.Read(buf)
		_, _ = upTLS.Write(buf)
	}
	pipe(clientTLS, upTLS)
	return nil
}

func (p *HostCredsProxy) handleControl(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
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
	if !p.controlAuthorized(req) {
		// Proxy token must not work on control.
		writeJSON(407, map[string]any{"ok": false, "error": "control auth required"})
		return
	}
	path := req.URL.Path
	if idx := strings.Index(path, "/__herd_control"); idx >= 0 {
		path = path[idx:]
	}
	switch {
	case strings.HasSuffix(path, "/ping") && (req.Method == http.MethodGet || req.Method == http.MethodPost):
		writeJSON(200, map[string]any{
			"ok":         true,
			"session_id": p.SessionID,
			"generation": p.Generation(),
			"hosts":      p.CredHosts(), // names only
		})
	case strings.HasSuffix(path, "/host_creds") && req.Method == http.MethodPost:
		var body struct {
			HostCreds map[string]string `json:"host_creds"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(400, map[string]any{"ok": false, "error": "bad json"})
			return
		}
		if len(body.HostCreds) == 0 {
			writeJSON(400, map[string]any{"ok": false, "error": "host_creds required"})
			return
		}
		for h, a := range body.HostCreds {
			if err := p.SetHostCredential(h, a); err != nil {
				writeJSON(403, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
		writeJSON(200, map[string]any{"ok": true, "hosts": len(body.HostCreds)})
	case strings.HasSuffix(path, "/revoke") && req.Method == http.MethodPost:
		var body struct {
			Host string `json:"host"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Host == "" {
			writeJSON(400, map[string]any{"ok": false, "error": "host required"})
			return
		}
		_ = p.RevokeHostCredential(body.Host)
		writeJSON(200, map[string]any{"ok": true, "revoked": body.Host})
	case strings.HasSuffix(path, "/rotate") && req.Method == http.MethodPost:
		var body struct {
			Host          string `json:"host"`
			Authorization string `json:"authorization"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(400, map[string]any{"ok": false, "error": "bad json"})
			return
		}
		if err := p.RotateHostCredential(body.Host, body.Authorization); err != nil {
			writeJSON(403, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(200, map[string]any{"ok": true, "rotated": body.Host})
	case strings.HasSuffix(path, "/shutdown") && req.Method == http.MethodPost:
		writeJSON(200, map[string]any{"ok": true, "shutdown": true})
		go func() { _ = p.Close() }()
	default:
		// Refuse secret dump endpoints — never emit credential values.
		writeJSON(404, map[string]any{"ok": false, "error": "unknown control path"})
	}
}

// --- helpers shared with broker internals ---

type brokerCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	mu      sync.Mutex
	leaves  map[string]*tls.Certificate
}

func newBrokerCA() (*brokerCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "herd-hostcreds-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &brokerCA{cert: cert, key: key, certPEM: pemBytes, leaves: map[string]*tls.Certificate{}}, nil
}

func (c *brokerCA) leafFor(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
		tmpl.DNSNames = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
	}
	c.leaves[host] = leaf
	return leaf, nil
}

func newBrokerToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("broker token crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func extractBasicOrBearer(req *http.Request) (user, pass string, bearer string, ok bool) {
	h := req.Header.Get("Proxy-Authorization")
	if h == "" {
		h = req.Header.Get("Authorization")
	}
	if strings.HasPrefix(h, "Bearer ") {
		return "", "", strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")), true
	}
	if strings.HasPrefix(strings.ToLower(h), "basic ") {
		idx := strings.Index(strings.ToLower(h), "basic ")
		raw := strings.TrimSpace(h[idx+6:])
		dec, err := decodeBasic(raw)
		if err != nil {
			return "", "", "", false
		}
		return dec[0], dec[1], "", true
	}
	return "", "", "", false
}

func isControlPath(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	return strings.HasPrefix(req.URL.Path, "/__herd_control")
}

func decodeBasic(raw string) ([2]string, error) {
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return [2]string{}, err
	}
	parts := strings.SplitN(string(dec), ":", 2)
	if len(parts) != 2 {
		return [2]string{}, fmt.Errorf("bad basic")
	}
	return [2]string{parts[0], parts[1]}, nil
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

func resolveAndPinIP(host string) (net.IP, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := validateDialIP(ip, true); err != nil {
			return nil, err
		}
		return ip, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("dns: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("dns: no addresses")
	}
	var chosen net.IP
	for _, ip := range ips {
		if err := validateDialIP(ip, false); err != nil {
			return nil, fmt.Errorf("dns rebind denied for %s: %w", host, err)
		}
		if chosen == nil || (ip.To4() != nil && chosen.To4() == nil) {
			chosen = ip
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("dns: no usable address for %s", host)
	}
	return chosen, nil
}

func validateDialIP(ip net.IP, allowLoopback bool) error {
	if ip == nil {
		return fmt.Errorf("nil ip")
	}
	if ip.IsLoopback() {
		if allowLoopback {
			return nil
		}
		return fmt.Errorf("loopback denied")
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("private/link-local destination denied")
	}
	return nil
}
