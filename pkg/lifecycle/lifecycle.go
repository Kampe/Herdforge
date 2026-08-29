// Package lifecycle implements fail-closed standing utilization and handoff
// invariant observation as a Go library. Default is observation ("point"). --act
// is the only mode allowed to kick a settled lane or invoke explicitly configured
// review/next-task hooks. It never mutates Kaneo or spawns an agent by itself.
//
// Ported from chainseer/bin/herd-lifecycle (46K zsh, 888 lines).
package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// ============================================================================
// Types
// ============================================================================

// EventType enumerates the kinds of lifecycle events.
type EventType string

const (
	EventReview  EventType = "review"
	EventVerdict EventType = "verdict"
	EventHandoff EventType = "handoff"
	EventDefault EventType = "default"
)

// EventRecord is the immutable payload published to the event lease store.
type EventRecord struct {
	EventID  string `json:"event_id"`
	Type     string `json:"type"`
	Producer string `json:"producer"`
	Reviewer string `json:"reviewer,omitempty"`
	Task     string `json:"task"`
	Payload  string `json:"payload,omitempty"`
}

// EventLease stores the lease state for an event.
type EventLease struct {
	Event      json.RawMessage `json:"event"`
	OwnerPID   int             `json:"owner_pid"`
	OwnerToken string          `json:"owner_token"`
}

// HookMarker is the state payload for a hook marker file.
type HookMarker struct {
	Event json.RawMessage `json:"event"`
	Hook  string          `json:"hook"`
}

// HookState is the target or ACK file for a hook execution.
type HookState struct {
	EventID string          `json:"event_id"`
	Hook    string          `json:"hook"`
	Event   json.RawMessage `json:"event"`
	State   string          `json:"state"` // "applied" or "acked"
}

// ReclaimGate is the gate file for a reclaim operation.
type ReclaimGate struct {
	Claim      json.RawMessage `json:"claim"`
	OwnerPID   int             `json:"owner_pid"`
	OwnerToken string          `json:"owner_token"`
}

// AgentSnapshot describes one herdr agent's state.
type AgentSnapshot struct {
	Name        string `json:"name"`
	Lane        string `json:"lane,omitempty"`
	Role        string `json:"role,omitempty"`
	Status      string `json:"status"`
	Standing    bool   `json:"standing"`
	Interactive bool   `json:"interactive"`
}

// LaneStateSnapshot describes one lane's state summary.
type LaneStateSnapshot struct {
	Lane        string         `json:"lane"`
	Valid       bool           `json:"valid"`
	StatePath   string         `json:"state_path,omitempty"`
	Active      map[string]any `json:"active,omitempty"`
	ActiveCount int            `json:"active_count"`
	NextStep    string         `json:"next_step,omitempty"`
}

// Summary is the JSON structure returned by the lifecycle observation.
type Summary struct {
	Standing              []AgentSnapshot  `json:"standing"`
	Todo                  int              `json:"todo"`
	Blocked               int              `json:"blocked"`
	BlockedRefs           []string         `json:"blocked_refs"`
	BlockedTargets        []HoldTarget     `json:"blocked_targets,omitempty"`
	Dispatchable          int              `json:"dispatchable"`
	OccupiedRefs          []string         `json:"occupied_refs,omitempty"`
	InProgress            int              `json:"in_progress"`
	StaleInProgress       int              `json:"stale_in_progress"`
	StaleCards            []StaleCard      `json:"stale_cards"`
	Settled               []AgentSnapshot  `json:"settled"`
	Utilized              []string         `json:"utilized"`
	Unutilized            []string         `json:"unutilized"`
	Goaled                []string         `json:"goaled"`
	GoalViolations        []string         `json:"goal_violations"`
	Red                   []string         `json:"red"`
	Critical              []string         `json:"critical,omitempty"`
	Actions               []ActionLogEntry `json:"actions"`
	Healthy               bool             `json:"healthy"`
	StaleActionsExecuted  int              `json:"stale_actions_executed"`
	RoutingActionExecuted bool             `json:"routing_action_executed"`
	EventCompleted        bool             `json:"event_completed"`
}

// StaleCard identifies a stale in-progress card.
type StaleCard struct {
	Ref   string `json:"ref"`
	Owner string `json:"owner"`
	Lane  string `json:"lane"`
	Role  string `json:"role"`
}

type HoldTarget struct {
	Repository string `json:"repository"`
	Owner      string `json:"owner"`
	Lane       string `json:"lane"`
	Task       string `json:"task"`
	Scope      string `json:"scope"`
}

// ActionLogEntry records one act-mode action.
type ActionLogEntry struct {
	Action     string `json:"action"`
	Ref        string `json:"ref,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Lane       string `json:"lane,omitempty"`
	EventID    string `json:"event_id,omitempty"`
	Producer   string `json:"producer,omitempty"`
	Reviewer   string `json:"reviewer,omitempty"`
	Task       string `json:"task,omitempty"`
	Verified   bool   `json:"verified"`
	Idempotent bool   `json:"idempotent,omitempty"`
	Observed   string `json:"observed,omitempty"`
}

// ============================================================================
// Engine
// ============================================================================

// Engine is the lifecycle observation and action engine.
// Zero value is ready to use with defaults.
type Engine struct {
	mu sync.Mutex

	StateRoot   string
	KickBin     string
	HerdrBin    string
	KaneoBin    string
	SendBin     string
	StandingBin string

	AgentsFile string // if set, read agents from file instead of herdr
	BoardFile  string // if set, read board from file instead of kaneo
	EventFile  string // if set, read event payload from file

	Lanes []string // explicit lane snapshot to observe; production injects it from herd.yaml
	// StandingRoster is the validated immutable config snapshot used to
	// classify configured live IDs and their Standing bit. Lifecycle never
	// falls back to a hard-coded legacy roster.
	StandingRoster *CanonicalLaneRegistry

	// Hooks for act mode.
	ReclaimHook  string // executable for reclaim actions
	RoutingHook  string // executable for routing repair
	ReviewHook   string // executable for review handoff
	NextHook     string // executable for next-task handoff
	ReadbackHook string // executable for authoritative readback
	HoldReader   HoldReader
	HoldIdentity func(task, lane, owner string) HoldIdentity
	// HoldLaneResolver maps a configured role label to its canonical lane.
	// Production composition must provide it when HoldReader is configured.
	HoldLaneResolver func(role string) (string, error)
	// HoldLiveAgentResolver maps a typed live standing ID to its configured
	// role and canonical lane. It prevents live IDs from being interpreted as
	// roles or lanes by act-mode side effects.
	HoldLiveAgentResolver func(string) (string, string, error)
	HoldRoles             []string

	// Test seams.
	TestClaimAttempts          int
	TestCrashAfterClaim        bool
	TestCrashAfterHookAck      string
	TestReleaseValidateBarrier string
	TestReclaimValidateBarrier string
}

func (e *Engine) stateRoot() string {
	if e.StateRoot != "" {
		return e.StateRoot
	}
	base := os.Getenv("HERD_LIFECYCLE_STATE_ROOT")
	if base != "" {
		return base
	}
	base = os.Getenv("HERD_STATE_DIR")
	if base != "" {
		return filepath.Join(base, "lane-state")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "chainseer", "herd", "lane-state")
}

func (e *Engine) kickBin() string {
	if e.KickBin != "" {
		return e.KickBin
	}
	return "herd-kick"
}

func (e *Engine) herdrBin() string {
	if e.HerdrBin != "" {
		return e.HerdrBin
	}
	return "herdr"
}

func (e *Engine) kaneoBin() string {
	if e.KaneoBin != "" {
		return e.KaneoBin
	}
	return "kaneo"
}

func (e *Engine) sendBin() string {
	if e.SendBin != "" {
		return e.SendBin
	}
	return filepath.Join(repoRoot(), "bin", "herd-send")
}

func (e *Engine) standingBin() string {
	if e.StandingBin != "" {
		return e.StandingBin
	}
	return filepath.Join(repoRoot(), "bin", "herd-standing")
}

func (e *Engine) lanes() []string {
	if len(e.Lanes) > 0 {
		return e.Lanes
	}
	if e.StandingRoster != nil {
		return e.StandingRoster.LaneNames()
	}
	env := os.Getenv("HERD_LIFECYCLE_LANES")
	if env != "" {
		return strings.Split(env, ",")
	}
	return nil
}

func (e *Engine) claimAttempts() int {
	if e.TestClaimAttempts > 0 {
		return e.TestClaimAttempts
	}
	return 120
}

func repoRoot() string {
	// Try HERD_REPO_ROOT env first.
	if r := os.Getenv("HERD_REPO_ROOT"); r != "" {
		return r
	}
	// Fall back to git. FAC-565: one definition of the working-tree root.
	if top, err := gitroot.Toplevel(context.Background(), "."); err == nil {
		return top
	}
	return "."
}

func randomToken() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	return fmt.Sprintf("%d:%d:%d", os.Getpid(), time.Now().Unix(), n.Int64())
}

// ============================================================================
// Event Leasing — 5-tuple idempotent filesystem event records
// ============================================================================

func eventLeaseRoot(stateRoot string) string {
	v := os.Getenv("HERD_LIFECYCLE_LEASE_ROOT")
	if v != "" {
		return v
	}
	return filepath.Join(stateRoot, "event-leases")
}

// eventLeasePayload reads an event lease payload. Returns nil if not found.
func eventLeasePayload(root, eventID string) ([]byte, error) {
	leaseFile := filepath.Join(root, eventID+".json")
	data, err := os.ReadFile(leaseFile)
	if err == nil {
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// Check for completed event directory.
	dir := filepath.Join(root, eventID)
	statusFile := filepath.Join(dir, "status")
	statusData, err := os.ReadFile(statusFile)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(statusData)) != "complete" {
		return nil, fmt.Errorf("event %s not complete", eventID)
	}
	return os.ReadFile(filepath.Join(dir, "event.json"))
}

// publishEventFile publishes one immutable no-clobber filesystem object.
// Returns 0 on success, 2 if target already exists with matching content.
func publishEventFile(root, filename string, payload []byte) (int, error) {
	target := filepath.Join(root, filename)
	if err := os.MkdirAll(root, 0755); err != nil {
		return 0, err
	}

	// Check if target already exists with matching content.
	if existing, err := os.ReadFile(target); err == nil {
		var a, b any
		json.Unmarshal(existing, &a)
		json.Unmarshal(payload, &b)
		ea, _ := json.Marshal(a)
		eb, _ := json.Marshal(b)
		if string(ea) == string(eb) {
			return 2, nil
		}
		return 0, fmt.Errorf("content mismatch for existing %s", target)
	}

	tmpFile := filepath.Join(root, "."+filename+".tmp")
	if err := os.WriteFile(tmpFile, payload, 0644); err != nil {
		os.Remove(tmpFile)
		return 0, err
	}
	if err := os.Rename(tmpFile, target); err != nil {
		os.Remove(tmpFile)
		return 0, err
	}
	return 0, nil
}

// releaseEventFile removes the target file if it still holds the expected JSON.
// Returns the python-based identity-bound release.
func releaseEventFile(root, filename string, expectedJSON []byte) error {
	target := filepath.Join(root, filename)

	existing, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var a, b any
	if err := json.Unmarshal(existing, &a); err != nil {
		return fmt.Errorf("cannot parse existing: %w", err)
	}
	if err := json.Unmarshal(expectedJSON, &b); err != nil {
		return fmt.Errorf("cannot parse expected: %w", err)
	}
	ea, _ := json.Marshal(a)
	eb, _ := json.Marshal(b)
	if string(ea) != string(eb) {
		return nil // content differs, leave it
	}

	// Identity-bound unlink: stat before and after.
	before, err := os.Stat(target)
	if err != nil {
		return nil
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	// Verify it was our file (not a replacement).
	after, err := os.Stat(target)
	if err == nil {
		if os.SameFile(before, after) {
			return fmt.Errorf("replacement detected for %s", target)
		}
	}
	return nil
}

// eventClaimOwnerLive checks whether the claim's owner PID is still alive.
func eventClaimOwnerLive(root, eventID string) bool {
	file := filepath.Join(root, eventID+".pending.json")
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var lease EventLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return false
	}
	if lease.OwnerPID <= 0 {
		return false
	}
	proc, err := os.FindProcess(lease.OwnerPID)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// reclaimGateOwnerLive checks whether the reclaim gate's owner PID is alive.
func reclaimGateOwnerLive(root, eventID string) bool {
	file := filepath.Join(root, eventID+".reclaim.gate.json")
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var gate ReclaimGate
	if err := json.Unmarshal(data, &gate); err != nil {
		return false
	}
	if gate.OwnerPID <= 0 {
		return false
	}
	proc, err := os.FindProcess(gate.OwnerPID)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// ============================================================================
// Hook Protocol — idempotent target+ACK receipt system
// ============================================================================

// eventHookPayload reads the hook marker payload.
func eventHookPayload(root, eventID, hook string) ([]byte, error) {
	file := filepath.Join(root, eventID+"."+hook+".json")
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var marker HookMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, err
	}
	return marker.Event, nil
}

// validateEventHookTarget validates the target-side hook state.
func validateEventHookTarget(root, eventID, hook string, eventJSON []byte) error {
	targetFile := filepath.Join(root, eventID+"."+hook+".target.json")
	info, err := os.Stat(targetFile)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target file is not a regular file")
	}
	data, err := os.ReadFile(targetFile)
	if err != nil {
		return err
	}
	var state HookState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.EventID != eventID || state.Hook != hook || state.State != "applied" {
		return fmt.Errorf("hook state mismatch")
	}
	// Compare event payloads.
	ea, _ := json.Marshal(state.Event)
	eb, _ := json.Marshal(json.RawMessage(eventJSON))
	if string(ea) != string(eb) {
		return fmt.Errorf("event payload mismatch")
	}
	return nil
}

// validateEventHookAck validates the coordinator ACK state.
func validateEventHookAck(root, eventID, hook string, eventJSON []byte) error {
	ackFile := filepath.Join(root, eventID+"."+hook+".ack.json")
	info, err := os.Stat(ackFile)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("ack file is not a regular file")
	}
	data, err := os.ReadFile(ackFile)
	if err != nil {
		return err
	}
	var state HookState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.EventID != eventID || state.Hook != hook || state.State != "acked" {
		return fmt.Errorf("ack state mismatch")
	}
	ea, _ := json.Marshal(state.Event)
	eb, _ := json.Marshal(json.RawMessage(eventJSON))
	if string(ea) != string(eb) {
		return fmt.Errorf("event payload mismatch")
	}
	return nil
}

// publishEventHookAck publishes the coordinator ACK.
func publishEventHookAck(root, eventID, hook string, eventJSON []byte) error {
	state := HookState{
		EventID: eventID,
		Hook:    hook,
		Event:   json.RawMessage(eventJSON),
		State:   "acked",
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	rc, err := publishEventFile(root, eventID+"."+hook+".ack.json", payload)
	if err != nil {
		return err
	}
	if rc == 2 {
		return validateEventHookAck(root, eventID, hook, eventJSON)
	}
	return nil
}

// runHookWithAck runs a hook command and validates the target/ACK state.
func (e *Engine) runHookWithAck(root, eventID, hook string, eventJSON []byte, hookCmd string, args ...string) error {
	targetFile := filepath.Join(root, eventID+"."+hook+".target.json")
	ackFile := filepath.Join(root, eventID+"."+hook+".ack.json")

	targetExists := false
	ackExists := false
	if _, err := os.Stat(targetFile); err == nil {
		targetExists = true
	}
	if _, err := os.Stat(ackFile); err == nil {
		ackExists = true
	}

	if targetExists || ackExists {
		if err := validateEventHookTarget(root, eventID, hook, eventJSON); err != nil {
			return fmt.Errorf("invalid target-side event state: %s/%s: %w", eventID, hook, err)
		}
		if ackExists {
			if err := validateEventHookAck(root, eventID, hook, eventJSON); err != nil {
				return fmt.Errorf("invalid hook ACK: %s/%s: %w", eventID, hook, err)
			}
		} else {
			if err := publishEventHookAck(root, eventID, hook, eventJSON); err != nil {
				return fmt.Errorf("failed to publish ACK: %w", err)
			}
		}
		return nil
	}

	// Run the hook.
	hookArgs := append(args,
		"--idempotency-key", eventID+":"+hook,
		"--target-state-file", targetFile,
		"--ack-file", ackFile,
		"--event-payload", string(eventJSON),
	)
	cmd := exec.Command(hookCmd, hookArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("hook %s failed: %w\n%s", hookCmd, err, string(out))
	}

	if err := validateEventHookTarget(root, eventID, hook, eventJSON); err != nil {
		return fmt.Errorf("hook did not publish applied target state: %s/%s: %w", eventID, hook, err)
	}

	if _, err := os.Stat(ackFile); err == nil {
		if err := validateEventHookAck(root, eventID, hook, eventJSON); err != nil {
			return fmt.Errorf("hook published malformed ACK: %s/%s: %w", eventID, hook, err)
		}
	} else {
		if err := publishEventHookAck(root, eventID, hook, eventJSON); err != nil {
			return fmt.Errorf("failed to publish ACK after hook: %w", err)
		}
	}
	return nil
}

// publishEventHook publishes the hook completion marker.
func publishEventHook(root, eventID, hook string, eventJSON []byte) error {
	marker := HookMarker{
		Event: json.RawMessage(eventJSON),
		Hook:  hook,
	}
	payload, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	rc, err := publishEventFile(root, eventID+"."+hook+".json", payload)
	if err != nil {
		return err
	}
	if rc == 2 {
		existing, err := eventHookPayload(root, eventID, hook)
		if err != nil {
			return fmt.Errorf("cannot verify existing hook marker: %w", err)
		}
		ea, _ := json.Marshal(json.RawMessage(eventJSON))
		if string(existing) != string(ea) {
			return fmt.Errorf("hook marker payload conflict")
		}
	}
	return nil
}

// ============================================================================
// Claim Handoff — atomic filesystem claim ownership
// ============================================================================

const unsafeEventIDChars = `[^A-Za-z0-9._:-]`

var unsafeEventIDRe = regexp.MustCompile(unsafeEventIDChars)

// reclaimEventClaim performs an identity-bound reclaim of a stale claim.
func (e *Engine) reclaimEventClaim(root, eventID string, expectedJSON []byte) error {
	pendingFile := filepath.Join(root, eventID+".pending.json")
	gateFile := filepath.Join(root, eventID+".reclaim.gate.json")

	// Publish reclaim gate.
	token := randomToken()
	gate := ReclaimGate{
		Claim:      json.RawMessage(expectedJSON),
		OwnerPID:   os.Getpid(),
		OwnerToken: token,
	}
	gatePayload, err := json.Marshal(gate)
	if err != nil {
		return err
	}
	if _, err := publishEventFile(root, eventID+".reclaim.gate.json", gatePayload); err != nil {
		return fmt.Errorf("cannot publish reclaim gate: %w", err)
	}

	// Check pending file exists and is a regular file.
	info, err := os.Stat(pendingFile)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		releaseEventFile(root, eventID+".reclaim.gate.json", gatePayload)
		return fmt.Errorf("pending file is not a regular file")
	}

	// Read pending file and compare.
	pendingData, err := os.ReadFile(pendingFile)
	if err != nil {
		releaseEventFile(root, eventID+".reclaim.gate.json", gatePayload)
		return err
	}
	pa, _ := json.Marshal(json.RawMessage(pendingData))
	ea, _ := json.Marshal(json.RawMessage(expectedJSON))
	if string(pa) != string(ea) {
		releaseEventFile(root, eventID+".reclaim.gate.json", gatePayload)
		return fmt.Errorf("pending content mismatch")
	}

	// Barrier for test.
	if e.TestReclaimValidateBarrier != "" {
		os.WriteFile(e.TestReclaimValidateBarrier, []byte(gateFile+"\n"), 0644)
		for {
			if _, err := os.Stat(e.TestReclaimValidateBarrier); os.IsNotExist(err) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Re-validate after barrier.
	postData, err := os.ReadFile(pendingFile)
	if err != nil || string(postData) != string(pendingData) || eventClaimOwnerLive(root, eventID) {
		releaseEventFile(root, eventID+".reclaim.gate.json", gatePayload)
		if err != nil {
			return err
		}
		return fmt.Errorf("reclaim aborted: concurrent modification")
	}

	// Atomic rename.
	reclaimFile := filepath.Join(root, fmt.Sprintf("%s.reclaiming.%d.%s.json", eventID, os.Getpid(), token))
	if err := os.Rename(pendingFile, reclaimFile); err != nil {
		releaseEventFile(root, eventID+".reclaim.gate.json", gatePayload)
		return fmt.Errorf("reclaim rename failed: %w", err)
	}

	// Verify moved content.
	movedData, err := os.ReadFile(reclaimFile)
	if err != nil || string(movedData) != string(pendingData) {
		os.Remove(reclaimFile)
		releaseEventFile(root, eventID+".reclaim.gate.json", gatePayload)
		return fmt.Errorf("stale claim identity changed during reclaim")
	}
	os.Remove(reclaimFile)

	return releaseEventFile(root, eventID+".reclaim.gate.json", gatePayload)
}

// claimEventHandoff performs the full handoff claim protocol with bounded retry.
// Returns the claim state and claim JSON on success.
func (e *Engine) claimEventHandoff(root, eventID string, eventJSON []byte) (string, []byte, error) {
	pendingFile := filepath.Join(root, eventID+".pending.json")
	gateFile := filepath.Join(root, eventID+".reclaim.gate.json")
	attempts := e.claimAttempts()

	for attempt := 1; attempt <= attempts; attempt++ {
		// Check for completed lease.
		stored, err := eventLeasePayload(root, eventID)
		if err == nil {
			ea, _ := json.Marshal(json.RawMessage(eventJSON))
			if string(stored) != string(ea) {
				return "", nil, fmt.Errorf("event_id payload conflict: %s", eventID)
			}
			return "replay", stored, nil
		}

		// Check reclaim gate.
		if gateData, err := os.ReadFile(gateFile); err == nil {
			if reclaimGateOwnerLive(root, eventID) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			releaseEventFile(root, eventID+".reclaim.gate.json", gateData)
			continue
		}

		// Check pending claim.
		if pendingData, err := os.ReadFile(pendingFile); err == nil {
			var pendingLease EventLease
			if err := json.Unmarshal(pendingData, &pendingLease); err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if string(pendingLease.Event) != string(eventJSON) {
				return "", nil, fmt.Errorf("event_id payload conflict: %s", eventID)
			}
			if eventClaimOwnerLive(root, eventID) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// Owner is dead, reclaim.
			if err := e.reclaimEventClaim(root, eventID, pendingData); err != nil {
				return "", nil, fmt.Errorf("reclaim failed: %w", err)
			}
			continue
		}

		// Publish new claim.
		token := randomToken()
		lease := EventLease{
			Event:      json.RawMessage(eventJSON),
			OwnerPID:   os.Getpid(),
			OwnerToken: token,
		}
		claimPayload, err := json.Marshal(lease)
		if err != nil {
			return "", nil, err
		}
		rc, err := publishEventFile(root, eventID+".pending.json", claimPayload)
		if err != nil {
			return "", nil, fmt.Errorf("handoff claim publication failed: %w", err)
		}
		if rc == 0 {
			return "owned", claimPayload, nil
		}
		// rc == 2 — someone else published first, loop to discover.
	}

	return "", nil, fmt.Errorf("handoff claim remained owned by live caller: %s", eventID)
}

// ============================================================================
// Event validation
// ============================================================================

func eventTypeNeedsReviewer(et string) bool {
	switch et {
	case "review", "verdict", "handoff":
		return true
	}
	return false
}

// validateEventID returns an error if the event ID contains unsafe characters.
func validateEventID(id string) error {
	if unsafeEventIDRe.MatchString(id) {
		return fmt.Errorf("event id contains unsafe characters: %s", id)
	}
	return nil
}

// ============================================================================
// Observation — gather agents, board, and lane state
// ============================================================================

// Observe gathers all state and produces a Summary.
// The act parameter controls whether action side effects are performed.
func (e *Engine) Observe(act bool) (*Summary, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	stateRoot := e.stateRoot()
	leaseRoot := eventLeaseRoot(stateRoot)

	// Gather agents.
	var agentsJSON json.RawMessage
	if e.AgentsFile != "" {
		data, err := os.ReadFile(e.AgentsFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read agents file: %w", err)
		}
		agentsJSON = data
	} else {
		out, err := exec.Command(e.herdrBin(), "agent", "list").Output()
		if err != nil {
			return nil, fmt.Errorf("agent list unavailable: %w", err)
		}
		agentsJSON = out
	}

	// Validate agents payload.
	var agentsWrapper struct {
		Result struct {
			Agents []json.RawMessage `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(agentsJSON, &agentsWrapper); err != nil {
		return nil, fmt.Errorf("invalid agent payload: %w", err)
	}

	// Gather board.
	var boardJSON json.RawMessage
	if e.BoardFile != "" {
		data, err := os.ReadFile(e.BoardFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read board file: %w", err)
		}
		boardJSON = data
	} else {
		out, err := exec.Command(e.kaneoBin(), "task", "list", "--all", "--json").Output()
		if err != nil {
			return nil, fmt.Errorf("board unavailable: %w", err)
		}
		boardJSON = out
	}
	if err := rejectKaneoErrorEnvelope("board observation", boardJSON); err != nil {
		return nil, fmt.Errorf("board unavailable: %w", err)
	}

	// Gather lane states.
	lanes := e.lanes()
	stateEntries := make([]LaneStateSnapshot, 0, len(lanes))
	for _, lane := range lanes {
		entry := e.observeLaneState(lane)
		stateEntries = append(stateEntries, entry)
	}

	// Gather event if specified.
	var eventJSON json.RawMessage
	if e.EventFile != "" {
		data, err := os.ReadFile(e.EventFile)
		if err != nil {
			return nil, fmt.Errorf("unreadable event file: %w", err)
		}
		var evtCheck struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &evtCheck); err != nil || evtCheck.Type == "" {
			return nil, fmt.Errorf("invalid event payload")
		}
		eventJSON = data
	}

	// Compute summary via jq-compatible logic.
	summary := e.computeSummary(agentsWrapper, boardJSON, stateEntries, eventJSON)

	// ====================================================================
	// Act mode
	// ====================================================================
	if act {
		if err := e.executeActMode(stateRoot, leaseRoot, summary, agentsJSON, boardJSON, stateEntries, eventJSON); err != nil {
			return nil, err
		}
	}

	// Compute red codes.
	summary = e.computeRedCodes(summary)

	return summary, nil
}

func (e *Engine) observeLaneState(lane string) LaneStateSnapshot {
	wtRoot := os.Getenv("HERD_STANDING_WT_ROOT")
	if wtRoot == "" {
		wtRoot = filepath.Join(os.Getenv("HOME"), "Personal", "worktrees")
	}

	statePaths := []string{
		filepath.Join(wtRoot, lane, "WORK-STATE.json"),
		filepath.Join(e.stateRoot(), lane, "WORK-STATE.json"),
	}
	progressPaths := []string{
		filepath.Join(wtRoot, lane, "LANE-PROGRESS.md"),
		filepath.Join(e.stateRoot(), lane, "LANE-PROGRESS.md"),
	}

	entry := LaneStateSnapshot{Lane: lane}

	for _, sp := range statePaths {
		data, err := os.ReadFile(sp)
		if err != nil {
			continue
		}
		var state struct {
			Features []struct {
				State    string `json:"state"`
				Behavior string `json:"behavior"`
				ID       string `json:"id"`
			} `json:"features"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		entry.Valid = true
		entry.StatePath = sp
		entry.ActiveCount = 0
		for _, f := range state.Features {
			if f.State == "active" {
				entry.ActiveCount++
				if entry.Active == nil {
					entry.Active = map[string]any{
						"id":       f.ID,
						"state":    f.State,
						"behavior": f.Behavior,
					}
				}
			}
		}
		break
	}

	for _, pp := range progressPaths {
		data, err := os.ReadFile(pp)
		if err != nil {
			continue
		}
		re := regexp.MustCompile(`(?m)^\s*[-*]?\s*(Next best step|Next action):\s*(.*)`)
		matches := re.FindStringSubmatch(string(data))
		if len(matches) >= 3 {
			entry.NextStep = strings.TrimSpace(matches[2])
		}
		break
	}

	return entry
}

type boardCard struct {
	Ref            string   `json:"ref,omitempty"`
	ID             string   `json:"id,omitempty"`
	Key            string   `json:"key,omitempty"`
	Title          string   `json:"title,omitempty"`
	Status         string   `json:"status,omitempty"`
	Column         string   `json:"column,omitempty"`
	State          string   `json:"state,omitempty"`
	Owner          string   `json:"owner,omitempty"`
	Assignee       string   `json:"assignee,omitempty"`
	AssignedTo     string   `json:"assigned_to,omitempty"`
	AssignedAgent  string   `json:"assignedAgent,omitempty"`
	AssigneeName   string   `json:"assigneeName,omitempty"`
	AssigneeID     string   `json:"assigneeId,omitempty"`
	Lane           string   `json:"lane,omitempty"`
	Labels         []any    `json:"labels,omitempty"`
	Blocked        *bool    `json:"blocked,omitempty"`
	BlockedBy      []string `json:"blocked_by,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
	CreatedAt      string   `json:"created_at,omitempty"`
	UpdatedAtCamel string   `json:"updatedAt,omitempty"`
	CreatedAtCamel string   `json:"createdAt,omitempty"`
	ReviewedAt     string   `json:"reviewed_at,omitempty"`
	MergedAt       string   `json:"merged_at,omitempty"`
}

func (e *Engine) computeSummary(agentsWrapper struct {
	Result struct {
		Agents []json.RawMessage `json:"agents"`
	} `json:"result"`
}, boardJSON json.RawMessage, stateEntries []LaneStateSnapshot, eventJSON json.RawMessage) *Summary {
	s := &Summary{}
	liveCounts := make(map[string]int)
	for _, raw := range agentsWrapper.Result.Agents {
		var ag struct {
			Name      string `json:"name"`
			Label     string `json:"label"`
			AgentName string `json:"agent_name"`
			Agent     string `json:"agent"`
		}
		if err := json.Unmarshal(raw, &ag); err != nil {
			continue
		}
		name := ag.Name
		if name == "" {
			name = ag.Label
		}
		if name == "" {
			name = ag.AgentName
		}
		if name == "" {
			name = ag.Agent
		}
		if name != "" {
			liveCounts[strings.ToLower(strings.TrimSpace(name))]++
		}
	}
	reportedDuplicates := make(map[string]bool)

	// Parse agents.
	for _, raw := range agentsWrapper.Result.Agents {
		var ag struct {
			Name              string `json:"name"`
			Label             string `json:"label"`
			AgentName         string `json:"agent_name"`
			Agent             string `json:"agent"`
			Status            string `json:"status"`
			AgentStatus       string `json:"agent_status"`
			Interactive       *bool  `json:"interactive"`
			InteractiveReady  *bool  `json:"interactive_ready"`
			InteractiveReady2 *bool  `json:"interactiveReady"`
		}
		if err := json.Unmarshal(raw, &ag); err != nil {
			continue
		}
		name := ag.Name
		if name == "" {
			name = ag.Label
		}
		if name == "" {
			name = ag.AgentName
		}
		if name == "" {
			name = ag.Agent
		}
		if name == "" {
			continue
		}

		st := ag.Status
		if st == "" {
			st = ag.AgentStatus
		}
		st = strings.ToLower(st)

		var canonical CanonicalLane
		var resolveErr error
		configured := false
		if e.StandingRoster == nil {
			resolveErr = fmt.Errorf("configured standing roster is unavailable")
		} else {
			canonical, resolveErr = e.StandingRoster.ResolveLiveAgentID(name)
			configured = resolveErr == nil
		}
		duplicateKey := strings.ToLower(strings.TrimSpace(name))
		if liveCounts[duplicateKey] > 1 {
			configured = false
			resolveErr = fmt.Errorf("duplicate live agent ID %q", name)
			if !reportedDuplicates[duplicateKey] {
				s.Critical = append(s.Critical, fmt.Sprintf("duplicate_live_agent:%s", name))
				reportedDuplicates[duplicateKey] = true
			}
		}
		isStanding := configured && canonical.Standing

		interactive := false
		if ag.Interactive != nil {
			interactive = *ag.Interactive
		} else if ag.InteractiveReady != nil {
			interactive = *ag.InteractiveReady
		} else if ag.InteractiveReady2 != nil {
			interactive = *ag.InteractiveReady2
		}

		as := AgentSnapshot{
			Name:        name,
			Lane:        canonical.Name,
			Role:        canonical.Role,
			Status:      st,
			Interactive: interactive,
			Standing:    isStanding,
		}
		s.Standing = append(s.Standing, as)
		if resolveErr != nil && liveCounts[duplicateKey] <= 1 {
			s.Critical = append(s.Critical, fmt.Sprintf("unknown_live_agent:%s:%v", name, resolveErr))
		}

		if (st == "idle" || st == "done" || st == "blocked" || st == "unknown") && configured {
			s.Settled = append(s.Settled, as)
		}
	}

	// Parse board cards.
	cards := e.parseCards(boardJSON)

	// Normalize labels and status.
	type cardNorm struct {
		Ref        string
		Status     string
		Labels     []string
		Owner      string
		Lane       string
		Role       string
		Blocked    bool
		UpdatedAt  string
		CreatedAt  string
		ReviewedAt string
		MergedAt   string
		LedgerAt   string
	}
	normalized := make([]cardNorm, 0, len(cards))
	for _, c := range cards {
		ref := c.Ref
		if ref == "" {
			ref = c.ID
		}
		if ref == "" {
			ref = c.Key
		}
		if ref == "" {
			continue
		}

		st := strings.ToLower(c.Status)
		if st == "" {
			st = strings.ToLower(c.Column)
		}
		if st == "" {
			st = strings.ToLower(c.State)
		}

		owner := c.Owner
		if owner == "" {
			owner = c.Assignee
		}
		if owner == "" {
			owner = c.AssignedTo
		}
		if owner == "" {
			owner = c.AssignedAgent
		}
		if owner == "" {
			owner = c.AssigneeName
		}
		if owner == "" {
			owner = c.AssigneeID
		}
		if owner == "" {
			owner = c.Lane
		}
		owner = strings.TrimSpace(owner)

		labels := make([]string, 0, len(c.Labels))
		for _, l := range c.Labels {
			switch v := l.(type) {
			case string:
				labels = append(labels, strings.ToLower(v))
			case map[string]any:
				if n, ok := v["name"].(string); ok {
					labels = append(labels, strings.ToLower(n))
				} else if lb, ok := v["label"].(string); ok {
					labels = append(labels, strings.ToLower(lb))
				}
			}
		}

		blocked := false
		if c.Blocked != nil {
			blocked = *c.Blocked
		}
		if !blocked && len(c.BlockedBy) > 0 {
			blocked = true
		}
		if !blocked {
			for _, l := range labels {
				if l == "blocked" || l == "blocked-by" {
					blocked = true
					break
				}
			}
		}
		updatedAt := c.UpdatedAt
		if updatedAt == "" {
			updatedAt = c.UpdatedAtCamel
		}
		createdAt := c.CreatedAt
		if createdAt == "" {
			createdAt = c.CreatedAtCamel
		}

		normalized = append(normalized, cardNorm{
			Ref:        ref,
			Status:     st,
			Labels:     labels,
			Owner:      owner,
			Lane:       strings.TrimSpace(c.Lane),
			Role:       recognizedRole(labels, e.HoldRoles),
			Blocked:    blocked,
			UpdatedAt:  updatedAt,
			CreatedAt:  createdAt,
			ReviewedAt: c.ReviewedAt,
			MergedAt:   c.MergedAt,
		})
	}

	// Classify cards.
	isTodo := func(st string) bool {
		matched, _ := regexp.MatchString(`to-?do|todo|backlog|planned`, st)
		return matched
	}
	isInProgress := func(st string) bool {
		matched, _ := regexp.MatchString(`in-progress|in_progress|working|started`, st)
		return matched
	}
	isReview := func(st string) bool {
		matched, _ := regexp.MatchString(`in-review|review`, st)
		return matched
	}
	isDone := func(st string) bool {
		matched, _ := regexp.MatchString(`done|complete|closed|merged|shipped`, st)
		return matched
	}
	isBounded := func(labels []string) bool {
		for _, l := range labels {
			if l == "epic" || l == "epic-needs-scoping" || l == "standing-epic" {
				return false
			}
		}
		return true
	}
	liveNames := make(map[string]bool)
	for _, a := range s.Standing {
		if a.Interactive {
			liveNames[a.Name] = true
		}
	}
	liveOwner := func(o string) bool {
		return o != "" && liveNames[o]
	}

	for _, c := range normalized {
		if !isBounded(c.Labels) {
			continue
		}
		if isTodo(c.Status) {
			s.Todo++
			if c.Blocked {
				s.Blocked++
				s.BlockedRefs = append(s.BlockedRefs, c.Ref)
				canonicalLane := c.Lane
				if e.HoldLaneResolver != nil && c.Role != "" {
					if resolved, err := e.HoldLaneResolver(c.Role); err == nil {
						canonicalLane = resolved
					} else {
						canonicalLane = ""
					}
				}
				s.BlockedTargets = append(s.BlockedTargets, HoldTarget{Repository: repoRoot(), Owner: c.Role, Lane: canonicalLane, Task: c.Ref, Scope: "task"})
			} else {
				if e.holdBlocks(context.Background(), c.Role, c.Role, c.Ref) {
					s.OccupiedRefs = append(s.OccupiedRefs, c.Ref)
				} else {
					s.Dispatchable++
				}
			}
		}
		if isInProgress(c.Status) {
			s.InProgress++
			if c.Owner == "" || !liveOwner(c.Owner) {
				s.StaleInProgress++
				s.StaleCards = append(s.StaleCards, StaleCard{Ref: c.Ref, Owner: c.Role, Lane: c.Lane, Role: c.Role})
				s.Unutilized = append(s.Unutilized, c.Ref)
			} else {
				s.Utilized = append(s.Utilized, c.Ref)
			}
		}
		_ = isReview
		_ = isDone
	}

	// Lane state goals.
	for _, entry := range stateEntries {
		if entry.Valid && entry.Active != nil && entry.ActiveCount == 1 {
			behavior, _ := entry.Active["behavior"].(string)
			if behavior != "" && entry.NextStep != "" {
				s.Goaled = append(s.Goaled, entry.Lane)
				continue
			}
		}
		s.GoalViolations = append(s.GoalViolations, entry.Lane)
	}

	return s
}

func recognizedRole(labels, configured []string) string {
	roles := make(map[string]bool, len(configured))
	for _, role := range configured {
		roles[strings.ToLower(strings.TrimSpace(role))] = true
	}
	var role string
	count := 0
	for _, label := range labels {
		if !roles[label] {
			continue
		}
		count++
		if role != "" && role != label {
			return ""
		}
		role = label
	}
	if count != 1 {
		return ""
	}
	return role
}

func (e *Engine) parseCards(boardJSON json.RawMessage) []boardCard {
	// Try array first.
	var cards []boardCard
	if err := json.Unmarshal(boardJSON, &cards); err == nil {
		return cards
	}

	// Try object wrappers.
	var wrapper struct {
		Tasks  []boardCard `json:"tasks"`
		Result struct {
			Tasks []boardCard `json:"tasks"`
		} `json:"result"`
		Items []boardCard `json:"items"`
	}
	if err := json.Unmarshal(boardJSON, &wrapper); err == nil {
		if len(wrapper.Tasks) > 0 {
			return wrapper.Tasks
		}
		if len(wrapper.Result.Tasks) > 0 {
			return wrapper.Result.Tasks
		}
		if len(wrapper.Items) > 0 {
			return wrapper.Items
		}
	}

	return nil
}

// ============================================================================
// Act mode
// ============================================================================

func (e *Engine) executeActMode(stateRoot, leaseRoot string, s *Summary, agentsJSON, boardJSON json.RawMessage, stateEntries []LaneStateSnapshot, eventJSON json.RawMessage) error {
	// 1. Reclaim stale in-progress cards.
	if s.StaleInProgress > 0 {
		reclaimHook := strings.TrimSpace(e.ReclaimHook)
		if reclaimHook == "" {
			return fmt.Errorf("lifecycle: stale reclaim requires a configured repository-approved ReclaimHook; refusing direct board mutation")
		}
		for _, sc := range s.StaleCards {
			if strings.TrimSpace(sc.Role) == "" {
				return fmt.Errorf("lifecycle: stale card %s has no configured role", sc.Ref)
			}
			held, err := e.targetHeld(context.Background(), sc.Role, sc.Ref)
			if err != nil {
				return err
			}
			if held {
				continue
			}
			cmd := exec.Command(reclaimHook,
				"--ref", sc.Ref,
				"--owner", sc.Owner,
				"--reason", "stale in-progress owner",
				"--return-to-to-do",
			)
			mutationOut, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("reclaim hook failed for %s: %w\n%s", sc.Ref, err, string(mutationOut))
			}
			if err := rejectKaneoErrorEnvelope("reclaim status", mutationOut); err != nil {
				return fmt.Errorf("reclaim hook failed for %s: %w", sc.Ref, err)
			}

			// Authoritative readback.
			readbackData, err := e.authoritativeReadback("reclaim", sc.Ref, "", "", "")
			if err != nil {
				return fmt.Errorf("reclaim readback failed for %s: %w", sc.Ref, err)
			}
			if !e.verifyReclaimReadback(sc.Ref, readbackData) {
				return fmt.Errorf("reclaim readback did not prove to-do/unassigned for %s", sc.Ref)
			}

			s.Actions = append(s.Actions, ActionLogEntry{
				Action:   "reclaim-stale",
				Ref:      sc.Ref,
				Owner:    sc.Owner,
				Verified: true,
			})
			s.StaleActionsExecuted++
		}
	}

	// 2. Routing repair for blocked-only queue.
	if s.Todo > 0 && s.Dispatchable == 0 && s.Blocked == s.Todo {
		blocked := make([]string, 0, len(s.BlockedTargets))
		for _, target := range s.BlockedTargets {
			if strings.TrimSpace(target.Owner) == "" || strings.TrimSpace(target.Lane) == "" || strings.TrimSpace(target.Task) == "" {
				return fmt.Errorf("lifecycle: blocked target has incomplete canonical role/lane/task identity: %+v", target)
			}
			lane, err := e.resolveRoleLane(target.Owner)
			if err != nil || lane != target.Lane {
				if err == nil {
					err = fmt.Errorf("resolved lane %q does not match target lane %q", lane, target.Lane)
				}
				return fmt.Errorf("lifecycle: blocked target identity: %w", err)
			}
			held, err := e.targetHeld(context.Background(), target.Owner, target.Task)
			if err != nil {
				return err
			}
			if !held {
				blocked = append(blocked, target.Task)
			}
		}
		if e.HoldReader == nil {
			blocked = append(blocked, s.BlockedRefs...)
		}
		if e.HoldReader != nil && len(blocked) == 0 {
			return nil
		}
		if e.RoutingHook == "" {
			return fmt.Errorf("blocked-only queue requires routing hook")
		}
		blockedRefs := strings.Join(blocked, ",")
		cmd := exec.Command(e.RoutingHook,
			"--refs", blockedRefs,
			"--reason", "blocked-only queue",
			"--skip-blocked",
		)
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("routing repair failed: %w", err)
		}

		var routingResult struct {
			ReplacementRef  string `json:"replacement_ref"`
			ReplacementRef2 string `json:"replacementRef"`
			Lane            string `json:"lane"`
			AssignedLane    string `json:"assigned_lane"`
			Owner           string `json:"owner"`
			Assignee        string `json:"assignee"`
		}
		json.Unmarshal(out, &routingResult)

		replacementRef := routingResult.ReplacementRef
		if replacementRef == "" {
			replacementRef = routingResult.ReplacementRef2
		}
		routeLane := routingResult.Lane
		if routeLane == "" {
			routeLane = routingResult.AssignedLane
		}
		routeOwner := routingResult.Owner
		if routeOwner == "" {
			routeOwner = routingResult.Assignee
		}

		if replacementRef == "" || routeLane == "" || routeOwner == "" {
			return fmt.Errorf("routing action must declare replacement_ref, lane, and owner")
		}

		readbackData, err := e.authoritativeReadback("route", blockedRefs, replacementRef, routeLane, routeOwner)
		if err != nil {
			return fmt.Errorf("routing readback failed: %w", err)
		}
		if !e.verifyRoutingReadback(readbackData, replacementRef, routeLane, routeOwner) {
			return fmt.Errorf("routing readback did not prove the exact replacement/lane/owner")
		}

		s.Actions = append(s.Actions, ActionLogEntry{
			Action:   "routing-repair",
			Ref:      blockedRefs,
			Owner:    routeOwner,
			Lane:     routeLane,
			Verified: true,
		})
		s.RoutingActionExecuted = true
	}

	// 3. Kick settled lanes with dispatchable queue.
	if s.Dispatchable > 0 && len(s.Settled) > 0 {
		for _, settled := range s.Settled {
			if e.HoldReader != nil && e.HoldLaneResolver == nil {
				return fmt.Errorf("lifecycle: canonical lane resolver is required")
			}
			role, lane := settled.Role, settled.Lane
			if role == "" || lane == "" {
				if e.HoldReader == nil {
					role, lane = settled.Name, settled.Name
				} else if e.HoldLiveAgentResolver != nil {
					var err error
					role, lane, err = e.HoldLiveAgentResolver(settled.Name)
					if err != nil {
						return fmt.Errorf("lifecycle: settled lane %s: %w", settled.Name, err)
					}
				}
			}
			if e.HoldReader != nil && (strings.TrimSpace(role) == "" || strings.TrimSpace(lane) == "") {
				return fmt.Errorf("lifecycle: settled lane %s has no canonical identity", settled.Name)
			}
			if err := e.checkLaneHold(lane, role); err != nil {
				return err
			}
			cmd := exec.Command(e.kickBin(), "--no-raise", "--quiet", "--reason", "lifecycle: dispatchable queue", settled.Name)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("kick failed for %s: %w\n%s", settled.Name, err, string(out))
			}

			// Verify interactive working transition.
			refreshOut, err := exec.Command(e.herdrBin(), "agent", "list").Output()
			if err != nil {
				return fmt.Errorf("cannot verify kick for %s: %w", settled.Name, err)
			}
			if !e.verifyAgentWorking(refreshOut, settled.Name) {
				return fmt.Errorf("kick did not prove interactive working transition for %s", settled.Name)
			}

			s.Actions = append(s.Actions, ActionLogEntry{
				Action:   "kick",
				Lane:     settled.Name,
				Observed: "interactive-working",
				Verified: true,
			})
		}
	}

	// 4. Event handoff processing.
	if eventJSON != nil && len(eventJSON) > 0 {
		var event struct {
			EventID  string `json:"event_id"`
			EventId  string `json:"eventId"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Producer string `json:"producer"`
			Lane     string `json:"lane"`
			Reviewer string `json:"reviewer"`
			Task     string `json:"task"`
			Ref      string `json:"ref"`
		}
		if err := json.Unmarshal(eventJSON, &event); err != nil {
			return err
		}
		eventID := event.EventID
		if eventID == "" {
			eventID = event.EventId
		}
		if eventID == "" {
			eventID = event.ID
		}
		producer := event.Producer
		if producer == "" {
			producer = event.Lane
		}
		task := event.Task
		if task == "" {
			task = event.Ref
		}

		if eventID == "" || producer == "" || event.Type == "" || task == "" {
			return fmt.Errorf("event requires event_id, type, producer, and task")
		}

		if eventTypeNeedsReviewer(event.Type) && (event.Reviewer == "" || event.Reviewer == producer) {
			return fmt.Errorf("review event requires an independent nonempty reviewer")
		}

		// Check for existing completed lease.
		if storedEvent, err := eventLeasePayload(leaseRoot, eventID); err == nil {
			if string(storedEvent) != string(eventJSON) {
				return fmt.Errorf("event_id payload conflict: %s", eventID)
			}
			s.EventCompleted = true
			s.Actions = append(s.Actions, ActionLogEntry{
				Action:     "event-replay-suppressed",
				EventID:    eventID,
				Idempotent: true,
			})
			return nil
		}

		if event.Type == "handoff" {
			claimState, claimJSON, err := e.claimEventHandoff(leaseRoot, eventID, eventJSON)
			if err != nil {
				return err
			}
			if claimState == "replay" {
				s.EventCompleted = true
				s.Actions = append(s.Actions, ActionLogEntry{
					Action:     "event-replay-suppressed",
					EventID:    eventID,
					Idempotent: true,
				})
				return nil
			}
			if claimState != "owned" {
				return fmt.Errorf("invalid handoff claim state: %s", claimState)
			}

			// Crash-after-claim test seam.
			if e.TestCrashAfterClaim {
				os.Exit(9)
			}

			// Run review hook.
			if e.ReviewHook != "" {
				existingReview, _ := eventHookPayload(leaseRoot, eventID, "review")
				if existingReview != nil {
					if string(existingReview) != string(eventJSON) {
						releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
						return fmt.Errorf("review completion payload conflict: %s", eventID)
					}
				} else {
					if err := e.runHookWithAck(leaseRoot, eventID, "review", eventJSON, e.ReviewHook,
						"--event-id", eventID,
						"--producer", producer,
						"--reviewer", event.Reviewer,
						"--task", task,
						"--independent",
					); err != nil {
						releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
						return fmt.Errorf("review hook failed: %w", err)
					}

					if e.TestCrashAfterHookAck == "review" {
						os.Exit(9)
					}

					if err := publishEventHook(leaseRoot, eventID, "review", eventJSON); err != nil {
						releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
						return fmt.Errorf("review publication failed: %w", err)
					}
				}
			}

			// Run next hook.
			if e.NextHook != "" {
				existingNext, _ := eventHookPayload(leaseRoot, eventID, "next")
				if existingNext != nil {
					if string(existingNext) != string(eventJSON) {
						releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
						return fmt.Errorf("next completion payload conflict: %s", eventID)
					}
				} else {
					if err := e.runHookWithAck(leaseRoot, eventID, "next", eventJSON, e.NextHook,
						"--event-id", eventID,
						"--lane", producer,
						"--after", task,
						"--owned",
					); err != nil {
						releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
						return fmt.Errorf("next hook failed: %w", err)
					}

					if e.TestCrashAfterHookAck == "next" {
						os.Exit(9)
					}

					if err := publishEventHook(leaseRoot, eventID, "next", eventJSON); err != nil {
						releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
						return fmt.Errorf("next publication failed: %w", err)
					}
				}
			}

			// Publish final lease.
			if _, err := publishEventFile(leaseRoot, eventID+".json", eventJSON); err != nil {
				releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
				return fmt.Errorf("event lease publication failed: %w", err)
			}

			releaseEventFile(leaseRoot, eventID+".pending.json", claimJSON)
			s.EventCompleted = true
			s.Actions = append(s.Actions, ActionLogEntry{
				Action:   "review-dispatch",
				EventID:  eventID,
				Producer: producer,
				Reviewer: event.Reviewer,
				Task:     task,
				Observed: "interactive-working",
			}, ActionLogEntry{
				Action:   "next-owned-task",
				EventID:  eventID,
				Lane:     producer,
				Ref:      task,
				Verified: true,
			})
		}
	}

	return nil
}

func (e *Engine) checkHold(task, lane, owner string) error {
	return e.checkHoldIdentity(HoldIdentity{Repository: repoRoot(), Owner: owner, Lane: lane, Task: task, Scope: "task"})
}

func (e *Engine) checkLaneHold(lane, owner string) error {
	return e.checkHoldIdentity(HoldIdentity{Repository: repoRoot(), Owner: owner, Lane: lane, Scope: "lane"})
}

func (e *Engine) checkHoldIdentity(identity HoldIdentity) error {
	if e.HoldReader == nil {
		return nil
	}
	if e.HoldIdentity != nil {
		identity = e.HoldIdentity(identity.Task, identity.Lane, identity.Owner)
	}
	held, err := e.isHeld(identity)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("lifecycle: held identity denied")
	}
	return nil
}

func (e *Engine) isHeld(identity HoldIdentity) (bool, error) {
	if e.HoldReader == nil {
		return false, nil
	}
	if !identity.valid() {
		return false, fmt.Errorf("lifecycle: hold authority denied ambiguous exact identity")
	}
	source, ok := e.HoldReader.(interface {
		CurrentGeneration(context.Context, HoldIdentity) (int64, error)
	})
	if !ok {
		return false, fmt.Errorf("lifecycle: hold generation source is required")
	}
	generation, err := source.CurrentGeneration(context.Background(), identity)
	if err != nil || generation <= 0 {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("lifecycle: invalid hold generation %d", generation)
	}
	decision, err := e.HoldReader.Check(context.Background(), identity, generation)
	if err != nil {
		return false, fmt.Errorf("lifecycle: hold authority: %w", err)
	}
	return decision.Held, nil
}

func (e *Engine) holdBlocks(ctx context.Context, lane, owner, task string) bool {
	if e.HoldReader == nil {
		return false
	}
	held, err := e.targetHeld(ctx, owner, task)
	return err != nil || held
}

func (e *Engine) targetHeld(ctx context.Context, role, task string) (bool, error) {
	if strings.TrimSpace(role) == "" || strings.TrimSpace(task) == "" {
		return true, nil
	}
	if e.HoldReader != nil && e.HoldLaneResolver == nil {
		return true, fmt.Errorf("lifecycle: canonical lane resolver is required")
	}
	lane := role
	if e.HoldLaneResolver != nil {
		var err error
		lane, err = e.HoldLaneResolver(role)
		if err != nil || strings.TrimSpace(lane) == "" {
			if err == nil {
				err = fmt.Errorf("unknown configured role %q", role)
			}
			return true, err
		}
	}
	identities := []HoldIdentity{
		{Repository: repoRoot(), Owner: role, Lane: lane, Scope: "lane"},
		{Repository: repoRoot(), Owner: role, Lane: lane, Task: task, Scope: "task"},
	}
	if e.HoldIdentity != nil {
		identities = []HoldIdentity{e.HoldIdentity("", lane, role), e.HoldIdentity(task, lane, role)}
	}
	for _, identity := range identities {
		held, err := e.isHeld(identity)
		if err != nil {
			return true, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) resolveRoleLane(role string) (string, error) {
	if e.HoldLaneResolver == nil {
		return "", fmt.Errorf("lifecycle: canonical lane resolver is required")
	}
	lane, err := e.HoldLaneResolver(role)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(lane) == "" {
		return "", fmt.Errorf("unknown configured role %q", role)
	}
	return lane, nil
}

func (e *Engine) authoritativeReadback(action, refOrRefs, replacementRef, lane, owner string) ([]byte, error) {
	var (
		out []byte
		err error
	)
	if e.ReadbackHook != "" {
		args := []string{"--action", action, "--ref", refOrRefs}
		if replacementRef != "" {
			args = append(args, "--replacement-ref", replacementRef)
		}
		if lane != "" {
			args = append(args, "--lane", lane)
		}
		if owner != "" {
			args = append(args, "--owner", owner)
		}
		args = append(args, "--after-action")
		out, err = exec.Command(e.ReadbackHook, args...).Output()
	} else if action == "reclaim" {
		out, err = exec.Command(e.kaneoBin(), "task", "get", refOrRefs, "--json").Output()
	} else if replacementRef != "" {
		out, err = exec.Command(e.kaneoBin(), "task", "get", replacementRef, "--json").Output()
	} else {
		return nil, fmt.Errorf("cannot determine readback target")
	}
	if err != nil {
		return nil, err
	}
	if err := rejectKaneoErrorEnvelope("authoritative readback", out); err != nil {
		return nil, err
	}
	return out, nil
}

func rejectKaneoErrorEnvelope(operation string, payload []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		return nil
	}
	if _, ok := envelope["error"]; ok {
		return fmt.Errorf("%s: kaneo exited 0 but returned an error envelope", operation)
	}
	return nil
}

func (e *Engine) verifyReclaimReadback(ref string, payload []byte) bool {
	var card struct {
		Ref           string `json:"ref"`
		ID            string `json:"id"`
		Key           string `json:"key"`
		Status        string `json:"status"`
		Column        string `json:"column"`
		State         string `json:"state"`
		Owner         string `json:"owner"`
		Assignee      string `json:"assignee"`
		AssignedTo    string `json:"assigned_to"`
		AssignedAgent string `json:"assignedAgent"`
		AssigneeName  string `json:"assigneeName"`
		AssigneeID    string `json:"assigneeId"`
	}
	if err := json.Unmarshal(payload, &card); err != nil {
		return false
	}
	cardRef := card.Ref
	if cardRef == "" {
		cardRef = card.ID
	}
	if cardRef == "" {
		cardRef = card.Key
	}
	if cardRef != ref {
		return false
	}
	st := strings.ToLower(card.Status)
	if st == "" {
		st = strings.ToLower(card.Column)
	}
	if st == "" {
		st = strings.ToLower(card.State)
	}
	if !strings.Contains(st, "to-do") && !strings.Contains(st, "todo") && !strings.Contains(st, "backlog") {
		return false
	}
	owner := card.Owner
	if owner == "" {
		owner = card.Assignee
	}
	if owner == "" {
		owner = card.AssignedTo
	}
	if owner == "" {
		owner = card.AssignedAgent
	}
	if owner == "" {
		owner = card.AssigneeName
	}
	if owner == "" {
		owner = card.AssigneeID
	}
	return owner == ""
}

func (e *Engine) verifyRoutingReadback(payload []byte, replacementRef, lane, owner string) bool {
	// Try direct card first.
	var card struct {
		Ref           string `json:"ref"`
		ID            string `json:"id"`
		Key           string `json:"key"`
		Status        string `json:"status"`
		Column        string `json:"column"`
		State         string `json:"state"`
		Owner         string `json:"owner"`
		Assignee      string `json:"assignee"`
		AssignedTo    string `json:"assigned_to"`
		AssignedAgent string `json:"assignedAgent"`
		AssigneeName  string `json:"assigneeName"`
		AssigneeID    string `json:"assigneeId"`
		Lane          string `json:"lane"`
		AssignedLane  string `json:"assigned_lane"`
		Labels        []any  `json:"labels"`
		Blocked       *bool  `json:"blocked"`
	}
	if err := json.Unmarshal(payload, &card); err == nil {
		return e.checkRoutingCard(card, replacementRef, lane, owner)
	}

	// Try array or wrapper.
	var wrapper struct {
		Tasks []struct {
			Ref           string `json:"ref"`
			ID            string `json:"id"`
			Key           string `json:"key"`
			Status        string `json:"status"`
			Column        string `json:"column"`
			State         string `json:"state"`
			Owner         string `json:"owner"`
			Assignee      string `json:"assignee"`
			AssignedTo    string `json:"assigned_to"`
			AssignedAgent string `json:"assignedAgent"`
			AssigneeName  string `json:"assigneeName"`
			AssigneeID    string `json:"assigneeId"`
			Lane          string `json:"lane"`
			AssignedLane  string `json:"assigned_lane"`
			Labels        []any  `json:"labels"`
			Blocked       *bool  `json:"blocked"`
		} `json:"tasks"`
		Items []struct {
			Ref           string `json:"ref"`
			ID            string `json:"id"`
			Key           string `json:"key"`
			Status        string `json:"status"`
			Column        string `json:"column"`
			State         string `json:"state"`
			Owner         string `json:"owner"`
			Assignee      string `json:"assignee"`
			AssignedTo    string `json:"assigned_to"`
			AssignedAgent string `json:"assignedAgent"`
			AssigneeName  string `json:"assigneeName"`
			AssigneeID    string `json:"assigneeId"`
			Lane          string `json:"lane"`
			AssignedLane  string `json:"assigned_lane"`
			Labels        []any  `json:"labels"`
			Blocked       *bool  `json:"blocked"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return false
	}
	items := wrapper.Tasks
	if len(items) == 0 {
		items = wrapper.Items
	}
	for _, item := range items {
		if e.checkRoutingCard(struct {
			Ref           string `json:"ref"`
			ID            string `json:"id"`
			Key           string `json:"key"`
			Status        string `json:"status"`
			Column        string `json:"column"`
			State         string `json:"state"`
			Owner         string `json:"owner"`
			Assignee      string `json:"assignee"`
			AssignedTo    string `json:"assigned_to"`
			AssignedAgent string `json:"assignedAgent"`
			AssigneeName  string `json:"assigneeName"`
			AssigneeID    string `json:"assigneeId"`
			Lane          string `json:"lane"`
			AssignedLane  string `json:"assigned_lane"`
			Labels        []any  `json:"labels"`
			Blocked       *bool  `json:"blocked"`
		}(item), replacementRef, lane, owner) {
			return true
		}
	}
	return false
}

func (e *Engine) checkRoutingCard(card struct {
	Ref           string `json:"ref"`
	ID            string `json:"id"`
	Key           string `json:"key"`
	Status        string `json:"status"`
	Column        string `json:"column"`
	State         string `json:"state"`
	Owner         string `json:"owner"`
	Assignee      string `json:"assignee"`
	AssignedTo    string `json:"assigned_to"`
	AssignedAgent string `json:"assignedAgent"`
	AssigneeName  string `json:"assigneeName"`
	AssigneeID    string `json:"assigneeId"`
	Lane          string `json:"lane"`
	AssignedLane  string `json:"assigned_lane"`
	Labels        []any  `json:"labels"`
	Blocked       *bool  `json:"blocked"`
}, replacementRef, lane, owner string) bool {
	ref := card.Ref
	if ref == "" {
		ref = card.ID
	}
	if ref == "" {
		ref = card.Key
	}
	if ref != replacementRef {
		return false
	}

	st := strings.ToLower(card.Status)
	if st == "" {
		st = strings.ToLower(card.Column)
	}
	if st == "" {
		st = strings.ToLower(card.State)
	}
	if !strings.Contains(st, "to-do") && !strings.Contains(st, "todo") &&
		!strings.Contains(st, "in-progress") && !strings.Contains(st, "backlog") {
		return false
	}

	blocked := false
	if card.Blocked != nil {
		blocked = *card.Blocked
	}
	if blocked {
		return false
	}

	cardOwner := card.Owner
	if cardOwner == "" {
		cardOwner = card.Assignee
	}
	if cardOwner == "" {
		cardOwner = card.AssignedTo
	}
	if cardOwner == "" {
		cardOwner = card.AssignedAgent
	}
	if cardOwner == "" {
		cardOwner = card.AssigneeName
	}
	if cardOwner == "" {
		cardOwner = card.AssigneeID
	}
	if cardOwner != owner {
		return false
	}

	cardLane := card.Lane
	if cardLane == "" {
		cardLane = card.AssignedLane
	}
	if strings.EqualFold(cardLane, lane) {
		return true
	}

	// Check labels for lane match.
	for _, l := range card.Labels {
		var labelStr string
		switch v := l.(type) {
		case string:
			labelStr = v
		case map[string]any:
			if n, ok := v["name"].(string); ok {
				labelStr = n
			} else if lb, ok := v["label"].(string); ok {
				labelStr = lb
			}
		}
		if strings.EqualFold(labelStr, lane) {
			return true
		}
	}
	return false
}

func (e *Engine) verifyAgentWorking(agentList []byte, name string) bool {
	var wrapper struct {
		Result struct {
			Agents []struct {
				Name              string `json:"name"`
				Label             string `json:"label"`
				Status            string `json:"status"`
				AgentStatus       string `json:"agent_status"`
				Interactive       *bool  `json:"interactive"`
				InteractiveReady  *bool  `json:"interactive_ready"`
				InteractiveReady2 *bool  `json:"interactiveReady"`
			} `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(agentList, &wrapper); err != nil {
		return false
	}
	for _, ag := range wrapper.Result.Agents {
		agName := ag.Name
		if agName == "" {
			agName = ag.Label
		}
		if agName != name {
			continue
		}
		st := ag.Status
		if st == "" {
			st = ag.AgentStatus
		}
		if strings.ToLower(st) != "working" {
			return false
		}
		interactive := false
		if ag.Interactive != nil {
			interactive = *ag.Interactive
		} else if ag.InteractiveReady != nil {
			interactive = *ag.InteractiveReady
		} else if ag.InteractiveReady2 != nil {
			interactive = *ag.InteractiveReady2
		}
		return interactive
	}
	return false
}

// ============================================================================
// Red Code Detection
// ============================================================================

func (e *Engine) computeRedCodes(s *Summary) *Summary {
	var red []string

	if len(s.GoalViolations) > 0 {
		red = append(red, "missing_bounded_goal")
	}
	if s.StaleInProgress > s.StaleActionsExecuted {
		red = append(red, "stale_in_progress")
	}
	if s.Dispatchable > 0 && len(s.Settled) > 0 {
		red = append(red, "done_or_idle_with_dispatchable_queue")
	}
	if s.Todo > 0 && s.Dispatchable == 0 && s.Blocked == s.Todo && !s.RoutingActionExecuted {
		red = append(red, "routing_repair_required")
	}
	if s.InProgress > 0 && len(s.Utilized) == 0 && s.StaleInProgress > s.StaleActionsExecuted {
		red = append(red, "no_live_owner")
	}
	if false { // placeholder: event check requires external state
		if !s.EventCompleted {
			red = append(red, "event_requires_act")
		}
	}

	if len(s.Critical) > 0 {
		red = append(red, "unknown_or_unconfigured_live_identity")
	}
	s.Red = red
	s.Healthy = len(red) == 0
	return s
}

// ============================================================================
// High-level API
// ============================================================================

// Point runs the lifecycle observation without actions (default mode).
// Returns the summary JSON and a healthy boolean. Exits with code 7 if unhealthy.
func (e *Engine) Point() (*Summary, error) {
	return e.Observe(false)
}

// Act runs the lifecycle observation WITH actions (--act mode).
// Returns the summary JSON and a healthy boolean. Exits with code 7 if unhealthy.
func (e *Engine) Act() (*Summary, error) {
	return e.Observe(true)
}

// RecordEvent records an event into the event lease store.
func (e *Engine) RecordEvent(event EventRecord) error {
	stateRoot := e.stateRoot()
	leaseRoot := eventLeaseRoot(stateRoot)

	if err := validateEventID(event.EventID); err != nil {
		return err
	}
	if eventTypeNeedsReviewer(event.Type) {
		if event.Reviewer == "" || event.Reviewer == event.Producer {
			return fmt.Errorf("review event requires an independent nonempty reviewer")
		}
	} else if event.Reviewer != "" {
		return fmt.Errorf("reviewer is valid only for review/verdict/handoff events")
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}

	// Idempotent publish with retry.
	for attempt := 0; attempt < 20; attempt++ {
		stored, err := eventLeasePayload(leaseRoot, event.EventID)
		if err == nil {
			var a, b any
			json.Unmarshal(stored, &a)
			json.Unmarshal(payload, &b)
			ea, _ := json.Marshal(a)
			eb, _ := json.Marshal(b)
			if string(ea) != string(eb) {
				return fmt.Errorf("event_id payload conflict: %s", event.EventID)
			}
			return nil // idempotent replay
		}

		if _, err := publishEventFile(leaseRoot, event.EventID+".json", payload); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("event lease race/incomplete: %s", event.EventID)
}
