// Package kick re-engages standing (or named) agents that are idle/done/blocked.
//
// Port of bin/herd-kick (zsh, 223 lines) to Go.
//
// Standing agents finish a pass and sit idle waiting for the coordinator.
// That is the wrong default: they should be kicked the moment there is work
// in their lane (merge landed, queue changed, guardian interval, etc.).
//
// Only sends when status is idle/done/blocked (or --all). Uses herd-send.
// Does not spawn new tabs; raises missing standing agents via herd-standing
// first when kicking the full fleet with no names (unless --no-raise).
package kick

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/boardfreeze"
	"github.com/Kampe/Herdforge/pkg/broadcast"
	"github.com/Kampe/Herdforge/pkg/goalguard"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/posture"
)

// The canonical standing fleet roster is derived at runtime by StandingIDs()
// (see roster.go): every lane declared in docs/agent/lane-registry.json (or
// .herd/lane-registry.json, then .herd/herd.yaml) is prefixed with ForgePrefix
// to produce the live herdr agent name. There is intentionally no hardcoded
// roster here — this repo's lanes are the source of truth.

// LiveStatuses identifies statuses that mean "agent session still holds the name".
const LiveStatuses = "working|idle|starting|done|blocked"

// AgentEntry represents a single agent from herdr agent list.
type AgentEntry struct {
	Name        string `json:"name,omitempty"`
	Label       string `json:"label,omitempty"`
	Status      string `json:"agent_status,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	Workspace   string `json:"workspace_id,omitempty"`
	Interactive *bool  `json:"interactive,omitempty"`
}

// AgentListResult wraps the herdr agent list response.
type AgentListResult struct {
	Result struct {
		Agents []AgentEntry `json:"agents"`
	} `json:"result"`
}

// Options controls the kick behavior.
type Options struct {
	// Names to kick. Empty means all standing agents.
	Names []string
	// Force kicks even agents that are working (--all).
	Force bool
	// DryRun prints without sending (--dry-run).
	DryRun bool
	// Quiet suppresses non-error output (--quiet).
	Quiet bool
	// Reason overrides the default kick context message (--reason).
	Reason string
	// RaiseMissing calls herd-standing for missing agents (default true).
	RaiseMissing bool
	// HoldReader is the shared durable hold authority. Production callers must
	// inject it; nil is rejected by Run before any fleet side effect.
	HoldReader lifecycle.HoldReader
	// Identity resolves a complete authority identity for each lane.
	Identity    func(string) (lifecycle.HoldIdentity, error)
	Generation  func(context.Context, lifecycle.HoldIdentity) (int64, error)
	ActiveTasks lifecycle.ActiveTaskResolver
	// FetchAgents lists live agents. When nil, Run uses the production herdr
	// lookup; tests and embedders can inject a deterministic source.
	FetchAgents func() ([]AgentEntry, error)
	// Markers maps agent name → broadcast exclusion markers (FAC-187).
	// Production runKick loads quarantine/protected sets here so a
	// lifted-hold kick never prompts excluded task lanes. Ephemeral
	// review-* tabs are auto-marked; standing forge-assayer / reviewer
	// lanes are NOT — kick is their recovery path.
	Markers map[string][]broadcast.ExclusionKind
	// LiveIdentity, when set, re-reads identity immediately before every
	// prompt; BoundIdentities supplies the bound side of the fence.
	LiveIdentity    func(context.Context, broadcast.Target) (broadcast.PromptIdentity, error)
	BoundIdentities map[string]broadcast.PromptIdentity
	// Cadence is the minimum interval between kicks. LaneCadence overrides it
	// for a named lane. A zero duration disables cadence throttling.
	Cadence     time.Duration
	LaneCadence map[string]time.Duration
	LastKick    map[string]time.Time
	Now         func() time.Time
	// Repair bypasses the fleet freeze gate. Repair prompts are safe to send
	// during a freeze because they restore an already-authorized change.
	Repair bool
	// AuthorityEnvelope re-delivers the standing grant on every resume kick.
	AuthorityEnvelope func(name string) (goalguard.AuthorityEnvelope, error)
	Freeze            func() (bool, string, error)
}

// Result holds counts for one kick run.
type Result struct {
	Kicked  int
	Skipped int
	Failed  int
	Entries []EntryResult
}

// EntryResult records what happened to one agent.
type EntryResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	PaneID string `json:"pane_id,omitempty"`
	Result string `json:"result"` // kicked/skipped/failed
	Reason string `json:"reason,omitempty"`
}

// KickMessage builds the re-engagement prompt for a standing lane.
// Generic role-based template: the lane id (the live forge agent name, e.g.
// "forge-worker") is embedded so the coordinator can see which lane is being
// asked to continue; the "Rapid turn" suffix is fixed so nudge cadence is
// stable across all lanes.
func KickMessage(id, reason string) string {
	base := fmt.Sprintf("STANDING KICK (%s). Continue your standing packet / lane role now. Refresh origin/main, finish or audit the current assignment, and report status when this pass finishes.", id)

	var extra string
	if reason != "" {
		extra = " Context: " + reason
	}
	return base + extra + " Rapid turn: do not wait for another nudge."
}

// HerdSend sends a message to a pane via herd-send. Returns the output.
// Falls back to `herdr agent prompt` if herd-send is not found.
func HerdSend(paneID, message string) (string, error) {
	// Prefer herd-send if available.
	if p, err := exec.LookPath("herd-send"); err == nil {
		cmd := exec.Command(p, paneID, message)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	// Fall back to herdr agent prompt.
	cmd := exec.Command("herdr", "agent", "prompt", "--pane", paneID, message)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// HerdStanding calls herd-standing to ensure missing agents exist.
func HerdStanding(args ...string) (string, error) {
	p, err := exec.LookPath("herd-standing")
	if err != nil {
		// For testing, return empty — the real tool may not be in PATH.
		return "", nil
	}
	cmdArgs := append([]string{"--quiet"}, args...)
	cmd := exec.Command(p, cmdArgs...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// FetchAgentList calls herdr agent list and returns the parsed result.
func FetchAgentList() ([]AgentEntry, error) {
	cmd := exec.Command("herdr", "agent", "list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	var result AgentListResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse agent list: %w", err)
	}
	return result.Result.Agents, nil
}

// LookupAgent finds an agent by name and returns its status and pane ID.
//
// FAC-695: exact equality alone made this the THIRD place carrying the FAC-660
// defect, after the census and herd attention -- and this one was the worst,
// because kick is the tool that wakes an idle lane.
//
// A live standing lane is spelled forge-<lane>-<digest>; the roster spells it
// forge-<lane>. So `herd kick` reported every standing lane
// "missing (not live)" and finished with kicked=0 while four of them were live
// and idle. The one command for restarting a stalled fleet could not act on a
// single lane, and reported a fleet gap instead of the idleness it was run to
// fix. Measured live: forge-scout-planner-2918de97b5 idle, reported missing.
//
// LaneForAgent already lived in this package. The matcher was here the whole
// time; this function simply did not call it.
//
// Exact equality is still tried first: it is the cheap path and it is what a
// non-standing target uses. Lane matching is the fallback only.
func LookupAgent(agents []AgentEntry, name string) (status, paneID string, found bool) {
	if st, pane, ok := lookupExact(agents, name); ok {
		return st, pane, true
	}
	lanes := []string{name}
	for _, a := range agents {
		if LaneForAgent(a.Name, lanes) == "" && LaneForAgent(a.Label, lanes) == "" {
			continue
		}
		st := a.Status
		if st == "" {
			st = "unknown"
		}
		return st, a.PaneID, true
	}
	return "", "", false
}

func lookupExact(agents []AgentEntry, name string) (status, paneID string, found bool) {
	for _, a := range agents {
		an := a.Name
		if an == "" {
			an = a.Label
		}
		if an != name {
			continue
		}
		st := a.Status
		if st == "" {
			st = "unknown"
		}
		return st, a.PaneID, true
	}
	return "", "", false
}

// ProviderDeathCheck runs herd-process --check-provider-death for a lane.
// Returns true if the provider died mid-run (should not re-engage).
func ProviderDeathCheck(lane string) bool {
	p, err := exec.LookPath("herd-process")
	if err != nil {
		return false
	}
	cmd := exec.Command(p, "--check-provider-death", lane)
	err = cmd.Run()
	return err == nil
}

// Selftest verifies that the package can produce messages for all standing IDs.
func Selftest() error {
	for _, id := range StandingIDs() {
		msg := KickMessage(id, "")
		if msg == "" {
			return fmt.Errorf("empty kick message for %s", id)
		}
		if !strings.Contains(msg, id) {
			return fmt.Errorf("kick message for %s does not contain agent id", id)
		}
	}
	return nil
}

// Run executes a kick operation.
func Run(opts Options) (*Result, error) {
	if opts.HoldReader == nil {
		return nil, fmt.Errorf("kick: hold authority is required")
	}
	if opts.Identity == nil {
		return nil, fmt.Errorf("kick: complete hold identity resolver is required")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if opts.LastKick == nil {
		opts.LastKick = make(map[string]time.Time)
	}
	// Determine target names before policy gates so a fleet-wide freeze can
	// report every standing lane, including the implicit default roster.
	names := opts.Names
	if len(names) == 0 {
		names = append([]string(nil), StandingIDs()...)
	}
	if !opts.Repair {
		freeze := opts.Freeze
		if freeze == nil {
			freeze = func() (bool, string, error) {
				if trigger, active := posture.BoardFrozen("."); active {
					return true, trigger, nil
				}
				_, active, err := boardfreeze.Active(now())
				return active, "board freeze", err
			}
		}
		active, detail, freezeErr := freeze()
		if freezeErr != nil {
			return nil, fmt.Errorf("kick: freeze authority: %w", freezeErr)
		}
		if active {
			if strings.TrimSpace(detail) == "" {
				detail = "board freeze active"
			}
			result := &Result{}
			for _, id := range names {
				result.Skipped++
				result.Entries = append(result.Entries, EntryResult{Name: id, Result: "skipped", Reason: "freeze: " + detail})
			}
			return result, nil
		}
	}
	// Sort deterministically.
	if !sorted(names, StandingIDs()) {
		sort.Strings(names)
	}

	// Consult the shared authority for every target before raising or nudging.
	held := make(map[string]string)
	for _, id := range names {
		laneID, identityErr := opts.Identity(id)
		if identityErr != nil {
			if errors.Is(identityErr, lifecycle.ErrHoldDenied) {
				held[id] = identityErr.Error()
				continue
			}
			return nil, fmt.Errorf("kick: lane %s identity: %w", id, identityErr)
		}
		gen := opts.Generation
		if err := lifecycle.CheckLaneAndTaskHold(context.Background(), opts.HoldReader, opts.ActiveTasks, laneID.Repository, laneID.Owner, laneID.Lane, gen); err != nil {
			if errors.Is(err, lifecycle.ErrHoldDenied) {
				held[id] = err.Error()
				continue
			}
			return nil, fmt.Errorf("kick: lane %s authority: %w", id, err)
		}
	}

	// Raise missing standing agents when kicking the full default set.
	if opts.RaiseMissing && len(names) >= len(StandingIDs()) && len(held) == 0 {
		if _, err := HerdStanding("--all"); err != nil {
			return nil, fmt.Errorf("kick: raise standing: %w", err)
		}
	}
	if len(held) == len(names) {
		result := &Result{}
		for _, id := range names {
			reason := held[id]
			if !opts.Quiet {
				fmt.Printf("herd-kick: skip %s (%s)\n", id, reason)
			}
			result.Skipped++
			result.Entries = append(result.Entries, EntryResult{Name: id, Result: "skipped", Reason: reason})
		}
		if !opts.Quiet {
			fmt.Printf("herd-kick: done kicked=%d skipped=%d failed=%d\n", result.Kicked, result.Skipped, result.Failed)
		}
		return result, nil
	}

	// Fetch agent list once (was 2N herdr calls in the zsh script).
	fetchAgents := opts.FetchAgents
	if fetchAgents == nil {
		fetchAgents = FetchAgentList
	}
	agents, err := fetchAgents()
	if err != nil {
		return nil, fmt.Errorf("kick: fetch agents: %w", err)
	}

	// FAC-187: compile the broadcast target set and subtract protected /
	// quarantined / ephemeral review-* task tabs before any prompt. Standing
	// reviewer lanes stay eligible — kick is their recovery path.
	excludedByBroadcast := compileKickExclusions(names, held, opts.Markers)

	result := &Result{}

	for _, id := range names {
		if reason, skip := held[id]; skip {
			if !opts.Quiet {
				fmt.Printf("herd-kick: skip %s (%s)\n", id, reason)
			}
			result.Skipped++
			result.Entries = append(result.Entries, EntryResult{Name: id, Result: "skipped", Reason: reason})
			continue
		}
		if reason, skip := excludedByBroadcast[id]; skip {
			if !opts.Quiet {
				fmt.Printf("herd-kick: skip %s (broadcast exclusion: %s)\n", id, reason)
			}
			result.Skipped++
			result.Entries = append(result.Entries, EntryResult{Name: id, Result: "skipped", Reason: "broadcast:" + reason})
			continue
		}
		if interval := cadenceFor(opts, id); interval > 0 {
			if previous, ok := opts.LastKick[id]; ok && now().Sub(previous) < interval {
				result.Skipped++
				result.Entries = append(result.Entries, EntryResult{Name: id, Result: "skipped", Reason: fmt.Sprintf("cadence: next kick in %s", interval-now().Sub(previous))})
				continue
			}
		}
		st, paneID, found := LookupAgent(agents, id)
		if !found || paneID == "" {
			if !opts.Quiet {
				fmt.Printf("herd-kick: %s missing (not live)\n", id)
			}
			if opts.RaiseMissing {
				if _, raiseErr := HerdStanding("--only", id); raiseErr != nil {
					result.Failed++
					result.Entries = append(result.Entries, EntryResult{Name: id, Result: "failed", Reason: "raise failed: " + raiseErr.Error()})
					continue
				}
				// Re-fetch just for this lane.
				agents2, err2 := fetchAgents()
				if err2 != nil {
					result.Failed++
					result.Entries = append(result.Entries, EntryResult{Name: id, Result: "failed", Reason: "refetch failed: " + err2.Error()})
					continue
				}
				st2, paneID2, found2 := LookupAgent(agents2, id)
				if found2 {
					st, paneID, found = st2, paneID2, found2
				}
			}
			if !found || paneID == "" {
				if opts.DryRun {
					if !opts.Quiet {
						fmt.Printf("herd-kick: DRY %s (missing, would raise)\n", id)
					}
					result.Kicked++
					opts.LastKick[id] = now()
					result.Entries = append(result.Entries, EntryResult{
						Name:   id,
						Result: "dry-run",
						Reason: "agent not live",
					})
					continue
				}
				fmt.Fprintf(os.Stderr, "herd-kick: FAIL %s still missing\n", id)
				result.Failed++
				result.Entries = append(result.Entries, EntryResult{
					Name:   id,
					Result: "failed",
					Reason: "still missing after raise attempt",
				})
				continue
			}
		}

		// Check for provider death before re-engaging a settled lane.
		if !opts.Force {
			switch st {
			case "idle", "done", "blocked", "unknown":
				if ProviderDeathCheck(id) {
					if !opts.Quiet {
						fmt.Printf("herd-kick: skip %s (provider died mid-run; cooled reset-aware + flagged coordinator; NOT re-engaging)\n", id)
					}
					result.Skipped++
					result.Entries = append(result.Entries, EntryResult{
						Name:   id,
						Result: "skipped",
						Reason: "provider died mid-run",
					})
					continue
				}
			}
		}

		// Status gate.
		if !opts.Force {
			switch st {
			case "idle", "done", "blocked", "unknown":
				// OK to kick.
			case "working", "starting":
				if !opts.Quiet {
					fmt.Printf("herd-kick: skip %s (status=%s)\n", id, st)
				}
				result.Skipped++
				result.Entries = append(result.Entries, EntryResult{
					Name:   id,
					Status: st,
					Result: "skipped",
					Reason: fmt.Sprintf("status=%s", st),
				})
				continue
			}
		}

		msg := KickMessage(id, opts.Reason)
		if opts.AuthorityEnvelope != nil {
			envelope, envelopeErr := opts.AuthorityEnvelope(id)
			if envelopeErr != nil {
				result.Failed++
				result.Entries = append(result.Entries, EntryResult{Name: id, Status: st, PaneID: paneID, Result: "failed", Reason: "authority envelope: " + envelopeErr.Error()})
				continue
			}
			if err := envelope.Validate(); err != nil {
				result.Failed++
				result.Entries = append(result.Entries, EntryResult{Name: id, Status: st, PaneID: paneID, Result: "failed", Reason: "authority envelope: " + err.Error()})
				continue
			}
			msg += "\n\n" + renderKickEnvelope(envelope)
		}

		// FAC-187: exact identity fence immediately before every prompt when
		// the caller supplies bound + live identity.
		if opts.LiveIdentity != nil {
			bound, ok := opts.BoundIdentities[id]
			if !ok {
				bound = broadcast.PromptIdentity{Action: "kick"}
			}
			if bound.Action == "" {
				bound.Action = "kick"
			}
			live, liveErr := opts.LiveIdentity(context.Background(), broadcast.Target{Name: id, PaneID: paneID})
			if liveErr != nil {
				if !opts.Quiet {
					fmt.Printf("herd-kick: skip %s (identity: %v)\n", id, liveErr)
				}
				result.Skipped++
				result.Entries = append(result.Entries, EntryResult{Name: id, Status: st, PaneID: paneID, Result: "skipped", Reason: "identity: " + liveErr.Error()})
				continue
			}
			if err := broadcast.CheckIdentity(bound, live); err != nil {
				if !opts.Quiet {
					fmt.Printf("herd-kick: skip %s (identity: %v)\n", id, err)
				}
				result.Skipped++
				result.Entries = append(result.Entries, EntryResult{Name: id, Status: st, PaneID: paneID, Result: "skipped", Reason: "identity: " + err.Error()})
				continue
			}
		}

		if opts.DryRun {
			if !opts.Quiet {
				fmt.Printf("herd-kick: DRY %s pane=%s status=%s\n", id, paneID, st)
				truncated := msg
				if len(truncated) > 120 {
					truncated = truncated[:120] + "…"
				}
				fmt.Printf("  msg: %s\n", truncated)
			}
			result.Kicked++
			opts.LastKick[id] = now()
			result.Entries = append(result.Entries, EntryResult{
				Name:   id,
				Status: st,
				PaneID: paneID,
				Result: "dry-run",
			})
			continue
		}

		sendOut, err := HerdSend(paneID, msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-kick: FAIL unconsumed prompt %s pane=%s: %s\n", id, paneID, sendOut)
			result.Failed++
			result.Entries = append(result.Entries, EntryResult{
				Name:   id,
				Status: st,
				PaneID: paneID,
				Result: "failed",
				Reason: fmt.Sprintf("send failed: %v", err),
			})
			continue
		}

		if !opts.Quiet {
			if sendOut != "" {
				fmt.Println(sendOut)
			}
			fmt.Printf("herd-kick: kicked %s (%s → working) pane=%s\n", id, st, paneID)
		}
		result.Kicked++
		opts.LastKick[id] = now()
		result.Entries = append(result.Entries, EntryResult{
			Name:   id,
			Status: st,
			PaneID: paneID,
			Result: "kicked",
		})
	}

	if !opts.Quiet {
		fmt.Printf("herd-kick: done kicked=%d skipped=%d failed=%d\n", result.Kicked, result.Skipped, result.Failed)
	}

	return result, nil
}

func renderKickEnvelope(a goalguard.AuthorityEnvelope) string {
	return fmt.Sprintf("AUTHORITY ENVELOPE (RESUME)\n- grantor: %s\n- packet path: %s\n- bounded autonomy: %s\n- mutation limits: %s\n- forbidden actions: %s\n- stop conditions: %s\nAUTHORITY ENVELOPE END", a.Grantor, a.PacketPath, a.BoundedAutonomy, a.MutationLimits, strings.Join(a.ForbiddenActions, "; "), strings.Join(a.StopConditions, "; "))
}

func cadenceFor(opts Options, id string) time.Duration {
	if d, ok := opts.LaneCadence[id]; ok {
		return d
	}
	return opts.Cadence
}

// CadenceStatePath is the default durable location for LastKick timestamps.
// Cadence throttling only suppresses a real repeat kick if this state
// survives across separate `herd kick` process invocations, so callers
// should load it with LoadLastKick before Run and persist it with
// SaveLastKick after.
func CadenceStatePath() string {
	return filepath.Join(posture.StateDir(), "kick-cadence.json")
}

// LoadLastKick reads a durable LastKick map from path. A missing file
// returns an empty, ready-to-use map rather than an error.
func LoadLastKick(path string) (map[string]time.Time, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kick: read cadence state: %w", err)
	}
	last := map[string]time.Time{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &last); err != nil {
			return nil, fmt.Errorf("kick: parse cadence state: %w", err)
		}
	}
	return last, nil
}

// SaveLastKick durably persists a LastKick map to path, replacing it
// atomically so a crash mid-write cannot leave a truncated state file.
func SaveLastKick(path string, last map[string]time.Time) error {
	data, err := json.Marshal(last)
	if err != nil {
		return fmt.Errorf("kick: encode cadence state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("kick: cadence state dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("kick: write cadence state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("kick: commit cadence state: %w", err)
	}
	return nil
}

func sorted(slice, reference []string) bool {
	if len(slice) != len(reference) {
		return false
	}
	for i, v := range slice {
		if v != reference[i] {
			return false
		}
	}
	return true
}

// compileKickExclusions builds the broadcast target set for a kick wave and
// returns name → exclusion reason for every candidate that must not be
// prompted. Auto-marks ephemeral review-* task tabs only; standing reviewer
// lanes stay kickable so recovery works. Held lanes and explicit Markers
// (quarantine/protected) are layered on top.
func compileKickExclusions(names []string, held map[string]string, markers map[string][]broadcast.ExclusionKind) map[string]string {
	candidates := make([]broadcast.Target, 0, len(names))
	for _, id := range names {
		t := broadcast.Target{
			Name:           id,
			Role:           "standing",
			AllowedActions: []string{"kick", "prompt"},
			Generation:     1, // standing kick has no lease generation fence
		}
		if isEphemeralReviewTab(id) {
			t.Markers = append(t.Markers, broadcast.ExcludeReviewer)
		}
		if _, ok := held[id]; ok {
			t.Markers = append(t.Markers, broadcast.ExcludeProtected)
		}
		if markers != nil {
			t.Markers = append(t.Markers, markers[id]...)
		}
		candidates = append(candidates, t)
	}
	sel := broadcast.Select(candidates)
	out := make(map[string]string, len(sel.Excluded))
	for _, e := range sel.Excluded {
		out[e.Target.Name] = string(e.Reason)
	}
	return out
}

// isEphemeralReviewTab matches task-bound review tabs created by
// herd review --spawn (review-assayer-FAC-…), not standing forge-assayer /
// forge-reviewer lanes. Standing reviewers must remain kickable: kick is
// the recovery path for an idle/blocked lane (FAC-187).
func isEphemeralReviewTab(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, "review-")
}

// LoadBroadcastMarkers reads an optional JSON object of agent-name →
// exclusion-kind arrays from path. Missing file returns nil, nil so production
// kick can call it unconditionally.
func LoadBroadcastMarkers(path string) (map[string][]broadcast.ExclusionKind, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var raw map[string][]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("kick: parse broadcast markers: %w", err)
	}
	out := make(map[string][]broadcast.ExclusionKind, len(raw))
	for name, kinds := range raw {
		for _, k := range kinds {
			out[name] = append(out[name], broadcast.ExclusionKind(k))
		}
	}
	return out, nil
}
