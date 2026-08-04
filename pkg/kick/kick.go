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
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
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

// LookupAgent finds an agent by name in the list and returns its status and pane ID.
func LookupAgent(agents []AgentEntry, name string) (status, paneID string, found bool) {
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
	// Determine target names.
	names := opts.Names
	if len(names) == 0 {
		names = append([]string(nil), StandingIDs()...)
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
