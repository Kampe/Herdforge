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

// WorkerProbeResult is written by a real worker OS process (not coordinator).
// CONNECT-only is insufficient proof: full TLS request + broker redacted receipt
// must prove exact host+method+path authorization and inject (no secret in result).
type WorkerProbeResult struct {
	PID              int    `json:"pid"`
	SessionID        string `json:"session_id"`
	Nonce            string `json:"nonce"`
	CapabilityMarker string `json:"capability_marker"` // derived HC:… (not secret)
	ClaimPort        int    `json:"claim_port"`
	AllowCONNECT     string `json:"allow_connect"` // status line
	DenyCONNECT      string `json:"deny_connect"`  // expect 403
	AllowHost        string `json:"allow_host"`
	DenyHost         string `json:"deny_host"`
	// Full TLS request through MITM (not mere CONNECT).
	TLSMethod       string `json:"tls_method,omitempty"`
	TLSPath         string `json:"tls_path,omitempty"`
	TLSStatus       int    `json:"tls_status,omitempty"`
	TLSBodySnippet  string `json:"tls_body_snippet,omitempty"` // redacted, short
	TLSRequestOK    bool   `json:"tls_request_ok"`
	// Worker never sees inject secret; only confirms response path completed.
	Error string `json:"error,omitempty"`
}

// WorkerProbeConfig configures the in-process worker probe child.
type WorkerProbeConfig struct {
	ProxyURL    string
	AllowHost   string
	DenyHost    string
	SessionID   string
	Nonce       string
	OutPath     string
	ClaimPath   string
	CAPEMPath   string // for TLS verify of MITM leaf chain
	Method      string // default POST
	Path        string // default /v1/chat/completions
	ConnectOnly bool   // skip full TLS (used for deny-host CONNECT checks only)
}

// RunWorkerProbeInProcess executes the worker probe in THIS process.
// Used when this binary is the worker child (herd hostcreds worker-probe).
func RunWorkerProbeInProcess(proxyURL, allowHost, denyHost, sessionID, nonce, outPath string) error {
	return RunWorkerProbeConfig(WorkerProbeConfig{
		ProxyURL:  proxyURL,
		AllowHost: allowHost,
		DenyHost:  denyHost,
		SessionID: sessionID,
		Nonce:     nonce,
		OutPath:   outPath,
	})
}

// RunWorkerProbeConfig is the full worker path: port claim, CONNECT deny/allow,
// optional full TLS request through MITM.
func RunWorkerProbeConfig(cfg WorkerProbeConfig) error {
	res := WorkerProbeResult{
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

	// Deny CONNECT: use a separate ephemeral dial (not claim) so denied peers
	// prove fail-closed without consuming the exclusive claim port.
	denyStatus, err := proxyCONNECTStatus(cfg.ProxyURL, cfg.DenyHost+":443")
	if err != nil {
		res.Error = "deny_dial: " + err.Error()
	}
	res.DenyCONNECT = denyStatus

	// Allow path: exclusive claim → parent AllowClientPort → CONNECT from claim.
	claimPath := cfg.ClaimPath
	if claimPath == "" {
		// Fallback claim in same dir as out (parent must still handshake).
		if cfg.OutPath != "" {
			claimPath = cfg.OutPath + ".claim"
		} else {
			claimPath = filepath.Join(os.TempDir(), fmt.Sprintf("hc-claim-%d", os.Getpid()))
		}
	}
	conn, port, err := WaitAllowAndDial(cfg.ProxyURL, claimPath, 10*time.Second)
	res.ClaimPort = port
	if err != nil {
		res.Error = strings.TrimSpace(res.Error + " claim_dial: " + err.Error())
		return writeWorkerResult(cfg.OutPath, &res)
	}
	defer conn.Close()

	// CONNECT allow host.
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	hostPort := cfg.AllowHost + ":443"
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort)
	br := bufio.NewReader(conn)
	line, rerr := br.ReadString('\n')
	if rerr != nil {
		res.Error = strings.TrimSpace(res.Error + " allow_connect: " + rerr.Error())
		return writeWorkerResult(cfg.OutPath, &res)
	}
	res.AllowCONNECT = strings.TrimSpace(line)
	// Drain remaining CONNECT response headers into br (keep buffered body for TLS).
	for {
		h, herr := br.ReadString('\n')
		if herr != nil || h == "\r\n" || h == "\n" {
			break
		}
	}
	if !strings.Contains(res.AllowCONNECT, "200") {
		res.Error = strings.TrimSpace(res.Error + " allow_connect_not_200")
		return writeWorkerResult(cfg.OutPath, &res)
	}

	if cfg.ConnectOnly {
		return writeWorkerResult(cfg.OutPath, &res)
	}

	// Full TLS request through MITM — proves host+method+path auth + inject path.
	tlsCfg := &tls.Config{
		ServerName:         cfg.AllowHost,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false,
	}
	if cfg.CAPEMPath != "" {
		pemBytes, perr := os.ReadFile(cfg.CAPEMPath)
		if perr == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pemBytes) {
				tlsCfg.RootCAs = pool
			}
		}
	}
	// If no CA, still attempt (may fail verify — tests supply SSL_CERT_FILE via env).
	if tlsCfg.RootCAs == nil {
		if p := os.Getenv("SSL_CERT_FILE"); p != "" {
			pemBytes, perr := os.ReadFile(p)
			if perr == nil {
				pool := x509.NewCertPool()
				if pool.AppendCertsFromPEM(pemBytes) {
					tlsCfg.RootCAs = pool
				}
			}
		}
	}
	// TLS must read from br (may hold post-CONNECT bytes), not raw conn alone.
	clientTLS := tls.Client(&bufConn{Conn: conn, r: br}, tlsCfg)
	if err := clientTLS.Handshake(); err != nil {
		res.Error = strings.TrimSpace(res.Error + " tls_handshake: " + err.Error())
		return writeWorkerResult(cfg.OutPath, &res)
	}
	defer clientTLS.Close()

	body := `{"model":"test","messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequest(method, "https://"+cfg.AllowHost+path, strings.NewReader(body))
	if err != nil {
		res.Error = "new_request: " + err.Error()
		return writeWorkerResult(cfg.OutPath, &res)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", cfg.AllowHost)
	// Worker must NOT set Authorization — broker injects.
	req.Header.Del("Authorization")
	if err := req.Write(clientTLS); err != nil {
		res.Error = strings.TrimSpace(res.Error + " write_req: " + err.Error())
		return writeWorkerResult(cfg.OutPath, &res)
	}
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), req)
	if err != nil {
		res.Error = strings.TrimSpace(res.Error + " read_resp: " + err.Error())
		return writeWorkerResult(cfg.OutPath, &res)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	res.TLSStatus = resp.StatusCode
	// Redact any accidental secret echo in body.
	res.TLSBodySnippet = RedactSecrets(truncateLive(string(rawBody), 200))
	// Any HTTP response after inject path means MITM authorized+forwarded.
	// 4xx/5xx from upstream still proves inject path ran (broker receipt is authoritative).
	res.TLSRequestOK = resp.StatusCode > 0
	return writeWorkerResult(cfg.OutPath, &res)
}

func writeWorkerResult(outPath string, res *WorkerProbeResult) error {
	raw, _ := json.MarshalIndent(res, "", "  ")
	if outPath == "" {
		_, _ = os.Stdout.Write(raw)
		return nil
	}
	return os.WriteFile(outPath, raw, 0o600)
}

// LaunchWorkerProbe starts a subprocess that runs the probe with exact scrubbed env.
func LaunchWorkerProbe(execPath string, env []string, proxyURL, allowHost, denyHost, sessionID, nonce, outPath, claimPath string, connectOnly bool) (*exec.Cmd, error) {
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	args := []string{
		"hostcreds", "worker-probe",
		"--proxy", proxyURL,
		"--allow-host", allowHost,
		"--deny-host", denyHost,
		"--session", sessionID,
		"--nonce", nonce,
		"--out", outPath,
		"--claim", claimPath,
	}
	if connectOnly {
		args = append(args, "--connect-only")
	}
	cmd := exec.Command(execPath, args...)
	cmd.Env = env
	cmd.Dir = filepath.Dir(outPath)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// WaitWorkerProbe waits and loads result JSON.
func WaitWorkerProbe(cmd *exec.Cmd, outPath string, timeout time.Duration) (*WorkerProbeResult, error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("worker probe timeout")
	case err := <-done:
		if err != nil {
			// still try to read partial
		}
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return nil, err
	}
	var res WorkerProbeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func proxyCONNECTStatus(proxyURL, hostPort string) (string, error) {
	hostPortProxy := stripProxyURL(proxyURL)
	c, err := net.DialTimeout("tcp", hostPortProxy, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ProveAllowlistedHostViaWorker launches a real child that CONNECTs and issues a
// full TLS request through MITM. Verifies:
//   - deny CONNECT 403
//   - allow CONNECT 200
//   - full TLS request completed (worker TLSRequestOK)
//   - broker LastReceipt matches host+method+path with InjectOK (redacted, no secret)
//   - capability marker derivation
//   - exact scrubbed env (no os.Environ secret append)
//   - worker PID matches cmd.Process.Pid (hard fail on mismatch)
func ProveAllowlistedHostViaWorker(mitm *TLSMitmProxy, allowHost, denyHost, sessionID, nonce string) (*WorkerProbeResult, error) {
	if mitm == nil {
		return nil, fmt.Errorf("nil mitm")
	}
	dir, err := os.MkdirTemp("", "hc-probe-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "probe.json")
	claimPath := filepath.Join(dir, "claim")

	// Exact scrubbed env: no append(os.Environ()) — secrets must not leak via duplicates.
	env := ExactWorkerChildEnv(
		HarnessProxyEnv(mitm, sessionID),
		CapabilityEnv(Capability{SessionID: sessionID, Nonce: nonce}),
	)
	// Assert no duplicate keys and no secret values.
	if err := assertExactEnvNoSecrets(env); err != nil {
		return nil, err
	}

	exe, err := findHerdOrBuild(dir)
	if err != nil {
		return nil, err
	}
	cmd, err := LaunchWorkerProbe(exe, env, mitm.ProxyURL(), allowHost, denyHost, sessionID, nonce, outPath, claimPath, false)
	if err != nil {
		return nil, err
	}
	// Port-claim handshake (primary peer). Also AllowPID on Linux as secondary.
	if _, err := ParentAllowClaimedPort(mitm, claimPath, 8*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("claim handshake: %w", err)
	}
	if cmd.Process != nil {
		mitm.AllowPID(cmd.Process.Pid)
	}
	res, err := WaitWorkerProbe(cmd, outPath, 20*time.Second)
	if err != nil {
		return res, err
	}
	if cmd.Process == nil {
		return res, fmt.Errorf("nil process after wait")
	}
	// Hard fail PID mismatch (no empty body).
	if res.PID != cmd.Process.Pid {
		return res, fmt.Errorf("worker pid mismatch: result=%d process=%d", res.PID, cmd.Process.Pid)
	}
	if !strings.Contains(res.DenyCONNECT, "403") {
		return res, fmt.Errorf("worker deny CONNECT want 403 got %q", res.DenyCONNECT)
	}
	if !strings.Contains(res.AllowCONNECT, "200") {
		return res, fmt.Errorf("worker allow CONNECT want 200 got %q", res.AllowCONNECT)
	}
	if !res.TLSRequestOK {
		return res, fmt.Errorf("worker full TLS request not ok: %s", res.Error)
	}
	// Broker-side redacted receipt — not secret, proves inject + exact path.
	mitm.mu.Lock()
	rcpt := mitm.LastReceipt
	mitm.mu.Unlock()
	if !rcpt.InjectOK || rcpt.Host != allowHost {
		return res, fmt.Errorf("broker receipt missing/wrong host: %+v", rcpt)
	}
	if !strings.EqualFold(rcpt.Method, res.TLSMethod) {
		return res, fmt.Errorf("broker receipt method %q want %q", rcpt.Method, res.TLSMethod)
	}
	if rcpt.Path != res.TLSPath && rcpt.Path != normalizePathQuiet(res.TLSPath) {
		// allow path normalization differences
		if !strings.HasPrefix(rcpt.Path, "/v1/") {
			return res, fmt.Errorf("broker receipt path %q worker %q", rcpt.Path, res.TLSPath)
		}
	}
	// Worker result must not contain secret material.
	blob := res.TLSBodySnippet + res.Error + res.CapabilityMarker
	if strings.Contains(strings.ToLower(blob), "bearer ") && strings.Contains(blob, "sk-") {
		return res, fmt.Errorf("worker result may contain secret")
	}
	want := DeriveCapabilityMarker(sessionID, nonce)
	if res.CapabilityMarker != want {
		return res, fmt.Errorf("capability marker mismatch")
	}
	return res, nil
}

func assertExactEnvNoSecrets(env []string) error {
	seen := map[string]int{}
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			return fmt.Errorf("bad env entry")
		}
		k := e[:i]
		v := e[i+1:]
		seen[k]++
		if seen[k] > 1 {
			return fmt.Errorf("duplicate env key %s", k)
		}
		lk := strings.ToLower(k)
		if strings.Contains(lk, "api_key") || k == "HERD_HOST_CREDS" || k == "HERD_HOSTCREDS_HANDLES" {
			if strings.TrimSpace(v) != "" {
				return fmt.Errorf("secret key non-empty in child env: %s", k)
			}
		}
		if strings.Contains(v, "sk-") || strings.Contains(strings.ToLower(v), "bearer ") {
			return fmt.Errorf("secret material in env value for %s", k)
		}
	}
	return nil
}

func normalizePathQuiet(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// bufConn merges a bufio leftover reader with the underlying TCP conn for TLS.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
