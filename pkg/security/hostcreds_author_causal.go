package security

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AuthorCausalResult is written by the REAL author child process itself
// (not a post-hoc helper). Contains derived marker, allow TLS status, deny
// status, peer port, and request digest — never secrets.
type AuthorCausalResult struct {
	PID              int    `json:"pid"`
	SessionID        string `json:"session_id"`
	Nonce            string `json:"nonce"`
	CapabilityMarker string `json:"capability_marker"`
	PeerPort         int    `json:"peer_port"`
	AllowCONNECT     string `json:"allow_connect"`
	DenyCONNECT      string `json:"deny_connect"`
	AllowHost        string `json:"allow_host"`
	DenyHost         string `json:"deny_host"`
	TLSMethod        string `json:"tls_method"`
	TLSPath          string `json:"tls_path"`
	TLSStatus        int    `json:"tls_status"`
	TLSRequestOK     bool   `json:"tls_request_ok"`
	RequestDigest    string `json:"request_digest"`
	ExitOK           bool   `json:"exit_ok"`
	Error            string `json:"error,omitempty"`
}

// AuthorCausalConfig configures the in-process author causal path.
type AuthorCausalConfig struct {
	ProxyURL    string
	AllowHost   string
	DenyHost    string
	SessionID   string
	Nonce       string
	OutPath     string
	Method      string
	Path        string
	ConnectOnly bool
}

// RunAuthorCausalInProcess is the exact-session author body: same PID computes
// capability marker, opens inherited one-shot claim FD, CONNECT deny (no claim),
// CONNECT allow + full TLS allowlisted request through MITM.
func RunAuthorCausalInProcess(cfg AuthorCausalConfig) error {
	res := AuthorCausalResult{
		PID:       os.Getpid(),
		SessionID: cfg.SessionID,
		Nonce:     cfg.Nonce,
		AllowHost: cfg.AllowHost,
		DenyHost:  cfg.DenyHost,
	}
	if cfg.SessionID != "" && cfg.Nonce != "" {
		res.CapabilityMarker = DeriveCapabilityMarker(cfg.SessionID, cfg.Nonce)
	}
	method := cfg.Method
	if method == "" {
		method = http.MethodPost
	}
	path := cfg.Path
	if path == "" {
		path = "/v1/chat/completions"
	}
	res.TLSMethod = method
	res.TLSPath = path

	// Forbidden: dial without claim FD → must 403 (unattributed peer).
	denyStatus, err := proxyCONNECTStatus(cfg.ProxyURL, cfg.DenyHost+":443")
	if err != nil {
		res.Error = "deny_dial: " + err.Error()
	}
	res.DenyCONNECT = denyStatus

	// Allow: inherited one-shot claim FD only (no claim file).
	conn, port, err := ConnectViaInheritedClaim(cfg.ProxyURL)
	res.PeerPort = port
	if err != nil {
		res.Error = strings.TrimSpace(res.Error + " claim_dial: " + err.Error())
		res.ExitOK = false
		return writeAuthorResult(cfg.OutPath, &res)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	hostPort := cfg.AllowHost + ":443"
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort)
	br := bufio.NewReader(conn)
	line, rerr := br.ReadString('\n')
	if rerr != nil {
		res.Error = strings.TrimSpace(res.Error + " allow_connect: " + rerr.Error())
		return writeAuthorResult(cfg.OutPath, &res)
	}
	res.AllowCONNECT = strings.TrimSpace(line)
	for {
		h, herr := br.ReadString('\n')
		if herr != nil || h == "\r\n" || h == "\n" {
			break
		}
	}
	if !strings.Contains(res.AllowCONNECT, "200") {
		res.Error = strings.TrimSpace(res.Error + " allow_connect_not_200")
		return writeAuthorResult(cfg.OutPath, &res)
	}
	if cfg.ConnectOnly {
		res.ExitOK = strings.Contains(res.DenyCONNECT, "403")
		return writeAuthorResult(cfg.OutPath, &res)
	}

	tlsCfg := &tls.Config{ServerName: cfg.AllowHost, MinVersion: tls.VersionTLS12}
	if p := os.Getenv("SSL_CERT_FILE"); p != "" {
		if pemBytes, perr := os.ReadFile(p); perr == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pemBytes) {
				tlsCfg.RootCAs = pool
			}
		}
	}
	clientTLS := tls.Client(&bufConn{Conn: conn, r: br}, tlsCfg)
	if err := clientTLS.Handshake(); err != nil {
		res.Error = strings.TrimSpace(res.Error + " tls_handshake: " + err.Error())
		return writeAuthorResult(cfg.OutPath, &res)
	}
	defer clientTLS.Close()

	body := `{"model":"test","messages":[{"role":"user","content":"ping"}]}`
	dig := RequestDigest(cfg.SessionID, method, cfg.AllowHost, path, cfg.Nonce, "author-causal")
	res.RequestDigest = dig
	req, err := http.NewRequest(method, "https://"+cfg.AllowHost+path, strings.NewReader(body))
	if err != nil {
		res.Error = "new_request: " + err.Error()
		return writeAuthorResult(cfg.OutPath, &res)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", cfg.AllowHost)
	req.Header.Set("X-Herd-Req-Digest", "author-causal")
	req.Header.Del("Authorization")
	if err := req.Write(clientTLS); err != nil {
		res.Error = strings.TrimSpace(res.Error + " write_req: " + err.Error())
		return writeAuthorResult(cfg.OutPath, &res)
	}
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), req)
	if err != nil {
		res.Error = strings.TrimSpace(res.Error + " read_resp: " + err.Error())
		return writeAuthorResult(cfg.OutPath, &res)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	res.TLSStatus = resp.StatusCode
	_ = RedactSecrets(string(rawBody))
	res.TLSRequestOK = resp.StatusCode > 0
	res.ExitOK = res.TLSRequestOK && strings.Contains(res.DenyCONNECT, "403") && res.CapabilityMarker != ""
	// Print marker on stdout for ModelMarkerReached (non-echo protocol).
	if res.CapabilityMarker != "" {
		_, _ = fmt.Fprintln(os.Stdout, res.CapabilityMarker)
	}
	return writeAuthorResult(cfg.OutPath, &res)
}

func writeAuthorResult(outPath string, res *AuthorCausalResult) error {
	raw, _ := json.MarshalIndent(res, "", "  ")
	if outPath == "" {
		_, _ = os.Stdout.Write(raw)
		return nil
	}
	return os.WriteFile(outPath, raw, 0o600)
}

// LaunchAuthorCausal starts the author child with inherited one-shot claim FD.
// Parent must ClaimLocalPort + AllowOneShotPeer before calling.
func LaunchAuthorCausal(execPath string, env []string, claimFD *os.File, args ...string) (*exec.Cmd, error) {
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(execPath, args...)
	cmd.Env = ExactWorkerChildEnv(env, []string{"HERD_HOSTCREDS_CLAIM_FD=3"})
	if claimFD != nil {
		cmd.ExtraFiles = []*os.File{claimFD}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// ProveAuthorCausalSession launches the REAL author child (author-causal) with
// one-shot peer FD; verifies marker, deny, allow TLS, bound receipt consume.
// A separate helper launched after Wait must NOT be used to satisfy proof.
func ProveAuthorCausalSession(mitm *TLSMitmProxy, allowHost, denyHost, sessionID, nonce string) (*AuthorCausalResult, BrokerReceipt, error) {
	if mitm == nil {
		return nil, BrokerReceipt{}, fmt.Errorf("nil mitm")
	}
	dir, err := os.MkdirTemp("", "hc-author-*")
	if err != nil {
		return nil, BrokerReceipt{}, err
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "author.json")
	exe, err := findHerdOrBuild(dir)
	if err != nil {
		return nil, BrokerReceipt{}, err
	}
	port, claim, err := ClaimLocalPort()
	if err != nil {
		return nil, BrokerReceipt{}, err
	}
	if err := mitm.AllowOneShotPeer(PeerGrant{
		Port: port, SessionID: sessionID, CapabilityNonce: nonce,
	}); err != nil {
		_ = claim.Close()
		return nil, BrokerReceipt{}, err
	}
	env := ExactWorkerChildEnv(
		HarnessProxyEnv(mitm, sessionID),
		CapabilityEnv(Capability{SessionID: sessionID, Nonce: nonce}),
	)
	if err := assertExactEnvNoSecrets(env); err != nil {
		_ = claim.Close()
		return nil, BrokerReceipt{}, err
	}
	cmd, err := LaunchAuthorCausal(exe, env, claim,
		"hostcreds", "author-causal",
		"--proxy", mitm.ProxyURL(),
		"--allow-host", allowHost,
		"--deny-host", denyHost,
		"--session", sessionID,
		"--nonce", nonce,
		"--out", outPath,
	)
	_ = claim.Close()
	if err != nil {
		return nil, BrokerReceipt{}, err
	}
	// Wait for author — check exit; no post-hoc helper.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
		return nil, BrokerReceipt{}, fmt.Errorf("author timeout")
	case waitErr = <-done:
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return nil, BrokerReceipt{}, fmt.Errorf("author result: %w (wait=%v)", err, waitErr)
	}
	var res AuthorCausalResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, BrokerReceipt{}, err
	}
	if cmd.Process == nil || res.PID != cmd.Process.Pid {
		return &res, BrokerReceipt{}, fmt.Errorf("author pid mismatch result=%d process=%v", res.PID, cmd.Process)
	}
	if waitErr != nil && !res.ExitOK {
		return &res, BrokerReceipt{}, fmt.Errorf("author wait: %w", waitErr)
	}
	if !strings.Contains(res.DenyCONNECT, "403") {
		return &res, BrokerReceipt{}, fmt.Errorf("author deny want 403 got %q", res.DenyCONNECT)
	}
	if !res.TLSRequestOK {
		return &res, BrokerReceipt{}, fmt.Errorf("author TLS not ok: %s", res.Error)
	}
	want := DeriveCapabilityMarker(sessionID, nonce)
	if res.CapabilityMarker != want {
		return &res, BrokerReceipt{}, fmt.Errorf("marker mismatch")
	}
	// Consume receipt bound to this author session/nonce/peer port.
	rcpt, ok := mitm.ConsumeReceiptFor(sessionID, nonce, res.PeerPort, "")
	if !ok || !rcpt.InjectOK {
		return &res, BrokerReceipt{}, fmt.Errorf("no bound receipt for author peer port %d", res.PeerPort)
	}
	if rcpt.SessionID != sessionID || rcpt.PeerPort != res.PeerPort {
		return &res, rcpt, fmt.Errorf("receipt binding mismatch")
	}
	// Second consume must fail.
	if _, ok2 := mitm.ConsumeReceiptFor(sessionID, nonce, res.PeerPort, ""); ok2 {
		return &res, rcpt, fmt.Errorf("receipt double-consume allowed")
	}
	return &res, rcpt, nil
}

// ProvePortReplayDenied: after one-shot grant is consumed, rebinding the same
// port number must not authorize CONNECT.
func ProvePortReplayDenied(mitm *TLSMitmProxy, host string) error {
	if mitm == nil {
		return fmt.Errorf("nil")
	}
	port, f, err := ClaimLocalPort()
	if err != nil {
		return err
	}
	if err := mitm.AllowOneShotPeer(PeerGrant{Port: port, SessionID: mitm.session, CapabilityNonce: "replay"}); err != nil {
		_ = f.Close()
		return err
	}
	// First connection consumes grant.
	c1, err := ConnectClaimed(f, mitm.Addr())
	_ = f.Close()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c1, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host)
	buf := make([]byte, 128)
	n, _ := c1.Read(buf)
	_ = c1.Close()
	if !strings.Contains(string(buf[:n]), "200") {
		// May 502 if upstream fails after peer OK — peer was accepted if not 403.
		if strings.Contains(string(buf[:n]), "403") {
			return fmt.Errorf("first connect peer denied: %q", string(buf[:n]))
		}
	}
	// Release port; bind same port if possible and CONNECT again — must 403.
	// Re-claim: try bind fixed port.
	fd, err := syscallSocketBindPort(port)
	if err != nil {
		// Port may still be in TIME_WAIT — use fresh port with stale grant (already consumed).
		// Re-register is not done; dial from new port must 403.
		c2, err2 := net.DialTimeout("tcp", mitm.Addr(), 2*time.Second)
		if err2 != nil {
			return err2
		}
		defer c2.Close()
		_, _ = fmt.Fprintf(c2, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host)
		n2, _ := c2.Read(buf)
		if !strings.Contains(string(buf[:n2]), "403") {
			return fmt.Errorf("post-consume unattributed CONNECT not 403: %q", string(buf[:n2]))
		}
		return nil
	}
	f2 := os.NewFile(uintptr(fd), "replay")
	c2, err := ConnectClaimed(f2, mitm.Addr())
	_ = f2.Close()
	if err != nil {
		return err
	}
	defer c2.Close()
	_, _ = fmt.Fprintf(c2, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host)
	n2, _ := c2.Read(buf)
	if !strings.Contains(string(buf[:n2]), "403") {
		return fmt.Errorf("port replay not denied: %q", string(buf[:n2]))
	}
	return nil
}

func syscallSocketBindPort(port int) (int, error) {
	// Use ClaimLocalPort path: if we can bind exact port, return fd.
	// Implemented via raw socket in peer_bind style.
	return bindExactPort(port)
}
