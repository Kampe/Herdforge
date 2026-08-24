// Package attention produces the coordinator-eyes triage: a list of
// standing agents that need a human (or the orchestrator) to look at them
// right now.
//
// Port of bin/herd-attention (zsh) which ran `herdr agent list` and piped
// it through jq filtering. The Go port cross-references the live agent
// list against the canonical standing roster (kick.StandingIDs() — the
// single source of truth shared with kick and standing), classifies each
// lane by urgency, and emits a deterministic, urgency-sorted triage.
//
// An agent "needs eyes" when it is NOT actively working/starting — i.e.
// it is done (awaiting review/harvest), blocked (stuck), idle (may need a
// kick), missing (fleet gap — needs raising), held (parked by the
// coordinator), or suffering a provider death (needs reroute). Healthy
// working/starting agents are LevelNone and excluded from the default
// output.
//
// Triage is a pure function with injected held/provider-death checkers so
// it is fully deterministic and testable; Run wires the real
// implementations (kick.LaneHeld, kick.ProviderDeathCheck, herdr agent
// list) for the CLI path.
package attention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// AttentionLevel ranks how urgently a lane needs coordinator eyes.
type AttentionLevel string

const (
	// LevelCritical: blocked or provider death — needs action now.
	LevelCritical AttentionLevel = "critical"
	// LevelHigh: done (awaiting review/harvest) or missing from the fleet.
	LevelHigh AttentionLevel = "high"
	// LevelMedium: idle/unknown — may need a kick if there is work.
	LevelMedium AttentionLevel = "medium"
	// LevelMissing: standing ID not live — fleet gap, needs raising.
	// (Kept distinct from High so callers can filter on fleet gaps alone.)
	LevelMissing AttentionLevel = "missing"
	// LevelLow: held by the coordinator — informational, parked.
	LevelLow AttentionLevel = "low"
	// LevelNone: working/starting — healthy, no eyes needed.
	LevelNone AttentionLevel = "none"
)

// Item is one lane in the attention triage.
type Item struct {
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Level      AttentionLevel `json:"level"`
	Reason     string         `json:"reason"`
	PaneID     string         `json:"pane_id,omitempty"`
	Held       bool           `json:"held,omitempty"`
	HeldReason string         `json:"held_reason,omitempty"`
}

// Result is the full attention triage.
type Result struct {
	Items   []Item                 `json:"items"`
	Counts  map[AttentionLevel]int `json:"counts"`
	Total   int                    `json:"total"`
	Needing int                    `json:"needing"`
}

// urgencyRank maps a level to a numeric urgency (higher = more urgent).
// Used for deterministic sort: (urgency DESC, name ASC).
func urgencyRank(l AttentionLevel) int {
	switch l {
	case LevelCritical:
		return 5
	case LevelHigh:
		return 4
	case LevelMedium:
		return 3
	case LevelMissing:
		return 2
	case LevelLow:
		return 1
	default:
		return 0
	}
}

// NeedsEyes reports whether a level warrants coordinator attention.
// LevelNone (working/starting) is the only level that does not.
func NeedsEyes(l AttentionLevel) bool {
	return l != LevelNone
}

// classifyStatus maps a raw agent status string to an attention level
// (ignoring hold/provider-death, which are applied by ClassifyAgent).
func classifyStatus(status string) (AttentionLevel, string) {
	switch status {
	case "working", "starting":
		return LevelNone, status
	case "done":
		return LevelHigh, "done — awaiting review/harvest"
	case "blocked":
		return LevelCritical, "blocked — needs unblock or reassign"
	case "idle":
		return LevelMedium, "idle — may need kick if there is work"
	case "", "unknown":
		return LevelMedium, "unknown status — read pane"
	default:
		return LevelMedium, fmt.Sprintf("status=%s — read pane", status)
	}
}

// ClassifyAgent classifies a single live agent entry into an attention
// item. Held takes precedence over everything (the coordinator already
// parked the lane); provider death takes precedence over the raw status
// for non-held lanes.
func ClassifyAgent(a kick.AgentEntry, held bool, heldReason string, providerDeath bool) Item {
	name := a.Name
	if name == "" {
		name = a.Label
	}
	status := a.Status
	if status == "" {
		status = "unknown"
	}

	item := Item{
		Name:   name,
		Status: status,
		PaneID: a.PaneID,
	}

	switch {
	case strings.HasPrefix(heldReason, "authority-error:"):
		item.Level = LevelCritical
		item.Reason = strings.TrimSpace(strings.TrimPrefix(heldReason, "authority-error:"))
	case held:
		item.Level = LevelLow
		item.Held = true
		item.HeldReason = heldReason
		if item.HeldReason == "" {
			item.HeldReason = "held by coordinator"
		}
		item.Reason = "held — parked by coordinator"
	case providerDeath:
		item.Level = LevelCritical
		item.Reason = "provider death — needs reroute (cooled reset-aware)"
	default:
		lvl, reason := classifyStatus(status)
		item.Level = lvl
		item.Reason = reason
	}

	return item
}

// Triage produces the coordinator-eyes triage from a live agent list and
// the standing roster. heldChecker and providerDeathChecker are injected
// so the function is fully deterministic and testable.
//
// Every standing ID is examined. IDs that are live but working/starting
// are LevelNone and excluded from Items (but counted in Total). The
// roster is the source of truth — extra live agents not in the roster are
// ignored, matching the original jq-filter-on-standing behavior.
func Triage(
	agents []kick.AgentEntry,
	standingIDs []string,
	heldChecker func(string) (string, bool),
	providerDeathChecker func(string) bool,
) Result {
	// Build a name->agent index for O(1) lookup (herdr may use name or label).
	index := make(map[string]kick.AgentEntry, len(agents))
	for _, a := range agents {
		if a.Name != "" {
			index[a.Name] = a
		}
		if a.Label != "" && a.Label != a.Name {
			index[a.Label] = a
		}
	}

	counts := map[AttentionLevel]int{}
	var items []Item

	for _, id := range standingIDs {
		a, found := index[id]
		if !found {
			// Missing standing agent — fleet gap.
			item := Item{
				Name:   id,
				Status: "missing",
				Level:  LevelMissing,
				Reason: "not live — needs raising (herd standing --only " + id + ")",
			}
			counts[LevelMissing]++
			items = append(items, item)
			continue
		}

		heldReason, held := heldChecker(id)
		providerDeath := providerDeathChecker(id)
		item := ClassifyAgent(a, held, heldReason, providerDeath)
		counts[item.Level]++
		if NeedsEyes(item.Level) {
			items = append(items, item)
		}
	}

	// Deterministic sort: (urgency DESC, name ASC).
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := urgencyRank(items[i].Level), urgencyRank(items[j].Level)
		if ri != rj {
			return ri > rj
		}
		return items[i].Name < items[j].Name
	})

	needing := 0
	for lvl, c := range counts {
		if NeedsEyes(lvl) {
			needing += c
		}
	}

	return Result{
		Items:   items,
		Counts:  counts,
		Total:   len(standingIDs),
		Needing: needing,
	}
}

// Summary returns a one-line human-readable triage summary.
func Summary(r Result) string {
	// FAC-604: scanning nothing is not a clean bill of health. When the standing
	// roster resolves empty -- as it does whenever lane-registry.json parses with
	// lanes but no standing flags, since that path never falls back to
	// .herd/herd.yaml -- every lane is invisible and Needing is trivially 0. This
	// reported "fleet healthy — 0 lane(s) scanned" while the orchestrator sat
	// done and the review supervisor sat idle, so coordinator-eyes triage was a
	// false green and no loop could use it as a wake signal.
	if r.Total == 0 {
		return "herd-attention: UNKNOWN — no standing lanes resolved, so nothing was scanned; " +
			"this is not a healthy fleet, it is an unresolved roster (check lane-registry.json standing flags vs .herd/herd.yaml)"
	}
	if r.Needing == 0 {
		return fmt.Sprintf("herd-attention: fleet healthy — %d lane(s) scanned, none need eyes", r.Total)
	}
	var parts []string
	for _, lvl := range []AttentionLevel{LevelCritical, LevelHigh, LevelMedium, LevelMissing, LevelLow} {
		if c := r.Counts[lvl]; c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, lvl))
		}
	}
	return fmt.Sprintf("herd-attention: %d of %d lane(s) need eyes — %s", r.Needing, r.Total, strings.Join(parts, ", "))
}

// FormatItem returns a one-line display string for a single triage item.
func FormatItem(item Item) string {
	extra := ""
	if item.Held {
		extra = fmt.Sprintf(" [held: %s]", item.HeldReason)
	}
	if item.PaneID != "" {
		extra += fmt.Sprintf(" pane=%s", item.PaneID)
	}
	return fmt.Sprintf("[%s] %s (%s) — %s%s", item.Level, item.Name, item.Status, item.Reason, extra)
}

// noHold is a heldChecker that reports no lane is held. Shared by Selftest
// and the test suite.
func noHold(string) (string, bool) { return "", false }

// noDeath is a providerDeathChecker that reports no provider death.
func noDeath(string) bool { return false }

// Selftest runs the package self-test assertions and returns an error on
// first failure. Mirrors the --selftest mode of the zsh original.
func Selftest() error {
	// 1. NeedsEyes is exhaustive.
	for _, lvl := range []AttentionLevel{LevelCritical, LevelHigh, LevelMedium, LevelMissing, LevelLow} {
		if !NeedsEyes(lvl) {
			return fmt.Errorf("NeedsEyes(%s) = false, want true", lvl)
		}
	}
	if NeedsEyes(LevelNone) {
		return fmt.Errorf("NeedsEyes(none) = true, want false")
	}

	// 2. Urgency ranks are strictly ordered and distinct.
	ranks := []AttentionLevel{LevelCritical, LevelHigh, LevelMedium, LevelMissing, LevelLow, LevelNone}
	prev := urgencyRank(ranks[0])
	for _, lvl := range ranks[1:] {
		cur := urgencyRank(lvl)
		if cur >= prev {
			return fmt.Errorf("urgency rank not strictly descending at %s: %d >= %d", lvl, cur, prev)
		}
		prev = cur
	}

	// 3. A working agent does not need eyes.
	working := ClassifyAgent(kick.AgentEntry{Name: "x", Status: "working", PaneID: "p"}, false, "", false)
	if NeedsEyes(working.Level) {
		return fmt.Errorf("working agent classified as needing eyes: %s", working.Level)
	}

	// 4. A blocked agent is critical.
	blocked := ClassifyAgent(kick.AgentEntry{Name: "x", Status: "blocked", PaneID: "p"}, false, "", false)
	if blocked.Level != LevelCritical {
		return fmt.Errorf("blocked classified as %s, want critical", blocked.Level)
	}

	// 5. A missing standing ID is reported as missing.
	r := Triage(nil, []string{"ghost-lane"}, noHold, noDeath)
	if r.Counts[LevelMissing] != 1 {
		return fmt.Errorf("missing count = %d, want 1", r.Counts[LevelMissing])
	}

	// 6. Sort is deterministic — same input yields same order regardless of roster order.
	agents := []kick.AgentEntry{
		{Name: "zeta", Status: "done", PaneID: "p1"},
		{Name: "alpha", Status: "done", PaneID: "p2"},
	}
	r1 := Triage(agents, []string{"zeta", "alpha"}, noHold, noDeath)
	r2 := Triage(agents, []string{"alpha", "zeta"}, noHold, noDeath)
	if r1.Items[0].Name != r2.Items[0].Name {
		return fmt.Errorf("non-deterministic sort: %s vs %s", r1.Items[0].Name, r2.Items[0].Name)
	}

	return nil
}

// Run executes a live attention triage: fetches the agent list from herdr,
// cross-references the standing roster, and returns the triage. On herdr
// failure it returns an error (fail-closed); the caller decides whether to
// proceed with an empty agent list.
func Run() (*Result, error) {
	return nil, fmt.Errorf("herd-attention: durable hold authority is required")
}

func RunWithHoldReader(reader lifecycle.HoldReader, repository string) (*Result, error) {
	return nil, fmt.Errorf("herd-attention: canonical lane registry and active-task resolver are required")
}

func RunWithHoldReaderAndTasks(reader lifecycle.HoldReader, repository string, resolver lifecycle.ActiveTaskResolver, registry lifecycle.CanonicalLaneRegistry) (*Result, error) {
	if reader == nil || strings.TrimSpace(repository) == "" {
		return nil, fmt.Errorf("herd-attention: durable hold authority and repository identity are required")
	}
	agents, err := kick.FetchAgentList()
	if err != nil {
		return nil, fmt.Errorf("herd-attention: %w", err)
	}
	heldFacts := map[string]string{}
	for _, name := range kick.StandingIDs() {
		if _, live := findAttentionAgent(agents, name); !live {
			continue
		}
		lane, resolveErr := registry.ResolveLiveAgentID(name)
		if resolveErr != nil {
			return authorityFailure(name, resolveErr)
		}
		generation := func(ctx context.Context, identity lifecycle.HoldIdentity) (int64, error) {
			if source, ok := reader.(interface {
				CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error)
			}); ok {
				return source.CurrentGeneration(ctx, identity)
			}
			return 0, fmt.Errorf("herd-attention: current generation source is required")
		}
		err := lifecycle.CheckLaneAndTaskHold(context.Background(), reader, resolver, repository, lane.Role, lane.Name, generation)
		if err != nil {
			if errors.Is(err, lifecycle.ErrHoldDenied) {
				heldFacts[name] = err.Error()
				continue
			}
			return authorityFailure(name, err)
		}
	}
	check := func(name string) (string, bool) { reason, held := heldFacts[name]; return reason, held }
	r := Triage(agents, kick.StandingIDs(), check, kick.ProviderDeathCheck)
	return &r, nil
}

func authorityFailure(name string, err error) (*Result, error) {
	item := Item{Name: name, Status: "unknown", Level: LevelCritical, Reason: "hold authority unavailable: " + err.Error()}
	return &Result{
		Items:   []Item{item},
		Counts:  map[AttentionLevel]int{LevelCritical: 1},
		Total:   1,
		Needing: 1,
	}, err
}

func findAttentionAgent(agents []kick.AgentEntry, name string) (kick.AgentEntry, bool) {
	for _, agent := range agents {
		if agent.Name == name || agent.Label == name {
			return agent, true
		}
	}
	return kick.AgentEntry{}, false
}

// MarshalJSON ensures Result (with its map) serializes cleanly.
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result
	return json.Marshal(alias(r))
}
