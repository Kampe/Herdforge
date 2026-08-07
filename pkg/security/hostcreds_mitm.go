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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BrokerReceipt is a redacted, single-consume record of one authorized inject.
// Never contains secret material. Bound to session + peer port + capability
// nonce + request digest so a helper cannot satisfy another author's proof.
type BrokerReceipt struct {
	SessionID       string
	CapabilityNonce string
	PeerPort        int
	AuthorPID       int
	Host            string
	Method          string
	Path            string
	RequestDigest   string
	InjectOK        bool
	Consumed        bool // set true when LiveProof consumes this receipt once
}

// TLSMitmProxy is the stock-CLI transport: loopback HTTPS CONNECT MITM for
// exact host:443 only. Secrets inject from CredentialAuthority; workers never
// receive API keys.
//
// Peer policy (fail-closed, single-use):
//   - Primary: one-shot PeerGrant on client source port (inherited claim FD).
//     authorizePeer CONSUMES the grant on first match — port replay is denied.
//   - Secondary (Linux only): AllowPID + /proc peer PID, also single-use.
//   - Darwin: no PID peer path (lsof rejected). Author must use one-shot port.
//   - Empty grants deny all.
//
// Request policy: after CONNECT+TLS, EVERY HTTP request is parsed and
// authorized (host/method/path/action/budget/auth-strip/inject). There is no
// raw keep-alive tunnel after the first request.
type TLSMitmProxy struct {
	mu sync.Mutex

	ln     net.Listener
	addr   string
	ca     *mitmCA
	caPath string
	caDir  string // owned temp dir if we created it
	auth   CredentialAuthority
	rules  []RequestRule
	hosts  map[string]bool
	// allowLoopback is derived from hosts at construction (loopback IP literal
	// present in the allowlist). Never widens the allowlist itself.
	allowLoopback bool
	session       string
	// allowed PIDs (Linux secondary; single-use).
	allowed map[int]bool
	// oneShot: port → PeerGrant (consumed on first authorizePeer match).
	oneShot map[int]*PeerGrant
	// activePeer: conn identity for this accepted connection (port/grant).
	// Set per-connection in handle after authorizePeer.
	closed bool
	// reqCount counts authorized HTTP requests (not CONNECT handshakes).
	reqCount int
	maxReqs  int
	// active client conns for Close.
	conns map[net.Conn]struct{}

	// Test/production dial override: host is normalized SNI; ip is pin result.
	dialHook    func(host string, ip net.IP) (net.Conn, error)
	resolveHook func(host string) (net.IP, error)

	// Receipts: append-only list; ConsumeReceiptFor pulls exact match once.
	receipts []BrokerReceipt
	// Observability (no secrets).
	LastInjectHost string
	LastReceipt    BrokerReceipt
	ConnectCount   int
	RequestCount   int
	DeniedConnects int
	DeniedRequests int
}

// StartTLSMitmProxy starts loopback MITM. caDir must be a real directory we
// control; if empty a private temp dir is created. Refuses symlink caDir.
func StartTLSMitmProxy(sessionID string, auth CredentialAuthority, rules []RequestRule, caDir string, maxReqs int) (*TLSMitmProxy, error) {
	if auth == nil {
		return nil, &BlockedError{Reason: BlockMissingCreds, Code: "mitm_authority"}
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, &BlockedError{Reason: BlockNoSession, Code: "mitm_session_required"}
	}
	if maxReqs <= 0 {
		maxReqs = 32
	}
	ca, err := newMitmCA()
	if err != nil {
		return nil, err
	}

	owned := false
	if caDir == "" {
		caDir, err = os.MkdirTemp("", "hc-ca-*")
		if err != nil {
			return nil, err
		}
		owned = true
	} else {
		if err := secureMkdirCA(caDir); err != nil {
			return nil, err
		}
	}
	caPath := filepath.Join(caDir, "broker-ca.pem")
	// O_EXCL-style write: refuse if path is symlink.
	if err := writeFileNoFollow(caPath, ca.certPEM, 0o644); err != nil {
		if owned {
			_ = os.RemoveAll(caDir)
		}
		return nil, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if owned {
			_ = os.RemoveAll(caDir)
		}
		return nil, err
	}
	hosts := map[string]bool{}
	// allowLoopback is derived from the allowlist, never passed in: it only lets
	// a host that is ALREADY allowlisted normalize, so it cannot widen policy.
	// Production kinds map to DNS provider hosts, so this stays false for them.
	// Without it, handle() normalized with a hardcoded false and a loopback
	// component session could never reach its own allowlisted host.
	allowLoopback := false
	for _, r := range rules {
		h := strings.ToLower(r.Host)
		hosts[h] = true
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			allowLoopback = true
		}
	}
	p := &TLSMitmProxy{
		ln:            ln,
		addr:          ln.Addr().String(),
		ca:            ca,
		caPath:        caPath,
		caDir:         "",
		auth:          auth,
		rules:         append([]RequestRule(nil), rules...),
		hosts:         hosts,
		allowLoopback: allowLoopback,
		session:       sessionID,
		allowed:       map[int]bool{},
		oneShot:       map[int]*PeerGrant{},
		maxReqs:       maxReqs,
		conns:         map[net.Conn]struct{}{},
	}
	if owned {
		p.caDir = caDir
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

// ProxyURL has no userinfo (never a credential).
func (p *TLSMitmProxy) ProxyURL() string {
	return "http://" + p.Addr()
}

func (p *TLSMitmProxy) CAPEMPath() string {
	if p == nil {
		return ""
	}
	return p.caPath
}

// AllowPID registers a single-use Linux peer PID (secondary, tests only).
// Production live path must NOT call this — use AllowOneShotPeer + inherited FD.
// RecordHarnessPrompt intentionally does not call AllowPID.
func (p *TLSMitmProxy) AllowPID(pid int) {
	if p == nil || pid <= 0 {
		return
	}
	p.mu.Lock()
	p.allowed[pid] = true
	p.mu.Unlock()
}

// AllowOneShotPeer registers a single-use source-port grant (primary peer path).
// Port is consumed on first successful authorizePeer — replay is denied.
func (p *TLSMitmProxy) AllowOneShotPeer(g PeerGrant) error {
	if p == nil {
		return fmt.Errorf("nil mitm")
	}
	if g.Port <= 0 || strings.TrimSpace(g.SessionID) == "" {
		return &BlockedError{Reason: BlockAbuse, Code: "oneshot_grant_invalid"}
	}
	if g.SessionID != p.session {
		return &BlockedError{Reason: BlockNoSession, Code: "oneshot_session_mismatch", SessionID: g.SessionID}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oneShot == nil {
		p.oneShot = map[int]*PeerGrant{}
	}
	cp := g
	p.oneShot[g.Port] = &cp
	return nil
}

// BindOneShotAuthorPID stamps AuthorPID onto all unconsumed one-shot grants
// (after child Start). Does not grant peer access by PID.
func (p *TLSMitmProxy) BindOneShotAuthorPID(pid int) {
	if p == nil || pid <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.oneShot {
		if g != nil && !g.Consumed {
			g.AuthorPID = pid
		}
	}
}

// AllowClientPort is a thin wrapper for tests: one-shot grant without nonce.
// Prefer AllowOneShotPeer with full binding.
func (p *TLSMitmProxy) AllowClientPort(port int) {
	if p == nil || port <= 0 {
		return
	}
	_ = p.AllowOneShotPeer(PeerGrant{Port: port, SessionID: p.session})
}

// ClearPeerAllowlists drops PID and one-shot grants without closing the listener.
func (p *TLSMitmProxy) ClearPeerAllowlists() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.allowed = map[int]bool{}
	p.oneShot = map[int]*PeerGrant{}
	p.mu.Unlock()
}

// ConsumeReceiptFor returns and marks consumed the first unconsumed receipt
// matching session + nonce + peer port (and optional request digest).
func (p *TLSMitmProxy) ConsumeReceiptFor(sessionID, nonce string, peerPort int, reqDigest string) (BrokerReceipt, bool) {
	if p == nil {
		return BrokerReceipt{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.receipts {
		r := &p.receipts[i]
		if r.Consumed || !r.InjectOK {
			continue
		}
		if r.SessionID != sessionID {
			continue
		}
		if nonce != "" && r.CapabilityNonce != nonce {
			continue
		}
		if peerPort > 0 && r.PeerPort != peerPort {
			continue
		}
		if reqDigest != "" && r.RequestDigest != reqDigest {
			continue
		}
		r.Consumed = true
		out := *r
		p.LastReceipt = out
		return out, true
	}
	return BrokerReceipt{}, false
}

// UnconsumedReceiptCount is for tests.
func (p *TLSMitmProxy) UnconsumedReceiptCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.receipts {
		if r.InjectOK && !r.Consumed {
			n++
		}
	}
	return n
}

// SetDialHook installs a test/production upstream dial override (no secrets).
func (p *TLSMitmProxy) SetDialHook(fn func(host string, ip net.IP) (net.Conn, error)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.dialHook = fn
	p.mu.Unlock()
}

// SetResolveHook installs a test DNS pin override.
func (p *TLSMitmProxy) SetResolveHook(fn func(host string) (net.IP, error)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.resolveHook = fn
	p.mu.Unlock()
}

// Revoke drops allowlist and closes listeners/conns (session killed).
func (p *TLSMitmProxy) Revoke() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.allowed = map[int]bool{}
	p.oneShot = map[int]*PeerGrant{}
	p.mu.Unlock()
	return p.Close()
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
	conns := p.conns
	p.conns = nil
	caDir := p.caDir
	// Zero CA private key material best-effort.
	if p.ca != nil {
		p.ca.key = nil
		p.ca.leaves = nil
	}
	p.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for c := range conns {
		_ = c.Close()
	}
	if caDir != "" {
		_ = os.RemoveAll(caDir)
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
		p.trackConn(c)
		go p.handle(c)
	}
}

func (p *TLSMitmProxy) trackConn(c net.Conn) {
	p.mu.Lock()
	if p.conns != nil {
		p.conns[c] = struct{}{}
	}
	p.mu.Unlock()
}

func (p *TLSMitmProxy) untrackConn(c net.Conn) {
	p.mu.Lock()
	if p.conns != nil {
		delete(p.conns, c)
	}
	p.mu.Unlock()
}

func (p *TLSMitmProxy) handle(c net.Conn) {
	defer func() {
		p.untrackConn(c)
		_ = c.Close()
	}()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))

	// Fail-closed peer policy (one-shot consume).
	grant, err := p.authorizePeer(c)
	if err != nil {
		p.mu.Lock()
		p.DeniedConnects++
		p.mu.Unlock()
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	// grant may be nil when PID secondary path used without port grant.

	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		// No absolute-form plaintext inject.
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
	if port != "443" {
		p.mu.Lock()
		p.DeniedConnects++
		p.mu.Unlock()
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	nh, nerr := NormalizeHost(host, p.allowLoopback)
	if nerr != nil || !p.hosts[nh] {
		p.mu.Lock()
		p.DeniedConnects++
		p.mu.Unlock()
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}

	p.mu.Lock()
	p.ConnectCount++
	p.mu.Unlock()

	leaf, err := p.ca.leafFor(nh)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	p.mu.Lock()
	rhook := p.resolveHook
	dhook := p.dialHook
	p.mu.Unlock()
	var ip net.IP
	if rhook != nil {
		ip, err = rhook(nh)
	} else {
		ip, err = resolveAndPinIP(nh)
	}
	if err != nil || ip == nil {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	var upRaw net.Conn
	if dhook != nil {
		upRaw, err = dhook(nh, ip)
	} else {
		upRaw, err = net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "443"), 8*time.Second)
	}
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = upRaw.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})

	// TLS must consume any leftover bytes in br (post-CONNECT request buffer).
	clientTLS := tls.Server(&bufConn{Conn: c, r: br}, &tls.Config{Certificates: []tls.Certificate{*leaf}, MinVersion: tls.VersionTLS12})
	if err := clientTLS.Handshake(); err != nil {
		_ = upRaw.Close()
		return
	}
	// dialHook targets are typically local test TLS origins with non-matching SNI.
	upTLSCfg := &tls.Config{ServerName: nh, MinVersion: tls.VersionTLS12}
	if dhook != nil {
		upTLSCfg.InsecureSkipVerify = true
	}
	upTLS := tls.Client(upRaw, upTLSCfg)
	if err := upTLS.Handshake(); err != nil {
		_ = clientTLS.Close()
		_ = upRaw.Close()
		return
	}
	defer clientTLS.Close()
	defer upTLS.Close()

	// Parse/authorize EVERY request — no raw keep-alive tunnel.
	cbr := bufio.NewReader(clientTLS)
	for {
		_ = clientTLS.SetDeadline(time.Now().Add(60 * time.Second))
		_ = upTLS.SetDeadline(time.Now().Add(60 * time.Second))
		creq, err := http.ReadRequest(cbr)
		if err != nil {
			return
		}
		if err := p.authorizeAndForwardRequest(nh, creq, cbr, clientTLS, upTLS, grant); err != nil {
			return
		}
	}
}

func (p *TLSMitmProxy) authorizeAndForwardRequest(host string, creq *http.Request, cbr *bufio.Reader, clientTLS, upTLS net.Conn, grant *PeerGrant) error {
	path, perr := normalizePathForMatch(creq.URL.RequestURI())
	if perr != nil {
		path, _ = normalizePathForMatch(creq.URL.Path)
	}
	if MatchRequestRule(p.rules, host, creq.Method, path) == nil {
		p.mu.Lock()
		p.DeniedRequests++
		p.mu.Unlock()
		return fmt.Errorf("request denied")
	}

	p.mu.Lock()
	if p.reqCount >= p.maxReqs {
		p.mu.Unlock()
		p.DeniedRequests++
		return fmt.Errorf("budget exceeded")
	}
	p.reqCount++
	p.RequestCount = p.reqCount
	p.mu.Unlock()

	// Strip worker Authorization; inject from authority only.
	creq.Header.Del("Authorization")
	for k, vv := range creq.Header {
		for _, v := range vv {
			if strings.ContainsAny(k, "\r\n\x00") || strings.ContainsAny(v, "\r\n\x00") {
				p.mu.Lock()
				p.DeniedRequests++
				p.mu.Unlock()
				return fmt.Errorf("header injection")
			}
		}
	}
	if err := p.auth.InjectAuthorization(host, creq.Header); err != nil {
		p.mu.Lock()
		p.DeniedRequests++
		p.mu.Unlock()
		return err
	}
	if IsDummyCredential(creq.Header.Get("Authorization")) || creq.Header.Get("Authorization") == "" {
		return fmt.Errorf("inject failed")
	}

	// Bound redacted receipt — never Authorization value.
	nonce := ""
	peerPort := 0
	authorPID := 0
	if grant != nil {
		nonce = grant.CapabilityNonce
		peerPort = grant.Port
		authorPID = grant.AuthorPID
	}
	// Body prefix for digest (limited, non-secret).
	bodyPrefix := ""
	if creq.Body != nil {
		// peek not available; digest path+method only when body already consumed later
		bodyPrefix = ""
	}
	// Optional worker-supplied request id header (non-secret).
	if rid := creq.Header.Get("X-Herd-Req-Digest"); rid != "" && !strings.ContainsAny(rid, "\r\n") {
		bodyPrefix = rid
	}
	dig := RequestDigest(p.session, creq.Method, host, path, nonce, bodyPrefix)
	receipt := BrokerReceipt{
		SessionID:       p.session,
		CapabilityNonce: nonce,
		PeerPort:        peerPort,
		AuthorPID:       authorPID,
		Host:            host,
		Method:          creq.Method,
		Path:            path,
		RequestDigest:   dig,
		InjectOK:        true,
		Consumed:        false,
	}
	p.mu.Lock()
	p.LastInjectHost = host
	p.LastReceipt = receipt
	p.receipts = append(p.receipts, receipt)
	p.mu.Unlock()

	creq.RequestURI = ""
	if creq.Body != nil {
		creq.Body = io.NopCloser(io.LimitReader(creq.Body, MaxOracleBodyBytes))
	}
	if err := creq.Write(upTLS); err != nil {
		return err
	}
	if creq.Body != nil {
		_ = creq.Body.Close()
	}

	ubr := bufio.NewReader(upTLS)
	resp, err := http.ReadResponse(ubr, creq)
	if err != nil {
		return err
	}
	if err := resp.Write(clientTLS); err != nil {
		_ = resp.Body.Close()
		return err
	}
	_ = resp.Body.Close()
	if resp.Close || strings.EqualFold(resp.Header.Get("Connection"), "close") {
		return fmt.Errorf("connection close")
	}
	return nil
}

// authorizePeer fails closed. One-shot port grant is consumed on match.
// Returns the consumed grant (may be synthetic for PID path).
func (p *TLSMitmProxy) authorizePeer(c net.Conn) (*PeerGrant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	nPID := len(p.allowed)
	nShot := len(p.oneShot)
	if nPID == 0 && nShot == 0 {
		return nil, fmt.Errorf("no allowed peer")
	}

	// Primary: exact client source port → consume one-shot grant.
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok && ta != nil && ta.Port > 0 {
		if g, ok := p.oneShot[ta.Port]; ok && g != nil && !g.Consumed {
			g.Consumed = true
			delete(p.oneShot, ta.Port)
			cp := *g
			return &cp, nil
		}
	}

	// Secondary: kernel peer PID (Linux only), single-use.
	if nPID > 0 && peerPIDSupported() {
		pid := localPeerPID(c)
		if pid > 0 && p.allowed[pid] {
			delete(p.allowed, pid)
			return &PeerGrant{SessionID: p.session, AuthorPID: pid, Consumed: true}, nil
		}
	}
	return nil, fmt.Errorf("peer not allowed")
}

// --- CA / helpers ---

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

// localPeerPID is implemented in hostcreds_peer_*.go (production TCP port→PID).

// findHerdOrBuild returns a herd binary for worker-probe subprocesses.
func findHerdOrBuild(tmpDir string) (string, error) {
	if p, err := exec.LookPath("herd"); err == nil {
		return p, nil
	}
	// Build from module (tests run from package dir).
	bin := filepath.Join(tmpDir, "herd-probe")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/Kampe/Herdforge/cmd/herd")
	if out, err := cmd.CombinedOutput(); err != nil {
		// try relative from repo
		cmd = exec.Command("go", "build", "-o", bin, "./cmd/herd")
		cmd.Dir = findModuleRoot()
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			return "", fmt.Errorf("build herd: %v %s / %v %s", err, out, err2, out2)
		}
	}
	return bin, nil
}

func findModuleRoot() string {
	wd, _ := os.Getwd()
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wd
}

func secureMkdirCA(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0o700)
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return &BlockedError{Reason: BlockAbuse, Code: "ca_dir_symlink"}
	}
	if !fi.IsDir() {
		return &BlockedError{Reason: BlockAbuse, Code: "ca_dir_not_dir"}
	}
	return nil
}

func writeFileNoFollow(path string, data []byte, mode os.FileMode) error {
	// Refuse if path exists as symlink.
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return &BlockedError{Reason: BlockAbuse, Code: "ca_pem_symlink"}
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_EXCL, mode)
	if err != nil {
		// If exists and not symlink, overwrite via temp+rename in same dir.
		if !os.IsExist(err) {
			return err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, mode); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}

// HarnessProxyEnv: MITM proxy + public CA; explicit empty API keys (no dummy).
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
		"HOME=", // isolate host auth files — live also sets explicit empty HOME dir
		"ANTHROPIC_API_KEY=",
		"OPENAI_API_KEY=",
		"XAI_API_KEY=",
		"HERD_HOST_CREDS=",
		"HERD_HOSTCREDS_HANDLES=",
	}
}

// ProveMITMExactHost proves the CONNECT host allowlist itself.
//
// The peer MUST be attributed on both legs (fresh one-shot claim FD each time),
// so the only variable is the host. An unattributed dial is denied by
// authorizePeer before handle() ever reaches the host check, which is why the
// earlier child-based prover could not observe host policy at all: its deny leg
// was a bare dial and the 403 came from peer attribution.
//
// Two legs, because a 403-only assertion also passes on a wholly broken proxy:
//
//	negative — attributed CONNECT to deniedHost must 403 and must NOT count a connect
//	positive — attributed CONNECT to an allowlisted host must pass the host gate
//
// ConnectCount is bumped immediately after the host check and before any
// upstream dial, so it is the exact host-gate boundary and needs no network.
func ProveMITMExactHost(mitm *TLSMitmProxy, deniedHost string) error {
	if mitm == nil {
		return fmt.Errorf("nil mitm")
	}
	allowHost := mitm.anAllowedHost()
	if allowHost == "" {
		return fmt.Errorf("proxy has no allowlisted host; nothing to contrast")
	}
	if nh, err := NormalizeHost(deniedHost, false); err == nil && nh == allowHost {
		return fmt.Errorf("denied host %q is the allowlisted host; vacuous", deniedHost)
	}

	// Negative leg: attributed peer, non-allowlisted host.
	beforeC, beforeD := mitm.counters()
	status, err := attributedCONNECTStatus(mitm, deniedHost)
	if err != nil {
		return fmt.Errorf("denied-host leg: %w", err)
	}
	afterC, afterD := mitm.counters()
	if !strings.Contains(status, "403") {
		return fmt.Errorf("attributed CONNECT to non-allowlisted %q not denied: %q", deniedHost, status)
	}
	if afterD == beforeD {
		return fmt.Errorf("denied-host leg did not register a denial (host gate not reached)")
	}
	if afterC != beforeC {
		return fmt.Errorf("non-allowlisted %q passed the host gate (ConnectCount %d→%d)", deniedHost, beforeC, afterC)
	}

	// Positive control: identical attributed path, allowlisted host, must pass
	// the host gate. Without this the negative leg proves nothing.
	beforeC2, _ := mitm.counters()
	if err := attributedCONNECTPassesHostGate(mitm, allowHost, beforeC2); err != nil {
		return fmt.Errorf("allowlisted %q wrongly denied — denied-host leg is vacuous: %w", allowHost, err)
	}
	return nil
}

// counters reads the observability counters under the proxy lock.
func (p *TLSMitmProxy) counters() (connects, deniedConnects int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ConnectCount, p.DeniedConnects
}

// anAllowedHost returns one allowlisted host (deterministic: lowest sorted).
func (p *TLSMitmProxy) anAllowedHost() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	best := ""
	for h := range p.hosts {
		if best == "" || h < best {
			best = h
		}
	}
	return best
}

// attributedCONNECTStatus issues one CONNECT from a freshly granted one-shot
// peer and returns the proxy's status line. Peer attribution succeeds by
// construction, so any 403 is host/port policy.
func attributedCONNECTStatus(mitm *TLSMitmProxy, host string) (string, error) {
	conn, err := dialAttributed(mitm)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host); err != nil {
		return "", err
	}
	buf := make([]byte, 128)
	n, _ := conn.Read(buf)
	return string(buf[:n]), nil
}

// attributedCONNECTPassesHostGate asserts an attributed CONNECT to host gets
// past the allowlist. It waits on ConnectCount rather than the status line so
// no upstream network is required (the counter bumps before any dial).
func attributedCONNECTPassesHostGate(mitm *TLSMitmProxy, host string, before int) error {
	conn, err := dialAttributed(mitm)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host); err != nil {
		return err
	}
	if !waitUntil(5*time.Second, func() bool {
		c, _ := mitm.counters()
		return c > before
	}) {
		return fmt.Errorf("ConnectCount never advanced past %d", before)
	}
	return nil
}

// dialAttributed binds an exclusive source port, grants it one-shot, and dials
// the proxy from it — the same kernel-owned attribution a real author child gets.
func dialAttributed(mitm *TLSMitmProxy) (net.Conn, error) {
	port, f, err := ClaimLocalPort()
	if err != nil {
		return nil, err
	}
	if err := mitm.AllowOneShotPeer(PeerGrant{
		Port: port, SessionID: mitm.session, CapabilityNonce: "hostpolicy",
	}); err != nil {
		_ = f.Close()
		return nil, err
	}
	conn, err := ConnectClaimed(f, mitm.Addr())
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// ProveMITMRequiresAllowPID: without peer registration, CONNECT fail-closed.
func ProveMITMRequiresAllowPID(mitm *TLSMitmProxy, host string) error {
	if mitm == nil {
		return fmt.Errorf("nil")
	}
	mitm.ClearPeerAllowlists()
	return proveCONNECTFromChild(mitm, host, false)
}

// proveCONNECTFromChild runs author-causal/worker CONNECT via inherited FD.
// If registerChild, parent ClaimLocalPort + AllowOneShotPeer before Start.
func proveCONNECTFromChild(mitm *TLSMitmProxy, host string, registerChild bool) error {
	dir, err := os.MkdirTemp("", "hc-conn-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "out.json")
	exe, err := findHerdOrBuild(dir)
	if err != nil {
		return err
	}
	env := ExactWorkerChildEnv(HarnessProxyEnv(mitm, mitm.session))
	cmd := exec.Command(exe, "hostcreds", "author-causal",
		"--proxy", mitm.ProxyURL(),
		"--allow-host", "api.x.ai",
		"--deny-host", host,
		"--session", mitm.session,
		"--nonce", "00",
		"--out", outPath,
		"--connect-only",
	)
	cmd.Env = env
	var claim *os.File
	if registerChild {
		port, f, cerr := ClaimLocalPort()
		if cerr != nil {
			return cerr
		}
		claim = f
		if err := mitm.AllowOneShotPeer(PeerGrant{
			Port: port, SessionID: mitm.session, CapabilityNonce: "00",
		}); err != nil {
			_ = f.Close()
			return err
		}
		cmd.ExtraFiles = []*os.File{f}
		cmd.Env = ExactWorkerChildEnv(env, []string{"HERD_HOSTCREDS_CLAIM_FD=3"})
	}
	if err := cmd.Start(); err != nil {
		if claim != nil {
			_ = claim.Close()
		}
		return err
	}
	if claim != nil {
		_ = claim.Close() // child holds dup via ExtraFiles
	}
	_, err = WaitWorkerProbe(cmd, outPath, 12*time.Second)
	raw, rerr := os.ReadFile(outPath)
	if !registerChild {
		if rerr != nil {
			return fmt.Errorf("want 403 without peer allow, err=%v read=%v", err, rerr)
		}
		if !strings.Contains(string(raw), "403") {
			return fmt.Errorf("want 403 without allow, got %s", raw)
		}
		return nil
	}
	if rerr != nil {
		return rerr
	}
	if !strings.Contains(string(raw), "403") {
		return fmt.Errorf("want 403 for denied host, got %s", raw)
	}
	return nil
}

// ExactWorkerChildEnv builds a child environ with no duplicate keys and no
// inherited secret entries. Only PATH/LANG/HOME from host (HOME forced empty
// unless provided) plus the given overrides (last key wins).
func ExactWorkerChildEnv(overrides ...[]string) []string {
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"LANG=C",
		"HOME=",
		"ANTHROPIC_API_KEY=",
		"OPENAI_API_KEY=",
		"XAI_API_KEY=",
		"HERD_HOST_CREDS=",
		"HERD_HOSTCREDS_HANDLES=",
	}
	m := map[string]string{}
	order := make([]string, 0, 32)
	put := func(e string) {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			return
		}
		k := e[:i]
		v := e[i+1:]
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	for _, e := range base {
		put(e)
	}
	for _, group := range overrides {
		for _, e := range group {
			put(e)
		}
	}
	// Never allow non-empty API key / handle env.
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS", "HERD_HOSTCREDS_HANDLES"} {
		m[k] = ""
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}
