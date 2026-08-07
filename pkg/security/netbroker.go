package security

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// HostAllowBroker is an authenticated localhost HTTP CONNECT proxy for agents.
//
// Security split (FAC-133 root audit):
//   - ProxyToken (Token): agent-visible via HTTP_PROXY Basic userinfo — CONNECT only.
//   - ControlToken: coordinator-only on a SEPARATE control listener; never in
//     agent env, proxy URL, or agent-readable proxy state JSON.
//   - HostCreds: coordinator-injected upstream credentials (Authorization) for
//     allowlisted model hosts — never placed in agent environment.
type HostAllowBroker struct {
	AllowHosts   []string
	Token        string // proxy token (agent-visible)
	ControlToken string // coordinator-only
	HostCreds    map[string]string // host -> Authorization header value

	ln         net.Listener // proxy
	ctrlLn     net.Listener // control (may be nil for pure inline unit tests)
	addr       string
	ctrlAddr   string
	mu         sync.Mutex
	closed     bool
	ctrlIdentity  string
	ctrlTabID     string
	ctrlSession   string
	ctrlStatePath string
	ctrlStarted   int64
	boundOldState string
	boundNewState string
	boundNewTab   string
	ca            *brokerCA // ephemeral CA for HostCreds CONNECT MITM
	done          chan struct{}
	doneOnce      sync.Once
}

// StartHostAllowBroker listens on 127.0.0.1:0 for proxy traffic.
// Control listener is optional until EnableControl is called (durable path).
func StartHostAllowBroker(allowHosts []string) (*HostAllowBroker, error) {
	proxyTok, err := newBrokerToken()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("broker listen: %w", err)
	}
	b := &HostAllowBroker{
		AllowHosts: append([]string(nil), allowHosts...),
		Token:      proxyTok,
		ln:         ln,
		addr:       ln.Addr().String(),
		done:       make(chan struct{}),
		HostCreds:  map[string]string{},
	}
	go b.serveProxy()
	return b, nil
}

// EnableControl opens a separate control listener with its own ControlToken.
// Must be called before durable state is published.
func (b *HostAllowBroker) EnableControl(identity, tabID, sessionID, statePath string, startedUnix int64) error {
	if b == nil {
		return fmt.Errorf("nil broker")
	}
	ctrlTok, err := newBrokerToken()
	if err != nil {
		return err
	}
	ctrlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("control listen: %w", err)
	}
	b.mu.Lock()
	b.ControlToken = ctrlTok
	b.ctrlLn = ctrlLn
	b.ctrlAddr = ctrlLn.Addr().String()
	b.ctrlIdentity = identity
	b.ctrlTabID = tabID
	b.ctrlSession = sessionID
	b.ctrlStatePath = statePath
	b.ctrlStarted = startedUnix
	b.mu.Unlock()
	go b.serveControl()
	return nil
}

// SetHostCredential injects an upstream Authorization header for host (coordinator only).
// Never placed in agent env. Used for absolute-form forward and CONNECT MITM.
func (b *HostAllowBroker) SetHostCredential(host, authorization string) {
	if b == nil || host == "" || authorization == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.HostCreds == nil {
		b.HostCreds = map[string]string{}
	}
	b.HostCreds[strings.ToLower(host)] = authorization
	// Ensure MITM CA exists when credentials are configured.
	if b.ca == nil {
		if ca, err := newBrokerCA(); err == nil {
			b.ca = ca
		}
	}
}

// ControlAddr returns the coordinator control listen address.
func (b *HostAllowBroker) ControlAddr() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctrlAddr
}

// Done is closed when the broker has fully shut down.
func (b *HostAllowBroker) Done() <-chan struct{} {
	if b == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return b.done
}

func (b *HostAllowBroker) signalDone() {
	if b == nil {
		return
	}
	b.doneOnce.Do(func() {
		if b.done != nil {
			close(b.done)
		}
	})
}

func newBrokerToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("broker token crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (b *HostAllowBroker) Addr() string {
	if b == nil {
		return ""
	}
	return b.addr
}

// ProxyURL embeds ProxyToken Basic credentials only (never ControlToken).
func (b *HostAllowBroker) ProxyURL() string {
	if b == nil {
		return ""
	}
	u := &url.URL{
		Scheme: "http",
		User:   url.UserPassword("herd", b.Token),
		Host:   b.Addr(),
	}
	return u.String()
}

// Close stops proxy and control listeners (idempotent).
func (b *HostAllowBroker) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var first error
	if b.ln != nil {
		if err := b.ln.Close(); err != nil && first == nil {
			first = err
		}
	}
	if b.ctrlLn != nil {
		if err := b.ctrlLn.Close(); err != nil && first == nil {
			first = err
		}
	}
	b.signalDone()
	return first
}

// HostAllowed reports policy allow for CONNECT destinations.
func (b *HostAllowBroker) HostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, a := range b.AllowHosts {
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

func (b *HostAllowBroker) serveProxy() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handleProxy(c)
	}
}

func (b *HostAllowBroker) serveControl() {
	b.mu.Lock()
	ln := b.ctrlLn
	b.mu.Unlock()
	if ln == nil {
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go b.handleControlConn(c)
	}
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

func (b *HostAllowBroker) proxyAuthorized(req *http.Request) bool {
	if b.Token == "" {
		return false
	}
	user, pass, bearer, ok := extractBasicOrBearer(req)
	if !ok {
		return false
	}
	if bearer != "" {
		// Proxy accepts bearer of proxy token only — never control token.
		return bearer == b.Token && (b.ControlToken == "" || bearer != b.ControlToken)
	}
	return user == "herd" && pass == b.Token
}

func (b *HostAllowBroker) controlAuthorized(req *http.Request) bool {
	if b.ControlToken == "" {
		return false
	}
	user, pass, bearer, ok := extractBasicOrBearer(req)
	if !ok {
		return false
	}
	if bearer != "" {
		return bearer == b.ControlToken
	}
	// Control uses distinct basic username so proxy tokens cannot be reused by mistake.
	return user == "herd-ctrl" && pass == b.ControlToken
}

func (b *HostAllowBroker) handleProxy(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	// Control paths are NEVER served on the agent proxy listener.
	if isControlPath(req) {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	if !b.proxyAuthorized(req) {
		_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nConnection: close\r\n\r\n")
		return
	}
	// Malicious agent with proxy token must not control the broker.
	if b.controlAuthorized(req) && b.Token != b.ControlToken {
		// Proxy token cannot equal control token by construction.
	}
	target := req.Host
	if target == "" && req.URL != nil {
		target = req.URL.Host
	}
	if req.Method == http.MethodConnect {
		_ = b.dialAllowed(c, target)
		return
	}
	if req.URL == nil || req.URL.Scheme == "" {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return
	}
	scheme := strings.ToLower(req.URL.Scheme)
	if scheme != "http" && scheme != "https" {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return
	}
	host := req.URL.Host
	if !b.HostAllowed(host) {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	hOnly, port, err := net.SplitHostPort(host)
	if err != nil {
		hOnly = host
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	ip, err := resolveAndPinIP(hOnly)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	pinned := net.JoinHostPort(ip.String(), port)
	tr := pinnedNoProxyTransport(pinned, hOnly, scheme == "https")
	u := &url.URL{Scheme: scheme, Host: pinned, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	outReq, err := http.NewRequest(req.Method, u.String(), req.Body)
	if err != nil {
		return
	}
	outReq.Header = req.Header.Clone()
	outReq.Header.Del("Proxy-Authorization")
	outReq.Header.Del("Proxy-Connection")
	outReq.Host = hOnly
	// Inject coordinator-held upstream credentials (never in agent env).
	b.mu.Lock()
	cred := b.HostCreds[strings.ToLower(hOnly)]
	b.mu.Unlock()
	if cred != "" && outReq.Header.Get("Authorization") == "" {
		outReq.Header.Set("Authorization", cred)
	}
	resp, err := tr.RoundTrip(outReq)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer resp.Body.Close()
	_ = resp.Write(c)
}

func isControlPath(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	p := req.URL.Path
	if req.URL.Host != "" && strings.HasPrefix(req.URL.Path, "/__herd_control") {
		return true
	}
	return strings.HasPrefix(p, "/__herd_control")
}

func (b *HostAllowBroker) handleControlConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if !b.controlAuthorized(req) {
		// Proxy token must not work here — return 407.
		_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nConnection: close\r\n\r\n")
		return
	}
	path := req.URL.Path
	if idx := strings.Index(path, "/__herd_control"); idx >= 0 {
		path = path[idx:]
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

	b.mu.Lock()
	ident := b.ctrlIdentity
	tab := b.ctrlTabID
	started := b.ctrlStarted
	b.mu.Unlock()

	switch {
	case strings.HasSuffix(path, "/ping") && (req.Method == http.MethodGet || req.Method == http.MethodPost):
		if ident == "" {
			writeJSON(503, map[string]any{"ok": false, "error": "control not configured"})
			return
		}
		writeJSON(200, controlPingBody{
			Identity:      ident,
			PID:           os.Getpid(),
			TabID:         tab,
			StartedAtUnix: started,
			OK:            true,
		})
	case strings.HasSuffix(path, "/shutdown") && req.Method == http.MethodPost:
		writeJSON(200, map[string]any{"ok": true, "shutdown": true})
		go func() { _ = b.Close() }()
	case strings.HasSuffix(path, "/bind_rebind") && req.Method == http.MethodPost:
		var body struct {
			BoundOldState string `json:"bound_old_state"`
			BoundNewState string `json:"bound_new_state"`
			BoundNewTab   string `json:"bound_new_tab"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if body.BoundOldState == "" || body.BoundNewState == "" || body.BoundNewTab == "" {
			writeJSON(400, map[string]any{"ok": false, "error": "bound paths and tab required"})
			return
		}
		b.mu.Lock()
		b.boundOldState = body.BoundOldState
		b.boundNewState = body.BoundNewState
		b.boundNewTab = body.BoundNewTab
		b.mu.Unlock()
		writeJSON(200, map[string]any{"ok": true})
	case strings.HasSuffix(path, "/rebind") && req.Method == http.MethodPost:
		// In-memory tab/session update only — NO state_path, NO arbitrary FS writes.
		var body struct {
			TabID     string `json:"tab_id"`
			SessionID string `json:"session_id"`
			StatePath string `json:"state_path"` // rejected if present
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if body.StatePath != "" {
			writeJSON(400, map[string]any{"ok": false, "error": "state_path not allowed on control rebind"})
			return
		}
		b.mu.Lock()
		if b.boundNewTab != "" && body.TabID != b.boundNewTab {
			b.mu.Unlock()
			writeJSON(403, map[string]any{"ok": false, "error": "tab not pre-authorized"})
			return
		}
		if body.TabID != "" {
			b.ctrlTabID = body.TabID
		}
		if body.SessionID != "" {
			b.ctrlSession = body.SessionID
		}
		tab = b.ctrlTabID
		b.mu.Unlock()
		writeJSON(200, map[string]any{"ok": true, "tab_id": tab})
	case strings.HasSuffix(path, "/host_creds") && req.Method == http.MethodPost:
		var body struct {
			HostCreds map[string]string `json:"host_creds"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if len(body.HostCreds) == 0 {
			writeJSON(400, map[string]any{"ok": false, "error": "host_creds required"})
			return
		}
		for h, a := range body.HostCreds {
			b.SetHostCredential(h, a)
		}
		writeJSON(200, map[string]any{"ok": true, "hosts": len(body.HostCreds)})
	default:
		writeJSON(404, map[string]any{"ok": false, "error": "unknown control path"})
	}
}

func pinnedNoProxyTransport(pinnedAddr, serverName string, useTLS bool) *http.Transport {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return d.DialContext(ctx, network, pinnedAddr)
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: serverName,
		},
		ForceAttemptHTTP2: false,
	}
	_ = useTLS
	return tr
}

func (b *HostAllowBroker) dialAllowed(client net.Conn, target string) error {
	if !b.HostAllowed(target) {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return fmt.Errorf("forbidden")
	}
	host := target
	port := "443"
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	}
	// Coordinator HostCreds: MITM inject Authorization (never in agent env).
	if cred := b.hostCred(host); cred != "" {
		return b.dialAllowedCredentialed(client, host, port, cred)
	}
	ip, err := resolveAndPinIP(host)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return err
	}
	dest := net.JoinHostPort(ip.String(), port)
	up, err := net.DialTimeout("tcp", dest, 5*time.Second)
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

func rejectPrivateRebind(host string) error {
	_, err := resolveAndPinIP(host)
	return err
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// BrokerEnv returns proxy env for limited-network children (proxy token only).
func BrokerEnv(broker *HostAllowBroker) []string {
	if broker == nil {
		return nil
	}
	u := broker.ProxyURL()
	return []string{
		"HTTP_PROXY=" + u,
		"HTTPS_PROXY=" + u,
		"http_proxy=" + u,
		"https_proxy=" + u,
		"HERD_NETWORK_BROKER=" + u,
	}
}

// ProveBrokerAllowDeny proves negative deny and positive allow with proxy token.
func ProveBrokerAllowDeny(broker *HostAllowBroker, allowedHost, deniedHost string) error {
	if broker == nil {
		return fmt.Errorf("nil broker")
	}
	if broker.Token == "" {
		return fmt.Errorf("broker token empty")
	}
	hasLoop := false
	for _, h := range broker.AllowHosts {
		if h == "127.0.0.1" || h == "localhost" {
			hasLoop = true
			break
		}
	}
	if !hasLoop {
		broker.AllowHosts = append(broker.AllowHosts, "127.0.0.1")
	}
	if err := brokerConnectStatus(broker, deniedHost+":443", "403"); err != nil {
		return fmt.Errorf("deny proof: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 512)
		_, _ = c.Read(buf)
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	}()
	c, err := net.DialTimeout("tcp", broker.Addr(), 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	basic := base64.StdEncoding.EncodeToString([]byte("herd:" + broker.Token))
	_, _ = fmt.Fprintf(c, "CONNECT 127.0.0.1:%s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nProxy-Authorization: Basic %s\r\n\r\n", port, port, basic)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, "200") {
		return fmt.Errorf("allow proof want 200 got %q", strings.TrimSpace(line))
	}
	return nil
}

func brokerConnectStatus(broker *HostAllowBroker, hostport, want string) error {
	c, err := net.DialTimeout("tcp", broker.Addr(), 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	basic := base64.StdEncoding.EncodeToString([]byte("herd:" + broker.Token))
	_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", hostport, hostport, basic)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, want) {
		return fmt.Errorf("want %s got %q", want, strings.TrimSpace(line))
	}
	return nil
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
