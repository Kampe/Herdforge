package security

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BrokerControlState is coordinator-only durable control material.
// NEVER written into agent-visible proxy state or agent env/argv/worktree.
type BrokerControlState struct {
	ControlToken  string   `json:"control_token"`
	ControlAddr   string   `json:"control_addr"` // 127.0.0.1:port
	ControlURL    string   `json:"control_url"`  // http://127.0.0.1:port/__herd_control
	Identity      string   `json:"identity"`
	PID           int      `json:"pid"`
	ProxyAddr     string   `json:"proxy_addr"`
	TabID         string   `json:"tab_id"`
	SessionID     string   `json:"session_id"`
	StatePath     string   `json:"state_path"`   // agent-visible proxy state
	ControlPath   string   `json:"control_path"` // this file
	AllowHosts    []string `json:"allow_hosts"`
	StartedAtUnix int64    `json:"started_at_unix"`
	Exe           string   `json:"exe,omitempty"`
	// Bound rebind targets (pre-authorized by coordinator only).
	BoundOldState string `json:"bound_old_state,omitempty"`
	BoundNewState string `json:"bound_new_state,omitempty"`
	BoundNewTab   string `json:"bound_new_tab,omitempty"`
	// HostCreds are coordinator-only model credentials (never agent-visible state).
	// Map host -> full Authorization header value (e.g. "Bearer sk-...").
	HostCreds map[string]string `json:"host_creds,omitempty"`
	// CAPEM is the public MITM CA for agent trust (not secret).
	CAPEM string `json:"ca_pem,omitempty"`
}

// BrokerControlPath returns the coordinator-only control file path.
func BrokerControlPath(sharedRoot, tabID string) string {
	safe := sanitizeAgentName(tabID)
	return filepath.Join(sharedRoot, ".herd", "brokers", safe+".ctrl.json")
}

// WriteBrokerControlState persists coordinator-only control material (0600).
func WriteBrokerControlState(path string, st *BrokerControlState) error {
	if path == "" || st == nil {
		return fmt.Errorf("control state write: path and state required")
	}
	st.ControlPath = path
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, raw, 0o600)
}

// ReadBrokerControlState loads coordinator control material.
func ReadBrokerControlState(path string) (*BrokerControlState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st BrokerControlState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("control state corrupt (preserved): %w", err)
	}
	st.ControlPath = path
	if st.ControlToken == "" || st.ControlAddr == "" || st.Identity == "" {
		return nil, fmt.Errorf("control state incomplete")
	}
	return &st, nil
}

// validateControlEndpoint ensures ControlURL/Addr is loopback, matches the
// bound control listener, and uses the exact control path prefix.
func validateControlEndpoint(controlAddr, controlURL string) error {
	if controlAddr == "" || controlURL == "" {
		return fmt.Errorf("control endpoint missing")
	}
	host, port, err := splitHostPortSafe(controlAddr)
	if err != nil {
		return fmt.Errorf("control addr: %w", err)
	}
	if host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("control addr must be loopback, got %q", host)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("control port invalid")
	}
	u, err := url.Parse(controlURL)
	if err != nil {
		return fmt.Errorf("control url: %w", err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("control url scheme must be http")
	}
	uhost, uport, err := splitHostPortSafe(u.Host)
	if err != nil {
		// host without port
		uhost = u.Host
		uport = ""
	}
	if uhost != "127.0.0.1" && uhost != "localhost" && uhost != "::1" {
		return fmt.Errorf("control url host must be loopback")
	}
	if uport != "" && uport != port {
		return fmt.Errorf("control url port %q != bound %q", uport, port)
	}
	if !strings.HasPrefix(u.Path, "/__herd_control") {
		return fmt.Errorf("control url path must be /__herd_control, got %q", u.Path)
	}
	return nil
}

// pinnedControlHTTPClient dials only the bound control address (no ambient proxy).
func pinnedControlHTTPClient(controlAddr string) *http.Client {
	d := &net.Dialer{Timeout: 2 * time.Second}
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			Dial: func(network, addr string) (net.Conn, error) {
				return d.Dial(network, controlAddr)
			},
		},
	}
}

// BrokerControlPing authenticates with the coordinator ControlToken only.
func BrokerControlPing(ctrl *BrokerControlState) error {
	if ctrl == nil {
		return fmt.Errorf("control ping: nil state")
	}
	if err := validateControlEndpoint(ctrl.ControlAddr, ctrl.ControlURL); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(ctrl.ControlURL, "/")+"/ping", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("herd-ctrl", ctrl.ControlToken)
	resp, err := pinnedControlHTTPClient(ctrl.ControlAddr).Do(req)
	if err != nil {
		return fmt.Errorf("control ping dial: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control ping status %d: %s", resp.StatusCode, truncate(string(body), 120))
	}
	var pb controlPingBody
	if err := json.Unmarshal(body, &pb); err != nil {
		return fmt.Errorf("control ping body: %w", err)
	}
	if !pb.OK || pb.Identity != ctrl.Identity {
		return fmt.Errorf("control identity mismatch")
	}
	if ctrl.PID > 0 && pb.PID != ctrl.PID {
		return fmt.Errorf("control pid mismatch live=%d state=%d", pb.PID, ctrl.PID)
	}
	if ctrl.StartedAtUnix > 0 && pb.StartedAtUnix != ctrl.StartedAtUnix {
		return fmt.Errorf("control incarnation mismatch")
	}
	ctrl.PID = pb.PID
	return nil
}

// BrokerControlShutdown requests authenticated graceful shutdown (control token).
func BrokerControlShutdown(ctrl *BrokerControlState) error {
	if ctrl == nil {
		return fmt.Errorf("control shutdown: nil")
	}
	if err := validateControlEndpoint(ctrl.ControlAddr, ctrl.ControlURL); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(ctrl.ControlURL, "/")+"/shutdown", strings.NewReader(`{}`))
	if err != nil {
		return err
	}
	req.SetBasicAuth("herd-ctrl", ctrl.ControlToken)
	resp, err := pinnedControlHTTPClient(ctrl.ControlAddr).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("control shutdown status %d: %s", resp.StatusCode, truncate(string(b), 120))
	}
	return nil
}

// BrokerControlNotifyRebind updates in-memory tab/session only (no arbitrary FS write).
// old/new state paths must match pre-bound values in control state.
func BrokerControlNotifyRebind(ctrl *BrokerControlState, newTab, newSession string) error {
	if ctrl == nil {
		return fmt.Errorf("control rebind: nil")
	}
	if err := validateControlEndpoint(ctrl.ControlAddr, ctrl.ControlURL); err != nil {
		return err
	}
	if ctrl.BoundNewTab != "" && newTab != ctrl.BoundNewTab {
		return fmt.Errorf("control rebind: tab %q not pre-authorized (bound %q)", newTab, ctrl.BoundNewTab)
	}
	payload, _ := json.Marshal(map[string]string{
		"tab_id":     newTab,
		"session_id": newSession,
		// Explicitly NO state_path — broker must not write arbitrary files.
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(ctrl.ControlURL, "/")+"/rebind", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.SetBasicAuth("herd-ctrl", ctrl.ControlToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pinnedControlHTTPClient(ctrl.ControlAddr).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("control rebind status %d: %s", resp.StatusCode, truncate(string(b), 120))
	}
	return nil
}

// BrokerControlBindRebind pre-authorizes the only allowed rebind targets.
func BrokerControlBindRebind(ctrl *BrokerControlState, oldState, newState, newTab string) error {
	if ctrl == nil {
		return fmt.Errorf("bind rebind: nil")
	}
	if err := validateControlEndpoint(ctrl.ControlAddr, ctrl.ControlURL); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{
		"bound_old_state": oldState,
		"bound_new_state": newState,
		"bound_new_tab":   newTab,
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(ctrl.ControlURL, "/")+"/bind_rebind", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.SetBasicAuth("herd-ctrl", ctrl.ControlToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pinnedControlHTTPClient(ctrl.ControlAddr).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bind rebind status %d: %s", resp.StatusCode, truncate(string(b), 120))
	}
	ctrl.BoundOldState = oldState
	ctrl.BoundNewState = newState
	ctrl.BoundNewTab = newTab
	return WriteBrokerControlState(ctrl.ControlPath, ctrl)
}
