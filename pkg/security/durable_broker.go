package security

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DurableBrokerState is the agent-visible proxy state under .herd/brokers/<tab>.json.
// It MUST NOT contain ControlToken. Coordinator control material lives in
// BrokerControlState (.ctrl.json) only.
type DurableBrokerState struct {
	PID           int      `json:"pid"`
	Addr          string   `json:"addr"` // proxy listen
	Token         string   `json:"token"` // proxy token only
	TabID         string   `json:"tab_id"`
	SessionID     string   `json:"session_id"`
	AllowHosts    []string `json:"allow_hosts"`
	ProxyURL      string   `json:"proxy_url"`
	StatePath     string   `json:"state_path"`
	Identity      string   `json:"identity"`
	StartedAtUnix int64    `json:"started_at_unix"`
	Exe           string   `json:"exe,omitempty"`
	// ControlPath is a relative hint for the coordinator (not a secret).
	ControlPath string `json:"control_path,omitempty"`
}

// BrokerStatePath returns the agent-visible proxy state file for a tab.
func BrokerStatePath(sharedRoot, tabID string) string {
	safe := sanitizeAgentName(tabID)
	return filepath.Join(sharedRoot, ".herd", "brokers", safe+".json")
}

var durableBrokerBinary string
var forceInlineBroker bool

func ForceInlineBrokerForTest(v bool) (restore func()) {
	prev := forceInlineBroker
	forceInlineBroker = v
	return func() { forceInlineBroker = prev }
}

func SetDurableBrokerBinaryForTest(path string) (restore func()) {
	prev := durableBrokerBinary
	durableBrokerBinary = path
	return func() { durableBrokerBinary = prev }
}

// StartDurableBroker launches a detached netbroker-serve process.
func StartDurableBroker(stateDir, tabID, sessionID string, allowHosts []string) (*DurableBrokerState, error) {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(tabID) == "" {
		return nil, fmt.Errorf("durable broker: stateDir and tabID required")
	}
	if sessionID == "" {
		sessionID = "pending"
	}
	statePath := BrokerStatePath(stateDir, tabID)
	ctrlPath := BrokerControlPath(stateDir, tabID)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, err
	}
	if err := StopDurableBroker(statePath); err != nil {
		return nil, fmt.Errorf("durable broker stale stop: %w", err)
	}

	self := durableBrokerBinary
	if self == "" {
		var err error
		self, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("durable broker executable: %w", err)
		}
	}
	args := []string{
		"netbroker-serve",
		"--state", statePath,
		"--control", ctrlPath,
		"--tab", tabID,
		"--session", sessionID,
		"--allow", strings.Join(allowHosts, ","),
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("durable broker start: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitCh:
			if err != nil {
				return nil, fmt.Errorf("durable broker exited before ready: %w", err)
			}
			return nil, fmt.Errorf("durable broker exited before ready")
		default:
		}
		st, err := ReadBrokerState(statePath)
		if err == nil && st.Addr != "" && st.Token != "" && st.PID > 0 && st.Identity != "" {
			ctrl, cerr := ReadBrokerControlState(ctrlPath)
			if cerr != nil {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			if err := BrokerControlPing(ctrl); err != nil {
				_ = forceKillPID(cmd.Process.Pid)
				<-waitCh
				_ = os.Remove(statePath)
				_ = os.Remove(ctrlPath)
				return nil, fmt.Errorf("durable broker control ping: %w", err)
			}
			// Ensure agent state has no control token field leakage.
			if strings.Contains(string(mustRead(statePath)), "control_token") {
				_ = forceKillPID(cmd.Process.Pid)
				<-waitCh
				return nil, fmt.Errorf("agent state leaked control_token")
			}
			return st, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	killErr := forceKillPID(cmd.Process.Pid)
	waitErr := <-waitCh
	if killErr != nil {
		return nil, fmt.Errorf("durable broker ready timeout (kill: %v; wait: %v)", killErr, waitErr)
	}
	return nil, fmt.Errorf("durable broker ready timeout (wait: %v)", waitErr)
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

// StopDurableBroker uses coordinator control channel only (never proxy token).
func StopDurableBroker(statePath string) error {
	if statePath == "" {
		return nil
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Try sibling control file cleanup.
			return stopViaControlSibling(statePath)
		}
		return err
	}
	var st DurableBrokerState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("broker state corrupt (preserved at %s): %w", statePath, err)
	}
	st.StatePath = statePath
	ctrlPath := st.ControlPath
	if ctrlPath == "" {
		ctrlPath = strings.TrimSuffix(statePath, ".json") + ".ctrl.json"
	}
	ctrl, err := ReadBrokerControlState(ctrlPath)
	if err != nil {
		if st.PID <= 0 {
			if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		if processAlive(st.PID) {
			return fmt.Errorf("broker stop: control state missing while pid alive (state preserved): %w", err)
		}
		_ = os.Remove(statePath)
		_ = os.Remove(ctrlPath)
		return nil
	}
	// Pin endpoint before credential-bearing request.
	if err := validateControlEndpoint(ctrl.ControlAddr, ctrl.ControlURL); err != nil {
		return fmt.Errorf("broker stop: control endpoint invalid (state preserved): %w", err)
	}
	if err := BrokerControlPing(ctrl); err != nil {
		if st.PID > 0 && processAlive(st.PID) {
			return fmt.Errorf("broker stop: control auth failed while pid alive (state preserved): %w", err)
		}
		_ = os.Remove(statePath)
		_ = os.Remove(ctrlPath)
		return nil
	}
	if err := BrokerControlShutdown(ctrl); err != nil {
		return fmt.Errorf("broker stop control shutdown (state preserved): %w", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(ctrl.PID) {
			break
		}
		if err := BrokerControlPing(ctrl); err != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if processAlive(ctrl.PID) {
		if err := terminatePID(ctrl.PID); err != nil {
			return fmt.Errorf("broker stop kill after control (state preserved): %w", err)
		}
	}
	if processAlive(ctrl.PID) {
		return fmt.Errorf("broker stop: pid %d still alive (state preserved)", ctrl.PID)
	}
	var first error
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		first = err
	}
	if err := os.Remove(ctrlPath); err != nil && !os.IsNotExist(err) && first == nil {
		first = err
	}
	return first
}

// stopViaControlSibling stops via control file when proxy state is missing.
// Failures propagate; control credentials are only removed after termination is proved.
func stopViaControlSibling(statePath string) error {
	ctrlPath := strings.TrimSuffix(statePath, ".json") + ".ctrl.json"
	ctrl, err := ReadBrokerControlState(ctrlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stopViaControlSibling read control: %w", err)
	}
	if err := validateControlEndpoint(ctrl.ControlAddr, ctrl.ControlURL); err != nil {
		return fmt.Errorf("stopViaControlSibling endpoint: %w", err)
	}
	if err := BrokerControlPing(ctrl); err != nil {
		if ctrl.PID > 0 && processAlive(ctrl.PID) {
			return fmt.Errorf("stopViaControlSibling ping while alive (control preserved): %w", err)
		}
		// Process already dead — remove control only after liveness check.
		if err := os.Remove(ctrlPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := BrokerControlShutdown(ctrl); err != nil {
		return fmt.Errorf("stopViaControlSibling shutdown (control preserved): %w", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(ctrl.PID) {
			break
		}
		if err := BrokerControlPing(ctrl); err != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if processAlive(ctrl.PID) {
		if err := terminatePID(ctrl.PID); err != nil {
			return fmt.Errorf("stopViaControlSibling kill (control preserved): %w", err)
		}
	}
	if processAlive(ctrl.PID) {
		return fmt.Errorf("stopViaControlSibling: pid %d still alive (control preserved)", ctrl.PID)
	}
	if err := os.Remove(ctrlPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

type controlPingBody struct {
	Identity      string `json:"identity"`
	PID           int    `json:"pid"`
	TabID         string `json:"tab_id"`
	StartedAtUnix int64  `json:"started_at_unix"`
	OK            bool   `json:"ok"`
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if isESRCH(err) {
		return false
	}
	return true
}

func terminatePID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil && !isESRCH(err) {
		return err
	}
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	if processAlive(pid) {
		return fmt.Errorf("pid %d survived SIGKILL", pid)
	}
	return nil
}

func forceKillPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(syscall.SIGKILL)
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		return fmt.Errorf("pid %d still alive after SIGKILL", pid)
	}
	return nil
}

func isESRCH(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "no such process") || strings.Contains(err.Error(), "process already finished") || strings.Contains(err.Error(), "os: process already finished"))
}

// RunNetbrokerServe is the durable broker process entrypoint.
// Writes agent-visible proxy state AND coordinator-only control state.
func RunNetbrokerServe(statePath, controlPath, tabID, sessionID, allowCSV string) error {
	hosts := []string{}
	for _, h := range strings.Split(allowCSV, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	hosts = filterLoopbackHosts(hosts)
	b, err := StartHostAllowBroker(hosts)
	if err != nil {
		return err
	}
	ident, err := newBrokerToken()
	if err != nil {
		_ = b.Close()
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		_ = b.Close()
		return fmt.Errorf("netbroker-serve executable: %w", err)
	}
	started := time.Now().Unix()
	if controlPath == "" {
		controlPath = strings.TrimSuffix(statePath, ".json") + ".ctrl.json"
	}
	if err := b.EnableControl(ident, tabID, sessionID, statePath, started); err != nil {
		_ = b.Close()
		return err
	}
	// Agent-visible state: proxy token only.
	st := DurableBrokerState{
		PID:           os.Getpid(),
		Addr:          b.Addr(),
		Token:         b.Token,
		TabID:         tabID,
		SessionID:     sessionID,
		AllowHosts:    hosts,
		ProxyURL:      b.ProxyURL(),
		StatePath:     statePath,
		Identity:      ident,
		StartedAtUnix: started,
		Exe:           exe,
		ControlPath:   controlPath,
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		_ = b.Close()
		return err
	}
	if err := atomicWriteFile(statePath, raw, 0o600); err != nil {
		_ = b.Close()
		return err
	}
	// Coordinator-only control state (never agent env). Load optional HostCreds
	// from the pre-written control path (parent may have seeded credentials).
	var seededCreds map[string]string
	if prev, err := ReadBrokerControlState(controlPath); err == nil && prev != nil {
		seededCreds = prev.HostCreds
		for host, auth := range seededCreds {
			b.SetHostCredential(host, auth)
		}
	}
	_ = b.EnsureCA()
	ctrl := &BrokerControlState{
		ControlToken:  b.ControlToken,
		ControlAddr:   b.ControlAddr(),
		ControlURL:    fmt.Sprintf("http://127.0.0.1:%s/__herd_control", portOnly(b.ControlAddr())),
		Identity:      ident,
		PID:           os.Getpid(),
		ProxyAddr:     b.Addr(),
		TabID:         tabID,
		SessionID:     sessionID,
		StatePath:     statePath,
		ControlPath:   controlPath,
		AllowHosts:    hosts,
		StartedAtUnix: started,
		Exe:           exe,
		HostCreds:     seededCreds,
		CAPEM:         string(b.CAPEM()),
	}
	if err := WriteBrokerControlState(controlPath, ctrl); err != nil {
		_ = b.Close()
		return err
	}
	<-b.Done()
	return nil
}

// SeedCoordinatorHostCreds writes HostCreds into the control state path before
// or after broker start. Never writes into agent-visible proxy state.
func SeedCoordinatorHostCreds(controlPath string, hostCreds map[string]string) error {
	if controlPath == "" || len(hostCreds) == 0 {
		return fmt.Errorf("seed host creds: path and creds required")
	}
	ctrl, err := ReadBrokerControlState(controlPath)
	if err != nil {
		// Pre-seed before process exists: write stub control file.
		ctrl = &BrokerControlState{ControlPath: controlPath, HostCreds: map[string]string{}}
	}
	if ctrl.HostCreds == nil {
		ctrl.HostCreds = map[string]string{}
	}
	for h, a := range hostCreds {
		ctrl.HostCreds[strings.ToLower(h)] = a
	}
	return WriteBrokerControlState(controlPath, ctrl)
}

// InjectHostCredsLive pushes HostCreds to a running broker via control channel
// and persists them in the control file (coordinator only).
func InjectHostCredsLive(ctrl *BrokerControlState, hostCreds map[string]string) error {
	if ctrl == nil || len(hostCreds) == 0 {
		return fmt.Errorf("inject host creds: nil")
	}
	if err := validateControlEndpoint(ctrl.ControlAddr, ctrl.ControlURL); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"host_creds": hostCreds})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(ctrl.ControlURL, "/")+"/host_creds", strings.NewReader(string(payload)))
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
		return fmt.Errorf("host_creds status %d", resp.StatusCode)
	}
	if ctrl.HostCreds == nil {
		ctrl.HostCreds = map[string]string{}
	}
	for h, a := range hostCreds {
		ctrl.HostCreds[strings.ToLower(h)] = a
	}
	return WriteBrokerControlState(ctrl.ControlPath, ctrl)
}

func portOnly(addr string) string {
	_, p, err := splitHostPortSafe(addr)
	if err != nil {
		return "0"
	}
	return p
}

func filterLoopbackHosts(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		hl := strings.ToLower(h)
		if hl == "127.0.0.1" || hl == "localhost" || hl == "::1" {
			continue
		}
		out = append(out, h)
	}
	return out
}

func CloseTabBrokerAt(sharedRoot, tabID string) error {
	return StopDurableBroker(BrokerStatePath(sharedRoot, tabID))
}

func ReadBrokerState(statePath string) (*DurableBrokerState, error) {
	b, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var st DurableBrokerState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("broker state corrupt (preserved): %w", err)
	}
	st.StatePath = statePath
	if st.PID <= 0 || st.Addr == "" || st.Token == "" || st.Identity == "" {
		return nil, fmt.Errorf("broker state incomplete")
	}
	// Reject leaked control secrets in agent-visible state.
	if strings.Contains(string(b), "control_token") {
		return nil, fmt.Errorf("broker state contains control_token leak")
	}
	return &st, nil
}

// RebindBrokerState is transactional and recoverable.
// Coordinator owns all FS writes; control only updates in-memory tab/session.
func RebindBrokerState(oldPath, newPath, tabID, sessionID string) (err error) {
	if oldPath == "" || newPath == "" || tabID == "" {
		return fmt.Errorf("rebind: paths and tabID required")
	}
	st, err := ReadBrokerState(oldPath)
	if err != nil {
		return fmt.Errorf("rebind read: %w", err)
	}
	ctrlPath := st.ControlPath
	if ctrlPath == "" {
		ctrlPath = strings.TrimSuffix(oldPath, ".json") + ".ctrl.json"
	}
	ctrl, err := ReadBrokerControlState(ctrlPath)
	if err != nil {
		return fmt.Errorf("rebind control read: %w", err)
	}
	if err := BrokerControlPing(ctrl); err != nil {
		return fmt.Errorf("rebind control ping: %w", err)
	}
	// Pre-authorize rebind targets on the live broker.
	if err := BrokerControlBindRebind(ctrl, oldPath, newPath, tabID); err != nil {
		return fmt.Errorf("rebind bind: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return err
	}

	// Keep oldPath until new is fully written + verified. Never leave zero paths.
	newState := *st
	newState.TabID = tabID
	if sessionID != "" {
		newState.SessionID = sessionID
	}
	newState.StatePath = newPath
	newCtrlPath := strings.TrimSuffix(newPath, ".json") + ".ctrl.json"
	newState.ControlPath = newCtrlPath
	raw, err := json.MarshalIndent(newState, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(newPath, raw, 0o600); err != nil {
		return fmt.Errorf("rebind write new: %w", err)
	}
	// Write new control file with updated metadata (same control token/addr).
	ctrl.TabID = tabID
	if sessionID != "" {
		ctrl.SessionID = sessionID
	}
	ctrl.StatePath = newPath
	ctrl.ControlPath = newCtrlPath
	ctrl.BoundOldState = oldPath
	ctrl.BoundNewState = newPath
	ctrl.BoundNewTab = tabID
	if err := WriteBrokerControlState(newCtrlPath, ctrl); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("rebind write control: %w", err)
	}

	// Notify live process (in-memory only).
	if err := BrokerControlNotifyRebind(ctrl, tabID, sessionID); err != nil {
		// Recoverable: stop broker using OLD control (still valid listener).
		_ = BrokerControlShutdown(ctrl)
		// Retain old authoritative paths; remove incomplete new.
		_ = os.Remove(newPath)
		_ = os.Remove(newCtrlPath)
		return fmt.Errorf("rebind notify (rolled back via shutdown): %w", err)
	}

	back, err := ReadBrokerState(newPath)
	if err != nil {
		_ = BrokerControlShutdown(ctrl)
		_ = os.Remove(newPath)
		_ = os.Remove(newCtrlPath)
		return fmt.Errorf("rebind readback: %w", err)
	}
	if back.TabID != tabID {
		_ = BrokerControlShutdown(ctrl)
		return fmt.Errorf("rebind readback tab mismatch")
	}
	if back.PID != st.PID || back.Identity != st.Identity {
		_ = BrokerControlShutdown(ctrl)
		return fmt.Errorf("rebind readback identity drift")
	}
	// Immutable policy fields must survive.
	if len(back.AllowHosts) != len(st.AllowHosts) {
		_ = BrokerControlShutdown(ctrl)
		return fmt.Errorf("rebind allow_hosts drift")
	}
	for i := range st.AllowHosts {
		if back.AllowHosts[i] != st.AllowHosts[i] {
			_ = BrokerControlShutdown(ctrl)
			return fmt.Errorf("rebind allow_hosts content drift")
		}
	}
	if st.Exe != "" && back.Exe != st.Exe {
		_ = BrokerControlShutdown(ctrl)
		return fmt.Errorf("rebind exe drift")
	}
	// Live ping via NEW control path.
	newCtrl, err := ReadBrokerControlState(newCtrlPath)
	if err != nil {
		_ = BrokerControlShutdown(ctrl)
		return fmt.Errorf("rebind new control read: %w", err)
	}
	if err := BrokerControlPing(newCtrl); err != nil {
		_ = BrokerControlShutdown(ctrl)
		return fmt.Errorf("rebind post-ping: %w", err)
	}
	// Only now remove old paths.
	if oldPath != newPath {
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			// New is authoritative; old residual is non-fatal but reported.
			return fmt.Errorf("rebind remove old (new authoritative): %w", err)
		}
		oldCtrl := strings.TrimSuffix(oldPath, ".json") + ".ctrl.json"
		if oldCtrl != newCtrlPath {
			_ = os.Remove(oldCtrl)
		}
	}
	return nil
}

func ParseBrokerPort(addr string) (int, error) {
	_, portStr, err := splitHostPortSafe(addr)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(portStr)
}

// BrokerLaunch is the result of starting durable or inline broker.
type BrokerLaunch struct {
	ProxyURL   string
	Endpoint   string
	Inline     *HostAllowBroker
	StatePath  string
	ControlPath string
	AltPaths   []string
	TabKey     string
	Identity   string
	PID        int
}

func (bl *BrokerLaunch) Close() error {
	if bl == nil {
		return nil
	}
	if bl.Inline != nil {
		return bl.Inline.Close()
	}
	var first error
	seen := map[string]struct{}{}
	paths := append([]string{bl.StatePath}, bl.AltPaths...)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if err := StopDurableBroker(p); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func preferDurableBroker() bool {
	if forceInlineBroker {
		return false
	}
	if durableBrokerBinary != "" {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	return !strings.HasSuffix(self, ".test")
}

func StartBrokerForLaunch(sharedRoot, tabKey, sessionID string, hosts []string) (*BrokerLaunch, error) {
	// Production durable brokers never allow loopback (localhost canary).
	// Inline/hermetic probe (forceInlineBroker) may retain loopback HostCreds targets.
	if preferDurableBroker() {
		dHosts := filterLoopbackHosts(hosts)
		if len(dHosts) == 0 {
			return nil, fmt.Errorf("broker: no non-loopback allow hosts")
		}
		st, err := StartDurableBroker(sharedRoot, tabKey, sessionID, dHosts)
		if err != nil {
			return nil, err
		}
		return &BrokerLaunch{
			ProxyURL:    st.ProxyURL,
			Endpoint:    st.Addr,
			StatePath:   st.StatePath,
			ControlPath: st.ControlPath,
			TabKey:      tabKey,
			Identity:    st.Identity,
			PID:         st.PID,
		}, nil
	}
	if !forceInlineBroker {
		hosts = filterLoopbackHosts(hosts)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("broker: no non-loopback allow hosts")
	}
	b, err := StartHostAllowBroker(hosts)
	if err != nil {
		return nil, err
	}
	return &BrokerLaunch{
		ProxyURL: b.ProxyURL(),
		Endpoint: b.Addr(),
		Inline:   b,
		TabKey:   tabKey,
	}, nil
}

func ProveDurableBrokerDeny(endpoint, proxyURL, deniedHost string) error {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return fmt.Errorf("bad proxy url")
	}
	pass, _ := u.User.Password()
	basic := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pass))
	c, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_, _ = fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\nProxy-Authorization: Basic %s\r\n\r\n", deniedHost, deniedHost, basic)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, "403") {
		return fmt.Errorf("deny want 403 got %q", strings.TrimSpace(line))
	}
	c2, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
	if err != nil {
		return err
	}
	defer c2.Close()
	_, _ = fmt.Fprintf(c2, "CONNECT 127.0.0.1:9 HTTP/1.1\r\nHost: 127.0.0.1:9\r\nProxy-Authorization: Basic %s\r\n\r\n", basic)
	line2, err := bufio.NewReader(c2).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line2, "403") {
		return fmt.Errorf("localhost canary want 403 got %q", strings.TrimSpace(line2))
	}
	return nil
}

func splitHostPortSafe(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("no port in %q", addr)
}
