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

// BrokerReceipt is a redacted broker-side record of one authorized inject.
// Never contains secret material — only host/method/path and inject_ok.
type BrokerReceipt struct {
	Host     string
	Method   string
	Path     string
	InjectOK bool
}

// TLSMitmProxy is the stock-CLI transport: loopback HTTPS CONNECT MITM for
// exact host:443 only. Secrets inject from CredentialAuthority; workers never
// receive API keys.
//
// Peer policy (fail-closed, kernel-exact):
//   - Primary: client source port must be AllowClientPort-registered (exclusive
//     claim protocol). RemoteAddr().Port is kernel-visible after Accept.
//   - Secondary (Linux only): AllowPID + /proc peer PID. Darwin has no
//     authoritative TCP peer-PID API — lsof is not used (nondeterministic).
//   - If neither port nor (supported) PID matches, CONNECT is denied.
//   - Empty allowlists deny all.
//
// Request policy: after CONNECT+TLS, EVERY HTTP request is parsed and
// authorized (host/method/path/action/budget/auth-strip/inject). There is no
// raw keep-alive tunnel after the first request.
type TLSMitmProxy struct {
	mu sync.Mutex

	ln      net.Listener
	addr    string
	ca      *mitmCA
	caPath  string
	caDir   string // owned temp dir if we created it
	auth    CredentialAuthority
	rules   []RequestRule
	hosts   map[string]bool
	session string
	// allowed PIDs (Linux secondary peer path).
	allowed map[int]bool
	// allowedPorts: exclusive client source ports (primary peer path).
	allowedPorts map[int]bool
	closed       bool
	// reqCount counts authorized HTTP requests (not CONNECT handshakes).
	reqCount int
	maxReqs  int
	// active client conns for Close.
	conns map[net.Conn]struct{}

	// Test/production dial override: host is normalized SNI; ip is pin result.
	// When set, used instead of net.Dial to upstream :443 (local TLS origins).
	dialHook func(host string, ip net.IP) (net.Conn, error)
	// resolveHook overrides DNS pin (tests only).
	resolveHook func(host string) (net.IP, error)

	// Observability for tests (no secrets).
	LastInjectHost   string
	LastReceipt      BrokerReceipt
	ConnectCount     int
	RequestCount     int
	DeniedConnects   int
	DeniedRequests   int
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
	for _, r := range rules {
		hosts[strings.ToLower(r.Host)] = true
	}
	p := &TLSMitmProxy{
		ln:           ln,
		addr:         ln.Addr().String(),
		ca:           ca,
		caPath:       caPath,
		caDir:        "",
		auth:         auth,
		rules:        append([]RequestRule(nil), rules...),
		hosts:        hosts,
		session:      sessionID,
		allowed:      map[int]bool{},
		allowedPorts: map[int]bool{},
		maxReqs:      maxReqs,
		conns:        map[net.Conn]struct{}{},
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

func (p *TLSMitmProxy) AllowPID(pid int) {
	if p == nil || pid <= 0 {
		return
	}
	p.mu.Lock()
	p.allowed[pid] = true
	p.mu.Unlock()
}

// AllowClientPort registers an exclusive client source port for CONNECT peer auth.
// This is the primary production peer mechanism (kernel RemoteAddr after Accept).
func (p *TLSMitmProxy) AllowClientPort(port int) {
	if p == nil || port <= 0 {
		return
	}
	p.mu.Lock()
	if p.allowedPorts == nil {
		p.allowedPorts = map[int]bool{}
	}
	p.allowedPorts[port] = true
	p.mu.Unlock()
}

// ClearPeerAllowlists drops PID and port allowlists without closing the listener.
func (p *TLSMitmProxy) ClearPeerAllowlists() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.allowed = map[int]bool{}
	p.allowedPorts = map[int]bool{}
	p.mu.Unlock()
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
	p.allowedPorts = map[int]bool{}
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

	// Fail-closed peer policy.
	if err := p.authorizePeer(c); err != nil {
		p.mu.Lock()
		p.DeniedConnects++
		p.mu.Unlock()
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}

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
	nh, nerr := NormalizeHost(host, false)
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
		if err := p.authorizeAndForwardRequest(nh, creq, cbr, clientTLS, upTLS); err != nil {
			return
		}
	}
}

func (p *TLSMitmProxy) authorizeAndForwardRequest(host string, creq *http.Request, cbr *bufio.Reader, clientTLS, upTLS net.Conn) error {
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
	// Reject CRLF in remaining headers.
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
	// Redacted broker receipt only — never store Authorization value.
	receipt := BrokerReceipt{
		Host:     host,
		Method:   creq.Method,
		Path:     path,
		InjectOK: true,
	}
	p.mu.Lock()
	p.LastInjectHost = host
	p.LastReceipt = receipt
	p.mu.Unlock()

	creq.RequestURI = ""
	// Limit body size.
	if creq.Body != nil {
		creq.Body = io.NopCloser(io.LimitReader(creq.Body, MaxOracleBodyBytes))
	}
	if err := creq.Write(upTLS); err != nil {
		return err
	}
	if creq.Body != nil {
		_ = creq.Body.Close()
	}

	// Read ONE response and write to client — do not pipe residual streams raw.
	ubr := bufio.NewReader(upTLS)
	resp, err := http.ReadResponse(ubr, creq)
	if err != nil {
		return err
	}
	// Never follow redirects.
	if err := resp.Write(clientTLS); err != nil {
		_ = resp.Body.Close()
		return err
	}
	_ = resp.Body.Close()
	// If Connection: close, stop.
	if resp.Close || strings.EqualFold(resp.Header.Get("Connection"), "close") {
		return fmt.Errorf("connection close")
	}
	return nil
}

// authorizePeer fails closed unless the client source port is allowlisted
// (primary) or, on platforms with kernel peer-PID support, the peer PID is
// allowlisted. Empty allowlists deny. Unknown attribution denies (no fail-open).
func (p *TLSMitmProxy) authorizePeer(c net.Conn) error {
	p.mu.Lock()
	nPID := len(p.allowed)
	nPort := len(p.allowedPorts)
	ports := p.allowedPorts
	pids := p.allowed
	p.mu.Unlock()
	if nPID == 0 && nPort == 0 {
		return fmt.Errorf("no allowed peer")
	}

	// Primary: exact client source port (kernel RemoteAddr after Accept).
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok && ta != nil && ta.Port > 0 {
		if nPort > 0 && ports[ta.Port] {
			return nil
		}
	}

	// Secondary: kernel peer PID (Linux /proc only; Darwin returns 0).
	if nPID > 0 && peerPIDSupported() {
		pid := localPeerPID(c)
		if pid > 0 && pids[pid] {
			return nil
		}
	}
	return fmt.Errorf("peer not allowed")
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

// ProveMITMExactHost: CONNECT to denied host must 403 from a real worker with
// port-claim peer binding (no test bypass, no flaky lsof).
func ProveMITMExactHost(mitm *TLSMitmProxy, deniedHost string) error {
	if mitm == nil {
		return fmt.Errorf("nil mitm")
	}
	return proveCONNECTFromChild(mitm, deniedHost, true)
}

// ProveMITMRequiresAllowPID: without peer registration, CONNECT fail-closed.
// Name retained for callers; peer auth is port-claim (and Linux PID secondary).
func ProveMITMRequiresAllowPID(mitm *TLSMitmProxy, host string) error {
	if mitm == nil {
		return fmt.Errorf("nil")
	}
	mitm.ClearPeerAllowlists()
	return proveCONNECTFromChild(mitm, host, false)
}

// proveCONNECTFromChild runs worker-probe CONNECT in a subprocess with claim
// handshake. If registerChild, parent AllowClientPort after claim file appears.
func proveCONNECTFromChild(mitm *TLSMitmProxy, host string, registerChild bool) error {
	dir, err := os.MkdirTemp("", "hc-conn-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "out.json")
	claimPath := filepath.Join(dir, "claim")
	exe, err := findHerdOrBuild(dir)
	if err != nil {
		return err
	}
	// Exact scrubbed env — no os.Environ secret leftovers.
	env := ExactWorkerChildEnv(HarnessProxyEnv(mitm, mitm.session))
	cmd := exec.Command(exe, "hostcreds", "worker-probe",
		"--proxy", mitm.ProxyURL(),
		"--allow-host", "api.x.ai",
		"--deny-host", host,
		"--session", mitm.session,
		"--nonce", "00",
		"--out", outPath,
		"--claim", claimPath,
		"--connect-only",
	)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return err
	}
	if registerChild {
		if _, err := ParentAllowClaimedPort(mitm, claimPath, 8*time.Second); err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		// Linux secondary: also AllowPID when available.
		if cmd.Process != nil {
			mitm.AllowPID(cmd.Process.Pid)
		}
	}
	_, err = WaitWorkerProbe(cmd, outPath, 12*time.Second)
	raw, rerr := os.ReadFile(outPath)
	if rerr != nil && !registerChild {
		// Without allow, child may still write deny status.
		if raw == nil {
			return fmt.Errorf("want 403 without peer allow, err=%v", err)
		}
	}
	if !registerChild {
		if !strings.Contains(string(raw), "403") {
			return fmt.Errorf("want 403 without allow, got %s err=%v", raw, err)
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
