package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/procsignal"
)

// FAC-172: Herdforge client contract for daemon-hosted BuilderUID isolation.
//
// The Herdr daemon owns pane/agent process creation. Running only the herdr
// CLI under setuid/sudo is NOT isolation and is never treated as capability.
// Help-text scraping is NOT capability negotiation.
//
// Required structured capability (herdr api capabilities --json):
//
//	schema_version >= 1
//	hosted_process_uid: true
//	tab_create_uid_flag: one of --uid | --run-as-uid | --hosted-uid
//	process_lineage_proof: true
//	agent_start_uid_flag: optional; defaults to tab_create_uid_flag
//
// When HERD_BUILDER_UID is configured and distinct from the coordinator UID
// (or HERD_REQUIRE_HOSTED_UID=1), task tab create and agent start fail closed
// unless negotiation succeeds and post-start process-info proves every shell
// and foreground PID runs as BuilderUID. Proof failure terminates exact hosted
// PIDs via procsignal (FAC-174) and closes the orphan tab.

const (
	// EnvBuilderUID is the kernel UID the daemon must host workers as.
	EnvBuilderUID = "HERD_BUILDER_UID"
	// EnvRequireHostedUID forces the hosted-UID path even without a distinct
	// builder env when set to "1" (still requires a valid HERD_BUILDER_UID).
	EnvRequireHostedUID = "HERD_REQUIRE_HOSTED_UID"
)

var (
	// ErrHostedUIDUnsupported is returned when isolation is required but the
	// live herdr daemon has no structured hosted-UID capability (FAC-172).
	ErrHostedUIDUnsupported = errors.New(
		"herdr: hosted process UID spawn unsupported (FAC-172 BLOCKED): " +
			"daemon must spawn tab shell/agent/descendants as HERD_BUILDER_UID; " +
			"required structured capability via `herdr api capabilities --json` " +
			"(hosted_process_uid + allowlisted uid flags + process_lineage_proof); " +
			"CLI-only setuid and help-text scraping are not enforcement",
	)

	// ErrHostedUIDProofFailed is returned when process-info proves a wrong UID
	// or lineage cannot be established for a hosted pane.
	ErrHostedUIDProofFailed = errors.New("herdr: hosted UID lineage proof failed")

	// ErrHostedUIDConfig is returned for invalid builder/coordinator identity
	// configuration (uid 0, missing env, builder == coordinator).
	ErrHostedUIDConfig = errors.New("herdr: hosted UID configuration invalid")
)

// HostedUIDCapability is the FAC-172 structured capability document.
// Only this (or an equivalent structured API) enables the hosted-UID path.
type HostedUIDCapability struct {
	SchemaVersion       int    `json:"schema_version"`
	HostedProcessUID    bool   `json:"hosted_process_uid"`
	HostedProcessGID    bool   `json:"hosted_process_gid"`
	TabCreateUIDFlag    string `json:"tab_create_uid_flag"`
	AgentStartUIDFlag   string `json:"agent_start_uid_flag"`
	ProcessLineageProof bool   `json:"process_lineage_proof"`
}

// HostedPaneIdentity is the structured process-lineage readback used for proof.
// StartToken is a race-safe PID start identity (ps lstart); empty is refused
// when process_lineage_proof is required.
type HostedPaneIdentity struct {
	PaneID         string
	ShellPID       int
	ShellStart     string
	ProcessGroupID int
	Foreground     []HostedProcessIdentity
}

// HostedProcessIdentity is one process in the hosted pane tree.
type HostedProcessIdentity struct {
	PID        int
	StartToken string
	UID        int
}

// test seams — production defaults use real OS / procsignal.
var (
	processUIDOf  = processUIDOfReal
	signalExact   = procsignal.SignalExactProcess
	osGetuid      = os.Getuid
	osGetpid      = os.Getpid
	readStartTok  = func(pid int) (string, error) { return readPIDStartToken(pid) }
	sleepBrief    = func() { time.Sleep(50 * time.Millisecond) }
)

// BuilderUID returns the configured HERD_BUILDER_UID or an error when unset/invalid.
func BuilderUID() (int, error) {
	raw := strings.TrimSpace(os.Getenv(EnvBuilderUID))
	if raw == "" {
		return 0, fmt.Errorf("%w: %s is required", ErrHostedUIDConfig, EnvBuilderUID)
	}
	uid, err := strconv.Atoi(raw)
	if err != nil || uid <= 0 {
		return 0, fmt.Errorf("%w: %s=%q must be a positive kernel uid", ErrHostedUIDConfig, EnvBuilderUID, raw)
	}
	if uid == 0 {
		return 0, fmt.Errorf("%w: uid 0 is never a valid BuilderUID", ErrHostedUIDConfig)
	}
	return uid, nil
}

// HostedUIDIsolationRequired is true when the fleet has configured a builder
// identity that must be daemon-hosted (distinct from the coordinator process).
func HostedUIDIsolationRequired() bool {
	uid, err := BuilderUID()
	if err != nil {
		return os.Getenv(EnvRequireHostedUID) == "1"
	}
	if uid == osGetuid() {
		return false
	}
	return true
}

// NegotiateHostedUIDCapability performs structured capability negotiation.
// It does NOT scrape --help text.
func NegotiateHostedUIDCapability() (HostedUIDCapability, error) {
	out, err := runHerdr("api", "capabilities", "--json")
	if err != nil {
		return HostedUIDCapability{}, fmt.Errorf("%w", ErrHostedUIDUnsupported)
	}
	var cap HostedUIDCapability
	if err := json.Unmarshal([]byte(out), &cap); err != nil {
		var env struct {
			Result HostedUIDCapability `json:"result"`
		}
		if err2 := json.Unmarshal([]byte(out), &env); err2 != nil {
			return HostedUIDCapability{}, fmt.Errorf("%w: capabilities JSON unreadable: %v", ErrHostedUIDUnsupported, err)
		}
		cap = env.Result
	}
	if err := validateHostedUIDCapability(cap); err != nil {
		return HostedUIDCapability{}, err
	}
	return cap, nil
}

func validateHostedUIDCapability(c HostedUIDCapability) error {
	if c.SchemaVersion < 1 {
		return fmt.Errorf("%w: capabilities schema_version < 1", ErrHostedUIDUnsupported)
	}
	if !c.HostedProcessUID {
		return fmt.Errorf("%w: hosted_process_uid=false", ErrHostedUIDUnsupported)
	}
	if !c.ProcessLineageProof {
		return fmt.Errorf("%w: process_lineage_proof=false", ErrHostedUIDUnsupported)
	}
	if err := allowlistedUIDFlag(c.TabCreateUIDFlag); err != nil {
		return fmt.Errorf("%w: tab_create_uid_flag %q not allowlisted", ErrHostedUIDUnsupported, c.TabCreateUIDFlag)
	}
	if flag := strings.TrimSpace(c.AgentStartUIDFlag); flag != "" {
		if err := allowlistedUIDFlag(flag); err != nil {
			return fmt.Errorf("%w: agent_start_uid_flag %q not allowlisted", ErrHostedUIDUnsupported, flag)
		}
	}
	return nil
}

func allowlistedUIDFlag(flag string) error {
	switch strings.TrimSpace(flag) {
	case "--uid", "--run-as-uid", "--hosted-uid":
		return nil
	default:
		return fmt.Errorf("not allowlisted")
	}
}

// HerdrSupportsHostedUID is true only after successful structured negotiation.
func HerdrSupportsHostedUID() bool {
	_, err := NegotiateHostedUIDCapability()
	return err == nil
}

// RequireHerdrBuilderSpawnCapability fails closed unless structured FAC-172
// capability negotiation succeeds and builderUID is a valid isolated identity.
func RequireHerdrBuilderSpawnCapability(builderUID int) error {
	if builderUID <= 0 {
		return fmt.Errorf("%w: invalid builder uid %d", ErrHostedUIDConfig, builderUID)
	}
	if builderUID == osGetuid() {
		return fmt.Errorf("%w: builder uid %d equals coordinator uid — not isolation", ErrHostedUIDConfig, builderUID)
	}
	if _, err := NegotiateHostedUIDCapability(); err != nil {
		return fmt.Errorf("%w: cannot host agent as uid %d: %v", ErrHostedUIDUnsupported, builderUID, err)
	}
	return nil
}

func hostedUIDFlagArgs(uid int) ([]string, error) {
	cap, err := NegotiateHostedUIDCapability()
	if err != nil {
		return nil, err
	}
	return []string{cap.TabCreateUIDFlag, strconv.Itoa(uid)}, nil
}

func agentStartUIDFlagArgs(uid int) ([]string, error) {
	cap, err := NegotiateHostedUIDCapability()
	if err != nil {
		return nil, err
	}
	flag := strings.TrimSpace(cap.AgentStartUIDFlag)
	if flag == "" {
		flag = cap.TabCreateUIDFlag
	}
	if err := allowlistedUIDFlag(flag); err != nil {
		return nil, fmt.Errorf("%w: agent_start_uid_flag %q not allowlisted", ErrHostedUIDUnsupported, flag)
	}
	return []string{flag, strconv.Itoa(uid)}, nil
}

// GetHostedPaneIdentity reads structured pane process-info and binds start tokens.
func GetHostedPaneIdentity(paneID string) (*HostedPaneIdentity, error) {
	if strings.TrimSpace(paneID) == "" {
		return nil, fmt.Errorf("%w: pane id required", ErrHostedUIDProofFailed)
	}
	output, err := runHerdr("pane", "process-info", "--pane", paneID)
	if err != nil {
		return nil, fmt.Errorf("%w: process-info: %v: %s", ErrHostedUIDProofFailed, err, output)
	}
	var resp struct {
		Result struct {
			ProcessInfo struct {
				PaneID                   string `json:"pane_id"`
				ShellPID                 int    `json:"shell_pid"`
				ForegroundProcessGroupID int    `json:"foreground_process_group_id"`
				ForegroundProcesses      []struct {
					PID int `json:"pid"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("%w: process-info JSON: %v", ErrHostedUIDProofFailed, err)
	}
	info := &HostedPaneIdentity{
		PaneID:         resp.Result.ProcessInfo.PaneID,
		ShellPID:       resp.Result.ProcessInfo.ShellPID,
		ProcessGroupID: resp.Result.ProcessInfo.ForegroundProcessGroupID,
	}
	if info.ShellPID > 0 {
		tok, err := readStartTok(info.ShellPID)
		if err != nil {
			return nil, fmt.Errorf("%w: shell start token pid %d: %v", ErrHostedUIDProofFailed, info.ShellPID, err)
		}
		info.ShellStart = tok
	}
	for _, p := range resp.Result.ProcessInfo.ForegroundProcesses {
		if p.PID <= 0 {
			continue
		}
		tok, err := readStartTok(p.PID)
		if err != nil {
			return nil, fmt.Errorf("%w: foreground start token pid %d: %v", ErrHostedUIDProofFailed, p.PID, err)
		}
		uid, err := processUIDOf(p.PID)
		if err != nil {
			return nil, fmt.Errorf("%w: foreground uid pid %d: %v", ErrHostedUIDProofFailed, p.PID, err)
		}
		info.Foreground = append(info.Foreground, HostedProcessIdentity{PID: p.PID, StartToken: tok, UID: uid})
	}
	return info, nil
}

func processUIDOfReal(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid")
	}
	out, err := exec.Command("/bin/ps", "-o", "uid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps uid for pid %d: %w", pid, err)
	}
	u, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse uid for pid %d: %w", pid, err)
	}
	return u, nil
}

// AssertHostedPaneUID proves pane shell and every foreground PID run as wantUID
// with race-safe start tokens present (lineage proof, not title/timing heuristics).
func AssertHostedPaneUID(paneID string, wantUID int) error {
	if wantUID <= 0 {
		return fmt.Errorf("%w: invalid want uid", ErrHostedUIDConfig)
	}
	info, err := GetHostedPaneIdentity(paneID)
	if err != nil {
		return err
	}
	if info.ShellPID <= 0 {
		return fmt.Errorf("%w: pane %s has no shell_pid", ErrHostedUIDProofFailed, paneID)
	}
	if strings.TrimSpace(info.ShellStart) == "" {
		return fmt.Errorf("%w: shell pid %d missing start token", ErrHostedUIDProofFailed, info.ShellPID)
	}
	shellUID, err := processUIDOf(info.ShellPID)
	if err != nil {
		return fmt.Errorf("%w: shell pid %d: %v", ErrHostedUIDProofFailed, info.ShellPID, err)
	}
	if shellUID != wantUID {
		// Negative control: CLI running as BuilderUID while daemon hosts as
		// coordinator is rejected here (shell UID != wantUID).
		return fmt.Errorf("%w: hosted shell pid %d uid=%d want BuilderUID=%d (CLI setuid is not isolation)",
			ErrHostedUIDProofFailed, info.ShellPID, shellUID, wantUID)
	}
	if len(info.Foreground) == 0 {
		return fmt.Errorf("%w: pane %s has empty foreground inventory", ErrHostedUIDProofFailed, paneID)
	}
	for _, p := range info.Foreground {
		if strings.TrimSpace(p.StartToken) == "" {
			return fmt.Errorf("%w: pid %d missing start token", ErrHostedUIDProofFailed, p.PID)
		}
		if p.UID != wantUID {
			return fmt.Errorf("%w: hosted process pid %d uid=%d want BuilderUID=%d",
				ErrHostedUIDProofFailed, p.PID, p.UID, wantUID)
		}
	}
	return nil
}

// AssertAgentHostedAsBuilder resolves agent by name and proves pane tree is BuilderUID.
func AssertAgentHostedAsBuilder(agentName string, builderUID int) error {
	agents, err := AgentList()
	if err != nil {
		return err
	}
	var pane string
	for _, a := range agents {
		if a.Name == agentName {
			pane = a.PaneID
			break
		}
	}
	if pane == "" {
		return fmt.Errorf("%w: agent %q not found for hosted-uid proof", ErrHostedUIDProofFailed, agentName)
	}
	return AssertHostedPaneUID(pane, builderUID)
}

// TerminateHostedPaneTree signals exact shell/foreground PIDs reported by
// herdr process-info, then closes the tab. All kills go through procsignal
// (FAC-174): host-wide targets (pid <= 1, self) are refused before syscall.
// Never issues kill(-1) or unvalidated process-group broadcasts.
func TerminateHostedPaneTree(paneID, tabID string) error {
	var first error
	info, err := GetHostedPaneIdentity(paneID)
	if err != nil && first == nil {
		first = err
	}
	self := osGetpid()
	seen := map[int]bool{self: true}
	killOne := func(pid int) {
		if pid <= 0 || seen[pid] {
			return
		}
		seen[pid] = true
		// ValidatePID refuses <=1 and self; SignalExactProcess re-validates.
		if err := signalExact(pid, syscall.SIGTERM); err != nil && first == nil {
			// ESRCH / already gone is fine; keep first non-safety error only
			// when it is not an unsafe-target refusal on a stale pid.
			if !errors.Is(err, procsignal.ErrUnsafeTarget) {
				first = err
			}
		}
		_ = signalExact(pid, syscall.SIGKILL)
	}
	if info != nil {
		killOne(info.ShellPID)
		for _, p := range info.Foreground {
			killOne(p.PID)
		}
	}
	sleepBrief()
	if tabID != "" {
		// Use raw close: isolation proof may fail before tool-child lifecycle
		// is bound, so TabClose's lifecycle authority gate would block cleanup.
		if err := tabCloseRaw(tabID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// FailHostedIsolationProof terminates the hosted pane tree, closes the tab,
// and returns the original proof error (with cleanup detail when cleanup fails).
func FailHostedIsolationProof(paneID, tabID string, proofErr error) error {
	cleanErr := TerminateHostedPaneTree(paneID, tabID)
	if cleanErr != nil {
		return fmt.Errorf("%w (cleanup: %v)", proofErr, cleanErr)
	}
	return proofErr
}

// hostedUIDLaunchEnv returns env pairs that identify the builder without
// treating env as the security boundary (diagnostic only for the hosted child).
func hostedUIDLaunchEnv(builderUID int) []string {
	return []string{EnvBuilderUID + "=" + strconv.Itoa(builderUID)}
}
