package security

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WorkerProbeResult is retained for legacy CONNECT isolation JSON shape.
// Live admission uses AuthorCausalResult from the author child only.
type WorkerProbeResult struct {
	PID              int    `json:"pid"`
	SessionID        string `json:"session_id"`
	Nonce            string `json:"nonce"`
	CapabilityMarker string `json:"capability_marker"`
	AllowCONNECT     string `json:"allow_connect"`
	DenyCONNECT      string `json:"deny_connect"`
	AllowHost        string `json:"allow_host"`
	DenyHost         string `json:"deny_host"`
	TLSMethod        string `json:"tls_method,omitempty"`
	TLSPath          string `json:"tls_path,omitempty"`
	TLSStatus        int    `json:"tls_status,omitempty"`
	TLSBodySnippet   string `json:"tls_body_snippet,omitempty"`
	TLSRequestOK     bool   `json:"tls_request_ok"`
	Error            string `json:"error,omitempty"`
}

// WaitWorkerProbe waits and loads result JSON (author-causal or legacy).
func WaitWorkerProbe(cmd *exec.Cmd, outPath string, timeout time.Duration) (*WorkerProbeResult, error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("worker probe timeout")
	case <-done:
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return nil, err
	}
	var ac AuthorCausalResult
	if err := json.Unmarshal(raw, &ac); err == nil && ac.PID > 0 {
		return &WorkerProbeResult{
			PID: ac.PID, SessionID: ac.SessionID, Nonce: ac.Nonce,
			CapabilityMarker: ac.CapabilityMarker,
			AllowCONNECT:     ac.AllowCONNECT, DenyCONNECT: ac.DenyCONNECT,
			AllowHost: ac.AllowHost, DenyHost: ac.DenyHost,
			TLSMethod: ac.TLSMethod, TLSPath: ac.TLSPath, TLSStatus: ac.TLSStatus,
			TLSRequestOK: ac.TLSRequestOK, Error: ac.Error,
		}, nil
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

// ProveAllowlistedHostViaWorker is not live-admissible (helper split).
// Use ProveAuthorCausalSession.
func ProveAllowlistedHostViaWorker(mitm *TLSMitmProxy, allowHost, denyHost, sessionID, nonce string) (*WorkerProbeResult, error) {
	return nil, &BlockedError{Reason: BlockAbuse, Code: "helper_probe_not_admission"}
}

// bufConn merges a bufio leftover reader with the underlying TCP conn for TLS.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
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
