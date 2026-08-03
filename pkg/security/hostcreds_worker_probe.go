package security

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// WorkerProbeResult is written by a real worker OS process (not coordinator).
type WorkerProbeResult struct {
	PID              int    `json:"pid"`
	SessionID        string `json:"session_id"`
	Nonce            string `json:"nonce"`
	CapabilityMarker string `json:"capability_marker"` // derived HC:…
	AllowCONNECT     string `json:"allow_connect"`     // "200" or status line
	DenyCONNECT      string `json:"deny_connect"`      // expect 403
	AllowHost        string `json:"allow_host"`
	DenyHost         string `json:"deny_host"`
	Error            string `json:"error,omitempty"`
}

// RunWorkerProbeInProcess executes the worker probe in THIS process.
// Used when this binary is the worker child (herd hostcreds worker-probe).
func RunWorkerProbeInProcess(proxyURL, allowHost, denyHost, sessionID, nonce, outPath string) error {
	res := WorkerProbeResult{
		PID:       os.Getpid(),
		SessionID: sessionID,
		Nonce:     nonce,
		AllowHost: allowHost,
		DenyHost:  denyHost,
	}
	if sessionID != "" && nonce != "" {
		res.CapabilityMarker = DeriveCapabilityMarker(sessionID, nonce)
	}
	// Brief pause so parent can AllowPID(this process) after Start (exact-session bind).
	time.Sleep(300 * time.Millisecond)
	// Deny first.
	denyStatus, err := proxyCONNECTStatus(proxyURL, denyHost+":443")
	if err != nil {
		res.Error = "deny_dial: " + err.Error()
	}
	res.DenyCONNECT = denyStatus
	// Allow CONNECT (status only; full TLS may fail for fake hosts).
	allowStatus, err := proxyCONNECTStatus(proxyURL, allowHost+":443")
	if err != nil && allowStatus == "" {
		res.Error = strings.TrimSpace(res.Error + " allow_dial: " + err.Error())
	}
	res.AllowCONNECT = allowStatus

	raw, _ := json.MarshalIndent(res, "", "  ")
	if outPath == "" {
		_, _ = os.Stdout.Write(raw)
		return nil
	}
	return os.WriteFile(outPath, raw, 0o600)
}

// LaunchWorkerProbe starts a subprocess that runs the probe with worker env.
// Uses os.Executable (herd binary) with hostcreds worker-probe subcommand.
func LaunchWorkerProbe(execPath string, env []string, proxyURL, allowHost, denyHost, sessionID, nonce, outPath string) (*exec.Cmd, error) {
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(execPath, "hostcreds", "worker-probe",
		"--proxy", proxyURL,
		"--allow-host", allowHost,
		"--deny-host", denyHost,
		"--session", sessionID,
		"--nonce", nonce,
		"--out", outPath,
	)
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
	u, err := url.Parse(proxyURL)
	if err != nil {
		return "", err
	}
	c, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
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

// ProveAllowlistedHostViaWorker launches a real child that CONNECTs through MITM.
// Production peer PID resolution must allow the child after AllowPID.
// Uses a local TLS origin for allowHost when dialHook is set on mitm — for
// api.x.ai, real network may apply. For tests, pass allowHost that MITM rules include
// and configure mitm.DialUpstream override.
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

	// Child env: proxy + empty keys + capability nonce (not expected marker).
	env := append(os.Environ(), HarnessProxyEnv(mitm, sessionID)...)
	env = append(env, CapabilityEnv(Capability{SessionID: sessionID, Nonce: nonce})...)

	exe, err := findHerdOrBuild(dir)
	if err != nil {
		return nil, err
	}
	cmd, err := LaunchWorkerProbe(exe, env, mitm.ProxyURL(), allowHost, denyHost, sessionID, nonce, outPath)
	if err != nil {
		return nil, err
	}
	// Critical: register child PID before it CONNECTs.
	if cmd.Process != nil {
		mitm.AllowPID(cmd.Process.Pid)
	}
	res, err := WaitWorkerProbe(cmd, outPath, 15*time.Second)
	if err != nil {
		return res, err
	}
	if res.PID != cmd.Process.Pid && cmd.Process != nil {
		// Process.Pid after Wait is still valid
	}
	if !strings.Contains(res.DenyCONNECT, "403") {
		return res, fmt.Errorf("worker deny CONNECT want 403 got %q", res.DenyCONNECT)
	}
	if !strings.Contains(res.AllowCONNECT, "200") {
		return res, fmt.Errorf("worker allow CONNECT want 200 got %q", res.AllowCONNECT)
	}
	want := DeriveCapabilityMarker(sessionID, nonce)
	if res.CapabilityMarker != want {
		return res, fmt.Errorf("capability marker mismatch")
	}
	return res, nil
}

