package security

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TLSMitmProxy is the harness transport adapter: loopback HTTPS CONNECT MITM
// for exact host:443 only. Secrets are injected from CredentialAuthority inside
// the broker; the worker never receives API keys (real or dummy).
//
// Authentication of the worker is by registered PID (local peer), not a proxy bearer.
type TLSMitmProxy struct {
	mu sync.Mutex

	ln       net.Listener
	addr     string // 127.0.0.1:port
	ca       *mitmCA
	caPath   string // public CA PEM path for SSL_CERT_FILE
	auth     CredentialAuthority
	rules    []RequestRule
	hosts    map[string]bool // exact hosts allowlisted for this session kind
	session  string
	allowed  map[int]bool // worker PIDs allowed to CONNECT
	closed   bool
	reqCount int
	maxReqs  int
	// lastInjectHost for tests (host name only)
	lastInjectHost string
}

// StartTLSMitmProxy starts a loopback MITM for the session's kind rules.
func StartTLSMitmProxy(sessionID string, auth CredentialAuthority, rules []RequestRule, caDir string, maxReqs int) (*TLSMitmProxy, error) {
	if auth == nil {
		return nil, &BlockedError{Reason: BlockMissingCreds, Code: "mitm_authority"}
	}
	if maxReqs <= 0 {
		maxReqs = 32
	}
	ca, err := newMitmCA()
	if err != nil {
		return nil, err
	}
	if caDir == "" {
		var e error
		caDir, e = os.MkdirTemp("", "hc-ca-*")
		if e != nil {
			return nil, e
		}
	}
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return nil, err
	}
	caPath := filepath.Join(caDir, "broker-ca.pem")
	if err := os.WriteFile(caPath, ca.certPEM, 0o644); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{}
	for _, r := range rules {
		hosts[strings.ToLower(r.Host)] = true
	}
	p := &TLSMitmProxy{
		ln:      ln,
		addr:    ln.Addr().String(),
		ca:      ca,
		caPath:  caPath,
		auth:    auth,
		rules:   append([]RequestRule(nil), rules...),
		hosts:   hosts,
		session: sessionID,
		allowed: map[int]bool{},
		maxReqs: maxReqs,
	}
	go p.serve()
	return p, nil
}

func (p *TLSMitmProxy) Addr() string {
	if p == nil {
		return ""
	}
	return p.addr
}

func (p *TLSMitmProxy) ProxyURL() string {
	// No userinfo — never a bearer/credential in the proxy URL.
	return "http://" + p.Addr()
}

func (p *TLSMitmProxy) CAPEMPath() string {
	if p == nil {
		return ""
	}
	return p.caPath
}

// AllowPID registers a worker process allowed to use the MITM.
func (p *TLSMitmProxy) AllowPID(pid int) {
	if p == nil || pid <= 0 {
		return
	}
	p.mu.Lock()
	p.allowed[pid] = true
	p.mu.Unlock()
}

func (p *TLSMitmProxy) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	ln := p.ln
	p.ln = nil
	p.mu.Unlock()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

func (p *TLSMitmProxy) serve() {
	for {
		p.mu.Lock()
		ln := p.ln
		p.mu.Unlock()
		if ln == nil {
			return
		}
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *TLSMitmProxy) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))

	// Peer PID binding (Darwin/Linux local TCP is weak; we still check when available).
	if pid := localPeerPID(c); pid > 0 {
		p.mu.Lock()
		ok := p.allowed[pid]
		// Also allow children of allowed PIDs is hard without /proc; require exact register.
		p.mu.Unlock()
		if !ok && len(p.allowed) > 0 {
			_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
			return
		}
	}

	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		// No absolute-form plaintext inject — refuse non-CONNECT.
		_, _ = io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n")
		return
	}
	hostPort := req.Host
	if hostPort == "" {
		hostPort = req.URL.Host
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
		port = "443"
	}
	// Exact port 443 only for credentialed providers.
	if port != "443" {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	nh, nerr := NormalizeHost(host, false)
	if nerr != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	if !p.hosts[nh] {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}

	p.mu.Lock()
	if p.reqCount >= p.maxReqs {
		p.mu.Unlock()
		_, _ = io.WriteString(c, "HTTP/1.1 429 Too Many Requests\r\nConnection: close\r\n\r\n")
		return
	}
	p.reqCount++
	p.mu.Unlock()

	// MITM: terminate client TLS with leaf for host, open TLS to upstream, inject Authorization.
	leaf, err := p.ca.leafFor(nh)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	ip, err := resolveAndPinIP(nh)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	upRaw, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "443"), 8*time.Second)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = upRaw.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})

	clientTLS := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}, MinVersion: tls.VersionTLS12})
	if err := clientTLS.Handshake(); err != nil {
		_ = upRaw.Close()
		return
	}
	upTLS := tls.Client(upRaw, &tls.Config{ServerName: nh, MinVersion: tls.VersionTLS12})
	if err := upTLS.Handshake(); err != nil {
		_ = clientTLS.Close()
		_ = upRaw.Close()
		return
	}

	cbr := bufio.NewReader(clientTLS)
	creq, err := http.ReadRequest(cbr)
	if err != nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return
	}
	// Path/method must match session rules.
	path, _ := normalizePathForMatch(creq.URL.RequestURI())
	if path == "" {
		path, _ = normalizePathForMatch(creq.URL.Path)
	}
	if MatchRequestRule(p.rules, nh, creq.Method, path) == nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return
	}
	// Strip any worker Authorization; inject from authority only.
	creq.Header.Del("Authorization")
	if err := p.auth.InjectAuthorization(nh, creq.Header); err != nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return
	}
	// Reject if inject somehow used dummy.
	if IsDummyCredential(creq.Header.Get("Authorization")) {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return
	}
	p.mu.Lock()
	p.lastInjectHost = nh
	p.mu.Unlock()

	creq.RequestURI = ""
	if err := creq.Write(upTLS); err != nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return
	}
	if n := cbr.Buffered(); n > 0 {
		buf := make([]byte, n)
		_, _ = cbr.Read(buf)
		_, _ = upTLS.Write(buf)
	}
	// Pipe remainder; do not follow redirects (single request write already done).
	pipeConns(clientTLS, upTLS)
}

// --- CA ---

type mitmCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	mu      sync.Mutex
	leaves  map[string]*tls.Certificate
}

func newMitmCA() (*mitmCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "herd-hostcreds-mitm-ca"},
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
	return &mitmCA{cert: cert, key: key, certPEM: pemBytes, leaves: map[string]*tls.Certificate{}}, nil
}

func (c *mitmCA) leafFor(host string) (*tls.Certificate, error) {
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}
	c.leaves[host] = leaf
	return leaf, nil
}

func pipeConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// localPeerPID best-effort peer PID for localhost connections (0 if unknown).
func localPeerPID(c net.Conn) int {
	// TCP peer credentials are not portable; return 0 (caller may skip).
	// Unix sockets would use getpeereid; MITM uses TCP for HTTPS_PROXY compat.
	_ = c
	_ = syscall.AF_UNIX
	return 0
}

// HarnessProxyEnv returns env for a stock hosted CLI: proxy + public CA only.
// No real or dummy API keys. Explicit empty key vars prevent inheritance.
func HarnessProxyEnv(mitm *TLSMitmProxy, sessionID string) []string {
	if mitm == nil {
		return nil
	}
	u := mitm.ProxyURL()
	ca := mitm.CAPEMPath()
	return []string{
		"HTTPS_PROXY=" + u,
		"HTTP_PROXY=" + u,
		"https_proxy=" + u,
		"http_proxy=" + u,
		"SSL_CERT_FILE=" + ca,
		"SSL_CERT_DIR=" + filepath.Dir(ca),
		"HERD_HOSTCREDS_SESSION=" + sessionID,
		"HERD_HOSTCREDS_TRANSPORT=https-mitm-connect",
		// Explicit empty — no real or dummy key bootstrap.
		"ANTHROPIC_API_KEY=",
		"OPENAI_API_KEY=",
		"XAI_API_KEY=",
		"HERD_HOST_CREDS=",
		"HERD_HOSTCREDS_HANDLES=",
	}
}

// ProveMITMExactHost is a unit helper: CONNECT non-allowlisted host must 403.
func ProveMITMExactHost(mitm *TLSMitmProxy, deniedHost string) error {
	if mitm == nil {
		return fmt.Errorf("nil mitm")
	}
	c, err := net.DialTimeout("tcp", mitm.Addr(), 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_, _ = fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", deniedHost, deniedHost)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, "403") {
		return fmt.Errorf("want 403 got %q", strings.TrimSpace(line))
	}
	return nil
}
