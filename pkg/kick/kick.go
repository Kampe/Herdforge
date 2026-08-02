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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// StandingIDs is the canonical standing fleet roster (single source of truth
// for kick, attention, and standing).
var StandingIDs = []string{
	"api-crusader",
	"chain-indexer",
	"defi-crusader",
	"docs-custodian",
	"herd-smith",
	"nft-data-engineer",
	"perf-cost-guard",
	"platform-ops",
	"qa-sentinel",
	"review-harvest-supervisor",
	"scout-planner",
	"security-sentinel",
	"ux-comber",
}

// LiveStatuses identifies statuses that mean "agent session still holds the name".
const LiveStatuses = "working|idle|starting|done|blocked"

// AgentEntry represents a single agent from herdr agent list.
type AgentEntry struct {
	Name       string `json:"name,omitempty"`
	Label      string `json:"label,omitempty"`
	Status     string `json:"agent_status,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`
	TabID      string `json:"tab_id,omitempty"`
	Workspace  string `json:"workspace_id,omitempty"`
	Interactive *bool `json:"interactive,omitempty"`
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
// Matches the per-lane templates from bin/herd-kick.
func KickMessage(id, reason string) string {
	var base string
	switch id {
	case "scout-planner":
		base = "STANDING KICK (scout-planner). Immediately: refresh origin/main, re-rank Kaneo forward queue, consume any open GitHub issues into tickets, note collisions with live worktrees, and report the top 5 claimable tickets with owned-paths. Do not set in-progress/done."
	case "ux-comber":
		base = "STANDING KICK (ux-comber). Immediately: walk consumer journeys at 1440w and 390w (auth + unauth), fix apps/web defects you own, file repro-backed tickets for backend/data bugs, commit on standing/ux-comber, report NEEDS_REVIEW with evidence. Do not self-merge."
	case "docs-custodian":
		base = "STANDING KICK (docs-custodian). Immediately: git fetch origin/main, rebase/reset standing branch onto origin/main, scan TOOLING-INVENTORY/PACKAGES/ENVIRONMENT/API-SURFACE-AUDIT/prompts against merged code, fix bounded R0 drift, report NEEDS_REVIEW or clean. Do not self-merge product code."
	case "platform-ops":
		base = "STANDING KICK (platform-ops). Immediately: guardian pass on infrastructure. Repair runtime drift within HARD BANS. Report ALERT or healthy with one-line evidence."
	case "review-harvest-supervisor":
		base = "STANDING KICK (review-harvest-supervisor). Immediately: run bin/herd-review-supervisor --json; if an act pass is delegated, run bin/herd-review-supervisor --act --spawn. Drain artifacts, independent reviews, and exact-SHA PASS harvests; never use force gates, claim product work, or wait for a coordinator."
	case "security-sentinel":
		base = "STANDING KICK (security-sentinel). Immediately: diff scan the working tree for secrets, supply-chain drift, and known CVEs affecting the deploy surface. Report any new findings or clean."
	case "defi-crusader":
		base = "STANDING KICK (defi-crusader). Immediately: verify position producers, check on-chain fixture coverage against current protocol state, reconcile any stale addresses. Report coverage gaps or clean."
	case "herd-smith":
		base = "STANDING KICK (herd-smith). Immediately: check build health on origin/main, verify no dependency drift, run make lint all across the forge. Report regressions or clean."
	case "api-crusader":
		base = "STANDING KICK (api-crusader). Immediately: contract-test the public API surface, check OpenAPI spec drift against implementation, verify auth boundaries on new endpoints. Report violations or clean."
	case "chain-indexer":
		base = "STANDING KICK (chain-indexer). Immediately: verify indexer progress, check for stalled blocks, reconcile any gaps against the chain head. Report lag or clean."
	case "nft-data-engineer":
		base = "STANDING KICK (nft-data-engineer). Immediately: verify NFT metadata freshness, check collection floor tracking, reconcile any stale token data. Report drift or clean."
	case "qa-sentinel":
		base = "STANDING KICK (qa-sentinel). Immediately: run the full test suite across all packages, check for flaky tests, verify coverage thresholds. Report failures or clean."
	case "perf-cost-guard":
		base = "STANDING KICK (perf-cost-guard). Immediately: profile recent deploys for latency and cost regressions, check resource utilization trends. Report anomalies or clean."
	default:
		base = fmt.Sprintf("STANDING KICK (%s). Continue your standing packet / task now. Report status when this pass finishes.", id)
	}

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

// LaneHeld checks whether a lane has a hold file in the hold directory.
// Returns the hold reason string and whether the lane is held.
func LaneHeld(lane string) (string, bool) {
	holdDir := os.Getenv("HERD_HOLD_DIR")
	if holdDir == "" {
		// Default hold dir.
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", false
			}
			stateHome = filepath.Join(home, ".local", "state")
		}
		holdDir = filepath.Join(stateHome, "chainseer", "herd", "hold")
	}
	path := filepath.Join(holdDir, lane)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	reason := strings.TrimSpace(string(data))
	if reason == "" {
		reason = "held by coordinator (no reason recorded)"
	}
	return reason, true
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
	for _, id := range StandingIDs {
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
	// Determine target names.
	names := opts.Names
	if len(names) == 0 {
		names = make([]string, len(StandingIDs))
		copy(names, StandingIDs)
	}

	// Sort deterministically.
	if !sorted(names, StandingIDs) {
		sort.Strings(names)
	}

	// Raise missing standing agents when kicking the full default set.
	if opts.RaiseMissing && len(names) >= len(StandingIDs) {
		HerdStanding("--all")
	}

	// Fetch agent list once (was 2N herdr calls in the zsh script).
	agents, err := FetchAgentList()
	if err != nil {
		agents = nil // proceed with empty list
	}

	result := &Result{}

	for _, id := range names {
		// Check hold first.
		if reason, held := LaneHeld(id); held {
			if !opts.Quiet {
				fmt.Printf("herd-kick: skip %s (HELD: %s) -- release with herd hold off %s\n", id, reason, id)
			}
			result.Skipped++
			result.Entries = append(result.Entries, EntryResult{
				Name:   id,
				Result: "skipped",
				Reason: fmt.Sprintf("HELD: %s", reason),
			})
			continue
		}

		st, paneID, found := LookupAgent(agents, id)
		if !found || paneID == "" {
			if !opts.Quiet {
				fmt.Printf("herd-kick: %s missing (not live)\n", id)
			}
			if opts.RaiseMissing {
				HerdStanding("--only", id)
				// Re-fetch just for this lane.
				agents2, err2 := FetchAgentList()
				if err2 == nil {
					st2, paneID2, found2 := LookupAgent(agents2, id)
					if found2 {
						st, paneID, found = st2, paneID2, found2
					}
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
