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
//	hosted_process_gid: true
//	tab_create_uid_flag: one of --uid | --run-as-uid | --hosted-uid
//	process_lineage_proof: true
//	agent_start_uid_flag: optional; defaults to tab_create_uid_flag
//
// When isolation is required, task tab create and agent start fail closed
// unless negotiation succeeds and post-start process-info proves shell,
// foreground, process-group members, and shell descendants run as BuilderUID.
// Proof failure terminates exact bound PIDs only after revalidating start
// tokens (procsignal FAC-174) and closes the orphan tab.

const (
	// EnvBuilderUID is the kernel UID the daemon must host workers as.
	EnvBuilderUID = "HERD_BUILDER_UID"
	// EnvRequireHostedUID forces the hosted-UID path when set to "1".
	// A valid HERD_BUILDER_UID distinct from the coordinator is still required
	// at the capability gate; force-on with misconfiguration fails closed.
	EnvRequireHostedUID = "HERD_REQUIRE_HOSTED_UID"
)

var (
	// ErrHostedUIDUnsupported is returned when isolation is required but the
	// live herdr daemon has no structured hosted-UID capability (FAC-172).
	ErrHostedUIDUnsupported = errors.New(
		"herdr: hosted process UID spawn unsupported (FAC-172 BLOCKED): " +
			"daemon must spawn tab shell/agent/descendants as HERD_BUILDER_UID; " +
			"required structured capability via `herdr api capabilities --json` " +
			"(hosted_process_uid + hosted_process_gid + allowlisted uid flags + process_lineage_proof); " +
			"CLI-only setuid and help-text scraping are not enforcement",
	)

	// ErrHostedUIDProofFailed is returned when process-info proves a wrong UID
	// or lineage cannot be established for a hosted pane.
	ErrHostedUIDProofFailed = errors.New("herdr: hosted UID lineage proof failed")

	// ErrHostedUIDConfig is returned for invalid builder/coordinator identity
	// configuration (uid 0, missing env, builder == coordinator).
	ErrHostedUIDConfig = errors.New("herdr: hosted UID configuration invalid")

	// ErrHostedUIDPIDReuse is returned when a PID's start token changed between
	// observation and action (kill refused — would hit a recycled identity).
	ErrHostedUIDPIDReuse = errors.New("herdr: hosted UID pid start-token reuse")
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
// Tree is the union of shell, foreground inventory, process-group members, and
// shell descendants — each with a race-safe start token.
type HostedPaneIdentity struct {
	PaneID         string
	ShellPID       int
	ShellStart     string
	ProcessGroupID int
	// Tree is the full set under isolation proof/cleanup (deduped by PID).
	Tree []HostedProcessIdentity
	// Foreground is the herdr-reported FG inventory (nil inventory is refused).
	Foreground []HostedProcessIdentity
	// Agent is set by AssertAgentHostedAsBuilder when a routed owner is found.
	Agent *HostedProcessIdentity
}

// HostedProcessIdentity is one process in the hosted pane tree.
type HostedProcessIdentity struct {
	PID        int
	StartToken string
	UID        int
	GID        int
	ParentPID  int
	Role       string // shell | foreground | pgid | descendant | agent
}

// EnvBuilderGID optionally pins the expected primary GID for hosted processes.
// When unset, proof requires a homogeneous tree GID equal to the shell's GID
// and distinct from the coordinator's primary GID (group-drop evidence).
const EnvBuilderGID = "HERD_BUILDER_GID"

// test seams — production defaults use real OS / procsignal.
var (
	processUIDOf    = processUIDOfReal
	processGIDOf    = processGIDOfReal
	signalExact     = procsignal.SignalExactProcess
	osGetuid        = os.Getuid
	osGetgid        = os.Getgid
	osGetpid        = os.Getpid
	readStartTok    = func(pid int) (string, error) { return readPIDStartToken(pid) }
	readParent      = func(pid int) (int, error) { return readPIDParent(pid) }
	listPGIDMembers = listProcessGroupMembersReal
	listChildPIDs   = listDirectChildrenReal
	sleepBrief      = func() { time.Sleep(50 * time.Millisecond) }
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
	return uid, nil
}

// HostedUIDIsolationRequired is true when the fleet has configured a builder
// identity that must be daemon-hosted, or when HERD_REQUIRE_HOSTED_UID=1 forces
// the path. Force-on never silently fail-opens for builder==coordinator: the
// subsequent BuilderUID/capability gates reject that misconfiguration.
func HostedUIDIsolationRequired() bool {
	if os.Getenv(EnvRequireHostedUID) == "1" {
		return true
	}
	uid, err := BuilderUID()
	if err != nil {
		return false
	}
	return uid != osGetuid()
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
	if !c.HostedProcessGID {
		return fmt.Errorf("%w: hosted_process_gid=false", ErrHostedUIDUnsupported)
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

// GetHostedPaneIdentity reads structured pane process-info, binds start tokens,
// and expands the isolation tree to process-group members and shell descendants.
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
				// Pointer so null vs [] is distinguishable (fail closed on null).
				ForegroundProcesses *[]struct {
					PID  int      `json:"pid"`
					Name string   `json:"name"`
					Argv []string `json:"argv"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("%w: process-info JSON: %v", ErrHostedUIDProofFailed, err)
	}
	fgRaw := resp.Result.ProcessInfo.ForegroundProcesses
	if fgRaw == nil {
		return nil, fmt.Errorf("%w: pane process-info returned nil foreground inventory", ErrHostedUIDProofFailed)
	}
	info := &HostedPaneIdentity{
		PaneID:         resp.Result.ProcessInfo.PaneID,
		ShellPID:       resp.Result.ProcessInfo.ShellPID,
		ProcessGroupID: resp.Result.ProcessInfo.ForegroundProcessGroupID,
	}
	byPID := map[int]*HostedProcessIdentity{}
	add := func(pid int, role string) error {
		if pid <= 0 {
			return nil
		}
		if existing, ok := byPID[pid]; ok {
			if existing.Role == "shell" || role == existing.Role {
				return nil
			}
			// Keep the more specific role tag when re-seen.
			if role == "agent" {
				existing.Role = role
			}
			return nil
		}
		tok, err := readStartTok(pid)
		if err != nil || strings.TrimSpace(tok) == "" {
			return fmt.Errorf("%w: start token pid %d role %s: %v", ErrHostedUIDProofFailed, pid, role, err)
		}
		uid, err := processUIDOf(pid)
		if err != nil {
			return fmt.Errorf("%w: uid pid %d role %s: %v", ErrHostedUIDProofFailed, pid, role, err)
		}
		gid, err := processGIDOf(pid)
		if err != nil {
			return fmt.Errorf("%w: gid pid %d role %s: %v", ErrHostedUIDProofFailed, pid, role, err)
		}
		ppid, _ := readParent(pid) // best-effort; zero is ok for shell
		id := HostedProcessIdentity{PID: pid, StartToken: tok, UID: uid, GID: gid, ParentPID: ppid, Role: role}
		byPID[pid] = &id
		return nil
	}

	if info.ShellPID > 0 {
		if err := add(info.ShellPID, "shell"); err != nil {
			return nil, err
		}
		info.ShellStart = byPID[info.ShellPID].StartToken
	}
	for _, p := range *fgRaw {
		if err := add(p.PID, "foreground"); err != nil {
			return nil, err
		}
		if id := byPID[p.PID]; id != nil {
			info.Foreground = append(info.Foreground, *id)
		}
	}
	// Process-group members: reported PGID > 1 must be usable. Silent skip
	// when ValidatePGID fails would drop prior HIGH #3 coverage.
	if info.ProcessGroupID > 1 {
		if err := procsignal.ValidatePGID(info.ProcessGroupID); err != nil {
			return nil, fmt.Errorf("%w: hosted process group %d unusable: %v", ErrHostedUIDProofFailed, info.ProcessGroupID, err)
		}
		members, err := listPGIDMembers(info.ProcessGroupID)
		if err != nil {
			return nil, fmt.Errorf("%w: process-group members pgid %d: %v", ErrHostedUIDProofFailed, info.ProcessGroupID, err)
		}
		for _, pid := range members {
			if err := add(pid, "pgid"); err != nil {
				return nil, err
			}
		}
	}
	// Recursive shell descendants (covers background children outside FG).
	if info.ShellPID > 1 {
		desc, err := collectDescendants(info.ShellPID)
		if err != nil {
			return nil, fmt.Errorf("%w: shell descendants: %v", ErrHostedUIDProofFailed, err)
		}
		for _, pid := range desc {
			if err := add(pid, "descendant"); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range byPID {
		info.Tree = append(info.Tree, *id)
	}
	return info, nil
}

func collectDescendants(root int) ([]int, error) {
	if root <= 1 {
		return nil, nil
	}
	var out []int
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		kids, err := listChildPIDs(cur)
		if err != nil {
			// pgrep exit 1 = no children — not an error for enumeration.
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				continue
			}
			return nil, err
		}
		for _, k := range kids {
			if k <= 1 || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
			queue = append(queue, k)
		}
	}
	return out, nil
}

func listProcessGroupMembersReal(pgid int) ([]int, error) {
	if err := procsignal.ValidatePGID(pgid); err != nil {
		return nil, err
	}
	out, err := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	return parsePIDList(string(out)), nil
}

func listDirectChildrenReal(pid int) ([]int, error) {
	if pid <= 1 {
		return nil, nil
	}
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	return parsePIDList(string(out)), nil
}

func parsePIDList(s string) []int {
	var pids []int
	for _, line := range strings.Fields(s) {
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n <= 0 {
			continue
		}
		pids = append(pids, n)
	}
	return pids
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

func processGIDOfReal(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid")
	}
	out, err := exec.Command("/bin/ps", "-o", "gid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps gid for pid %d: %w", pid, err)
	}
	g, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse gid for pid %d: %w", pid, err)
	}
	return g, nil
}

// expectedBuilderGID resolves the GID the isolation tree must share.
// Explicit HERD_BUILDER_GID wins; otherwise shell GID is the anchor and must
// differ from the coordinator primary GID (evidence of group drop).
func expectedBuilderGID(shellGID int) (int, error) {
	if raw := strings.TrimSpace(os.Getenv(EnvBuilderGID)); raw != "" {
		g, err := strconv.Atoi(raw)
		if err != nil || g < 0 {
			return 0, fmt.Errorf("%w: %s=%q invalid", ErrHostedUIDConfig, EnvBuilderGID, raw)
		}
		return g, nil
	}
	if shellGID < 0 {
		return 0, fmt.Errorf("%w: shell gid unreadable", ErrHostedUIDProofFailed)
	}
	if shellGID == osGetgid() {
		return 0, fmt.Errorf("%w: shell gid %d equals coordinator gid — group drop not proven", ErrHostedUIDProofFailed, shellGID)
	}
	return shellGID, nil
}

// AssertHostedPaneUID proves shell, foreground inventory, process-group
// members, and shell descendants all run as wantUID with non-empty start
// tokens and a homogeneous primary GID (capability hosted_process_gid).
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
	if len(info.Foreground) == 0 {
		return fmt.Errorf("%w: pane %s has empty foreground inventory", ErrHostedUIDProofFailed, paneID)
	}
	if len(info.Tree) == 0 {
		return fmt.Errorf("%w: pane %s has empty isolation tree", ErrHostedUIDProofFailed, paneID)
	}
	shell := HostedProcessIdentity{}
	for _, p := range info.Tree {
		if p.PID == info.ShellPID {
			shell = p
			break
		}
	}
	wantGID, err := expectedBuilderGID(shell.GID)
	if err != nil {
		return err
	}
	for _, p := range info.Tree {
		if strings.TrimSpace(p.StartToken) == "" {
			return fmt.Errorf("%w: pid %d missing start token", ErrHostedUIDProofFailed, p.PID)
		}
		if p.UID != wantUID {
			return fmt.Errorf("%w: hosted process pid %d uid=%d want BuilderUID=%d role=%s (CLI setuid is not isolation)",
				ErrHostedUIDProofFailed, p.PID, p.UID, wantUID, p.Role)
		}
		if p.GID != wantGID {
			return fmt.Errorf("%w: hosted process pid %d gid=%d want BuilderGID=%d role=%s",
				ErrHostedUIDProofFailed, p.PID, p.GID, wantGID, p.Role)
		}
	}
	return nil
}

// AssertAgentHostedAsBuilder proves the exact routed agent process (and its
// wrapper parent when required) runs as BuilderUID on the agent's pane.
// It is not a second full-pane walk: it matches provider/argv like bindToolChildLifecycle.
func AssertAgentHostedAsBuilder(agentName string, builderUID int, provider string, argv []string) error {
	if builderUID <= 0 {
		return fmt.Errorf("%w: invalid builder uid", ErrHostedUIDConfig)
	}
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(provider) == "" || len(argv) == 0 {
		return fmt.Errorf("%w: agent name, provider, and argv required for agent lineage proof", ErrHostedUIDProofFailed)
	}
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
	processes, err := paneProcesses(pane)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHostedUIDProofFailed, err)
	}
	var matches, wrappers []PaneProcess
	for _, p := range processes {
		if nativeCandidate(provider, argv, p) {
			matches = append(matches, p)
		}
		if wrapperCandidate(p) {
			wrappers = append(wrappers, p)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("%w: pane %s has %d exact routed agent processes for %s", ErrHostedUIDProofFailed, pane, len(matches), provider)
	}
	p := matches[0]
	if p.PID <= 0 {
		return fmt.Errorf("%w: agent process identity incomplete", ErrHostedUIDProofFailed)
	}
	tok, err := readStartTok(p.PID)
	if err != nil || strings.TrimSpace(tok) == "" {
		return fmt.Errorf("%w: agent start token pid %d: %v", ErrHostedUIDProofFailed, p.PID, err)
	}
	uid, err := processUIDOf(p.PID)
	if err != nil {
		return fmt.Errorf("%w: agent uid pid %d: %v", ErrHostedUIDProofFailed, p.PID, err)
	}
	if uid != builderUID {
		return fmt.Errorf("%w: agent pid %d uid=%d want BuilderUID=%d", ErrHostedUIDProofFailed, p.PID, uid, builderUID)
	}
	parent, err := readParent(p.PID)
	if err != nil {
		return fmt.Errorf("%w: agent parent pid %d: %v", ErrHostedUIDProofFailed, p.PID, err)
	}
	if strings.EqualFold(provider, "codex") {
		if len(wrappers) != 1 || parent != wrappers[0].PID {
			return fmt.Errorf("%w: codex agent requires exact node wrapper parent", ErrHostedUIDProofFailed)
		}
		w := wrappers[0]
		wTok, err := readStartTok(w.PID)
		if err != nil || strings.TrimSpace(wTok) == "" {
			return fmt.Errorf("%w: wrapper start token pid %d: %v", ErrHostedUIDProofFailed, w.PID, err)
		}
		wUID, err := processUIDOf(w.PID)
		if err != nil {
			return fmt.Errorf("%w: wrapper uid pid %d: %v", ErrHostedUIDProofFailed, w.PID, err)
		}
		if wUID != builderUID {
			return fmt.Errorf("%w: wrapper pid %d uid=%d want BuilderUID=%d", ErrHostedUIDProofFailed, w.PID, wUID, builderUID)
		}
	} else if len(wrappers) > 1 {
		return fmt.Errorf("%w: provider process has ambiguous node wrappers", ErrHostedUIDProofFailed)
	}
	// Ensure the agent PID is also inside the pane isolation tree as BuilderUID.
	if err := AssertHostedPaneUID(pane, builderUID); err != nil {
		return err
	}
	return nil
}

// revalidateStartToken re-reads the PID's start identity and refuses when it
// no longer matches the bound token (PID reuse window).
func revalidateStartToken(pid int, bound string) error {
	if pid <= 0 || strings.TrimSpace(bound) == "" {
		return fmt.Errorf("%w: incomplete identity", ErrHostedUIDPIDReuse)
	}
	cur, err := readStartTok(pid)
	if err != nil {
		// Process gone — not reuse; treat as already reaped.
		return err
	}
	if cur != bound {
		return fmt.Errorf("%w: pid %d start token changed (was %q now %q)", ErrHostedUIDPIDReuse, pid, bound, cur)
	}
	return nil
}

// signalBoundIdentity delivers sig only after start-token revalidation.
// Callers must have proven ownership of the identity (procsignal contract).
func signalBoundIdentity(id HostedProcessIdentity, sig syscall.Signal) error {
	if id.PID == osGetpid() {
		return fmt.Errorf("%w: refusing to signal self", procsignal.ErrUnsafeTarget)
	}
	if err := revalidateStartToken(id.PID, id.StartToken); err != nil {
		// Gone: idempotent success for kill. Reuse: hard refuse.
		if errors.Is(err, ErrHostedUIDPIDReuse) {
			return err
		}
		return nil
	}
	return signalExact(id.PID, sig)
}

// SignalHostedPaneTree terminates shell/foreground/pgid/descendant PIDs with
// start-token revalidation. Does not close the tab (so lifecycle rollback can
// still observe the agent and write tombstones).
func SignalHostedPaneTree(paneID string) error {
	info, err := GetHostedPaneIdentity(paneID)
	if err != nil {
		return err
	}
	var first error
	self := osGetpid()
	seen := map[int]bool{self: true}
	for _, p := range info.Tree {
		if p.PID <= 0 || seen[p.PID] {
			continue
		}
		seen[p.PID] = true
		if err := signalBoundIdentity(p, syscall.SIGTERM); err != nil && first == nil {
			if !errors.Is(err, procsignal.ErrUnsafeTarget) {
				first = err
			}
		}
		_ = signalBoundIdentity(p, syscall.SIGKILL)
	}
	sleepBrief()
	return first
}

// TerminateHostedPaneTree signals the isolation tree then closes the tab.
// Prefer SignalHostedPaneTree + lifecycle rollback + tab close when a tool-child
// lifecycle is bound (so Invalidate/dropToolChild still run).
func TerminateHostedPaneTree(paneID, tabID string) error {
	var first error
	if err := SignalHostedPaneTree(paneID); err != nil {
		first = err
	}
	if tabID != "" {
		// Raw close: isolation proof may fail before tool-child lifecycle is
		// bound, so TabClose's lifecycle authority gate would block cleanup.
		if err := tabCloseRaw(tabID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// FailHostedIsolationProof terminates the hosted pane tree, closes the tab,
// and returns the original proof error (with cleanup detail when cleanup fails).
// Use FailHostedIsolationProofWithLifecycle when a tool-child lifecycle is bound.
func FailHostedIsolationProof(paneID, tabID string, proofErr error) error {
	cleanErr := TerminateHostedPaneTree(paneID, tabID)
	if cleanErr != nil {
		return fmt.Errorf("%w (cleanup: %v)", proofErr, cleanErr)
	}
	return proofErr
}

// FailHostedIsolationProofWithLifecycle signals bound PIDs first, then runs
// lifecycle rollback (tombstone + drop) before any raw tab close, so
// compensateStartedProcessExact can still see the agent and Invalidate runs.
//
// If rollback fails mid-cascade, this path still writes a durable tombstone
// via Invalidate and drops process-local authority — never drop without a
// tombstone attempt (sticky provisional residual of prior HIGH #2).
func FailHostedIsolationProofWithLifecycle(paneID, tabID string, lc ToolChildLifecycle, proofErr error) error {
	var parts []error
	if err := SignalHostedPaneTree(paneID); err != nil {
		parts = append(parts, err)
	}
	if lc != nil {
		if err := rollbackToolChild(tabID, paneID, lc, "hosted-uid-proof-failed"); err != nil {
			parts = append(parts, err)
			// Rollback returned before Invalidate/drop — force tombstone + close.
			if invErr := lc.Invalidate("hosted-uid-proof-failed"); invErr != nil {
				parts = append(parts, invErr)
			} else if verErr := lc.VerifyTerminal(); verErr != nil {
				// VerifyTerminal is strict for JSONL sinks; MemorySink fails it.
				// Tombstone write already happened — retain verify failure only
				// as secondary evidence, never skip Invalidate.
				parts = append(parts, verErr)
			}
			if tabID != "" {
				if cerr := tabCloseRaw(tabID); cerr != nil {
					parts = append(parts, cerr)
				}
			}
			dropToolChild(tabID, paneID)
		}
	} else if tabID != "" {
		if err := tabCloseRaw(tabID); err != nil {
			parts = append(parts, err)
		}
	}
	if len(parts) == 0 {
		return proofErr
	}
	return fmt.Errorf("%w (cleanup: %v)", proofErr, errors.Join(parts...))
}

// hostedUIDLaunchEnv returns env pairs that identify the builder without
// treating env as the security boundary (diagnostic only for the hosted child).
func hostedUIDLaunchEnv(builderUID int) []string {
	return []string{EnvBuilderUID + "=" + strconv.Itoa(builderUID)}
}
