package herdr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Geometry floors below which a multi-pane agent is treated as cramped.
// Solo full-tab agents are never rescued on size alone: only split geometry
// is recoverable without destroying the session.
const (
	MinRescueWidth  = 40
	MinRescueHeight = 8
)

// Rescue kinds are stable report labels for dry-run and apply.
const (
	RescueEmptySibling = "empty-sibling"
	RescueMultiAgent   = "multi-agent-split"
	RescueCramped      = "cramped-split"
)

// Rescue actions a finding may request.
const (
	RescueActionClose = "close-empty-sibling"
	RescueActionMove  = "move-to-new-tab"
	RescueActionNone  = "none"
)

// RescueFinding is one diagnosed pane/tab repair candidate, or an explicit
// refusal. Pure DiagnoseRescue never mutates; ApplyRescue mutates only when
// the finding is Safe and Action is not none.
type RescueFinding struct {
	Kind         string `json:"kind"`
	Action       string `json:"action"`
	TabID        string `json:"tab_id"`
	PaneID       string `json:"pane_id"`
	Workspace    string `json:"workspace_id,omitempty"`
	AgentName    string `json:"agent_name,omitempty"`
	AgentStatus  string `json:"agent_status,omitempty"`
	Reason       string `json:"reason"`
	Safe         bool   `json:"safe"`
	RefuseReason string `json:"refuse_reason,omitempty"`
	// BeforeCwd is the process/foreground cwd recorded before apply (and
	// during diagnose when process info is available). Empty means proof
	// was not established.
	BeforeCwd string `json:"before_cwd,omitempty"`
	// AfterCwd is filled by ApplyRescue after a successful mutation.
	AfterCwd string `json:"after_cwd,omitempty"`
	// KeepPaneID is the agent pane that must remain for empty-sibling closes.
	KeepPaneID string `json:"keep_pane_id,omitempty"`
	Label      string `json:"label,omitempty"`
}

// RescueReport is the full dry-run diagnosis of the live pane inventory.
type RescueReport struct {
	Findings []RescueFinding `json:"findings"`
	// Next is the single finding Apply would act on (first safe, deterministic).
	Next *RescueFinding `json:"next,omitempty"`
}

// RescueOptions controls ApplyRescue.
type RescueOptions struct {
	// Label overrides the rescued tab label for move actions.
	Label string
}

// isIdentifiableAgent reports a real agent lifecycle status. Empty and
// "unknown" are excluded: acting on them would be guessing.
func isIdentifiableAgent(status string) bool {
	switch strings.TrimSpace(status) {
	case "idle", "working", "starting", "done", "blocked":
		return true
	default:
		return false
	}
}

// agentPriority ranks which pane to keep in a multi-agent tab (higher wins).
// Focused panes are never candidates for move; among the rest, prefer the
// more "live" status so a done agent is the one un-split first.
func agentPriority(status string) int {
	switch strings.TrimSpace(status) {
	case "working":
		return 5
	case "starting":
		return 4
	case "idle":
		return 3
	case "blocked":
		return 2
	case "done":
		return 1
	default:
		return 0
	}
}

// DiagnoseRescue is pure policy over structured pane, agent, and layout data.
// It never calls herdr. Layouts may be nil or partial: multi-agent and
// empty-sibling diagnosis does not require geometry; cramped does.
//
// Invariants (acceptance):
//   - healthy single-agent tabs produce no findings
//   - focused panes are never selected as mutation targets
//   - unknown panes are never move targets (empty siblings may be closed
//     only when an identifiable agent remains as KeepPaneID)
//   - findings that cannot prove work attachment are emitted as Safe=false
func DiagnoseRescue(panes []PaneEntry, agents []AgentEntry, layouts map[string]PaneLayout) RescueReport {
	byName := map[string]AgentEntry{}
	for _, a := range agents {
		if a.PaneID != "" {
			byName[a.PaneID] = a
		}
	}

	// Enrich pane rows with agent names when pane list omits them.
	enriched := make([]PaneEntry, len(panes))
	copy(enriched, panes)
	for i := range enriched {
		if a, ok := byName[enriched[i].PaneID]; ok {
			if enriched[i].Name == "" && a.Name != "" {
				enriched[i].Name = a.Name
			}
			if enriched[i].AgentStatus == "" && a.Status != "" {
				enriched[i].AgentStatus = a.Status
			}
			if enriched[i].Cwd == "" && a.Cwd != "" {
				enriched[i].Cwd = a.Cwd
			}
		}
	}

	byTab := map[string][]PaneEntry{}
	var tabOrder []string
	for _, p := range enriched {
		if p.TabID == "" || p.PaneID == "" {
			continue
		}
		if _, seen := byTab[p.TabID]; !seen {
			tabOrder = append(tabOrder, p.TabID)
		}
		byTab[p.TabID] = append(byTab[p.TabID], p)
	}
	sort.Strings(tabOrder)

	var findings []RescueFinding
	for _, tabID := range tabOrder {
		group := byTab[tabID]
		sort.Slice(group, func(i, j int) bool { return group[i].PaneID < group[j].PaneID })
		findings = append(findings, diagnoseTab(tabID, group, layouts)...)
	}

	sortRescueFindings(findings)
	rep := RescueReport{Findings: findings}
	if next, ok := SelectNextRescue(findings); ok {
		n := next
		rep.Next = &n
	}
	return rep
}

func diagnoseTab(tabID string, group []PaneEntry, layouts map[string]PaneLayout) []RescueFinding {
	if len(group) == 0 {
		return nil
	}
	// Healthy: one pane, identifiable agent, not a multi-pane geometry problem.
	if len(group) == 1 {
		return nil
	}

	var agents, unknowns []PaneEntry
	for _, p := range group {
		if isIdentifiableAgent(p.AgentStatus) {
			agents = append(agents, p)
		} else {
			unknowns = append(unknowns, p)
		}
	}

	var out []RescueFinding

	// Multi-agent split: more than one identifiable agent in one tab.
	// Keep the highest-priority non-focused agent (or highest overall if all
	// focused — then refuse every move). Move each other non-focused agent.
	if len(agents) >= 2 {
		keep := pickKeepAgent(agents)
		for _, p := range agents {
			if keep.PaneID != "" && p.PaneID == keep.PaneID {
				continue
			}
			f := baseFinding(RescueMultiAgent, RescueActionMove, p)
			f.Reason = fmt.Sprintf("tab %s has %d identifiable agents; move %s so one agent remains per tab", tabID, len(agents), p.PaneID)
			f.KeepPaneID = keep.PaneID
			f.Label = rescueLabel(p)
			annotateSafety(&f, p)
			out = append(out, f)
		}
	}

	// Empty siblings: blank shells left beside an agent by tab create+start.
	if len(agents) >= 1 && len(unknowns) >= 1 {
		keep := pickKeepAgent(agents)
		if keep.PaneID == "" {
			// Every agent pane is focused or otherwise unusable as anchor —
			// refuse rather than guess which blank to close.
			for _, p := range unknowns {
				f := baseFinding(RescueEmptySibling, RescueActionNone, p)
				f.Reason = fmt.Sprintf("tab %s has empty siblings but no safe agent anchor", tabID)
				f.Safe = false
				f.RefuseReason = "no non-focused identifiable agent pane to keep"
				out = append(out, f)
			}
		} else {
			for _, p := range unknowns {
				f := baseFinding(RescueEmptySibling, RescueActionClose, p)
				f.Reason = fmt.Sprintf("blank sibling %s beside agent %s in tab %s", p.PaneID, keep.PaneID, tabID)
				f.KeepPaneID = keep.PaneID
				if p.Focused {
					f.Action = RescueActionNone
					f.Safe = false
					f.RefuseReason = "focused pane is never changed"
				} else if isIdentifiableAgent(p.AgentStatus) {
					// Defensive: unknowns filter already excludes these.
					f.Action = RescueActionNone
					f.Safe = false
					f.RefuseReason = "pane has agent identity; refusing close"
				} else {
					f.Safe = true
				}
				out = append(out, f)
			}
		}
	}

	// Cramped geometry: multi-pane tab where an agent rect is below floor.
	// Requires layout for the pane; without it we do not invent a size.
	if len(agents) >= 1 && len(group) >= 2 {
		for _, p := range agents {
			// Already scheduled as multi-agent move — skip duplicate cramped.
			if len(agents) >= 2 {
				continue
			}
			layout, ok := layouts[p.PaneID]
			if !ok {
				// Layout missing: emit refusal only when no other finding covers this tab.
				if len(out) == 0 {
					f := baseFinding(RescueCramped, RescueActionNone, p)
					f.Reason = fmt.Sprintf("tab %s is multi-pane but layout for %s is unavailable", tabID, p.PaneID)
					f.Safe = false
					f.RefuseReason = "session geometry unproven without pane layout"
					out = append(out, f)
				}
				continue
			}
			rect, ok := layout.RectFor(p.PaneID)
			if !ok {
				continue
			}
			if rect.Width >= MinRescueWidth && rect.Height >= MinRescueHeight {
				continue // geometry is fine; empty-sibling findings still apply
			}
			f := baseFinding(RescueCramped, RescueActionMove, p)
			f.Reason = fmt.Sprintf("agent pane %s geometry %dx%d is below %dx%d floor in multi-pane tab %s",
				p.PaneID, rect.Width, rect.Height, MinRescueWidth, MinRescueHeight, tabID)
			f.Label = rescueLabel(p)
			annotateSafety(&f, p)
			out = append(out, f)
		}
	}

	return out
}

func baseFinding(kind, action string, p PaneEntry) RescueFinding {
	return RescueFinding{
		Kind:        kind,
		Action:      action,
		TabID:       p.TabID,
		PaneID:      p.PaneID,
		Workspace:   p.Workspace,
		AgentName:   p.Name,
		AgentStatus: p.AgentStatus,
		BeforeCwd:   firstNonEmpty(p.ForegroundCwd, p.Cwd),
	}
}

func annotateSafety(f *RescueFinding, p PaneEntry) {
	if p.Focused {
		f.Action = RescueActionNone
		f.Safe = false
		f.RefuseReason = "focused pane is never changed"
		return
	}
	if !isIdentifiableAgent(p.AgentStatus) {
		f.Action = RescueActionNone
		f.Safe = false
		f.RefuseReason = "unknown pane is never changed"
		return
	}
	// Move requires a proven work cwd so reattachment/worktree ownership is
	// recoverable after the geometry change.
	if f.Action == RescueActionMove && strings.TrimSpace(f.BeforeCwd) == "" {
		f.Action = RescueActionNone
		f.Safe = false
		f.RefuseReason = "work preservation unproven: process/foreground cwd missing"
		return
	}
	f.Safe = true
}

func pickKeepAgent(agents []PaneEntry) PaneEntry {
	var best PaneEntry
	bestScore := -1
	for _, p := range agents {
		if p.Focused {
			// Prefer keeping a focused agent in place when several exist.
			score := 100 + agentPriority(p.AgentStatus)
			if score > bestScore || (score == bestScore && (best.PaneID == "" || p.PaneID < best.PaneID)) {
				best = p
				bestScore = score
			}
			continue
		}
		score := agentPriority(p.AgentStatus)
		if score > bestScore || (score == bestScore && (best.PaneID == "" || p.PaneID < best.PaneID)) {
			best = p
			bestScore = score
		}
	}
	return best
}

func rescueLabel(p PaneEntry) string {
	if name := strings.TrimSpace(p.Name); name != "" {
		return name
	}
	return "rescued"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func sortRescueFindings(findings []RescueFinding) {
	kindRank := func(k string) int {
		switch k {
		case RescueEmptySibling:
			return 0
		case RescueMultiAgent:
			return 1
		case RescueCramped:
			return 2
		default:
			return 9
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		// Safe actionable first.
		if a.Safe != b.Safe {
			return a.Safe
		}
		if ka, kb := kindRank(a.Kind), kindRank(b.Kind); ka != kb {
			return ka < kb
		}
		if a.TabID != b.TabID {
			return a.TabID < b.TabID
		}
		return a.PaneID < b.PaneID
	})
}

// SelectNextRescue returns the single finding Apply would execute: first
// Safe finding with a real action. Deterministic given sortRescueFindings.
func SelectNextRescue(findings []RescueFinding) (RescueFinding, bool) {
	for _, f := range findings {
		if f.Safe && (f.Action == RescueActionClose || f.Action == RescueActionMove) {
			return f, true
		}
	}
	return RescueFinding{}, false
}

// FilterEmptySiblingFindings returns only empty-sibling findings (for the
// --empty-siblings CLI mode).
func FilterEmptySiblingFindings(findings []RescueFinding) []RescueFinding {
	var out []RescueFinding
	for _, f := range findings {
		if f.Kind == RescueEmptySibling {
			out = append(out, f)
		}
	}
	return out
}

// FilterPaneFindings returns findings for a single pane id.
func FilterPaneFindings(findings []RescueFinding, paneID string) []RescueFinding {
	paneID = strings.TrimSpace(paneID)
	var out []RescueFinding
	for _, f := range findings {
		if f.PaneID == paneID {
			out = append(out, f)
		}
	}
	return out
}

// ErrRescueUnsafe is returned when Apply is asked to mutate an unsafe finding.
var ErrRescueUnsafe = errors.New("herdr rescue: finding is not safe to apply")

// ErrRescueNoTarget means diagnose found nothing actionable.
var ErrRescueNoTarget = errors.New("herdr rescue: no safe target")

// SnapshotRescue gathers live herdr state and returns a dry-run report.
func SnapshotRescue() (RescueReport, error) {
	panes, err := PaneList()
	if err != nil {
		return RescueReport{}, err
	}
	agents, err := AgentList()
	if err != nil {
		// Agent list failure degrades name enrichment; pane list alone still
		// diagnoses empty siblings and multi-agent splits by status fields.
		agents = nil
	}
	layouts := map[string]PaneLayout{}
	// Load layout only for multi-pane tabs (cramped detection). Failures are
	// non-fatal: diagnose emits refuse findings when geometry is required.
	byTab := map[string][]PaneEntry{}
	for _, p := range panes {
		byTab[p.TabID] = append(byTab[p.TabID], p)
	}
	for _, group := range byTab {
		if len(group) < 2 {
			continue
		}
		for _, p := range group {
			if !isIdentifiableAgent(p.AgentStatus) {
				continue
			}
			layout, err := GetPaneLayout(p.PaneID)
			if err != nil {
				continue
			}
			layouts[p.PaneID] = layout
		}
	}
	return DiagnoseRescue(panes, agents, layouts), nil
}

// ApplyRescue performs exactly one proven repair. On close, the keep agent
// pane is re-checked before mutation. On move, before/after process cwd is
// recorded; a post-move cwd mismatch is reported but the pane is left where
// herdr put it (the original tab no longer holds it — recovery is the new
// tab identity + recorded cwd, not a reverse move that could double-split).
func ApplyRescue(f RescueFinding, opts RescueOptions) (RescueFinding, error) {
	if !f.Safe || (f.Action != RescueActionClose && f.Action != RescueActionMove) {
		if f.RefuseReason != "" {
			return f, fmt.Errorf("%w: %s", ErrRescueUnsafe, f.RefuseReason)
		}
		return f, ErrRescueUnsafe
	}

	// Re-prove the target still looks like the diagnosis: refuse if the live
	// pane list no longer matches (idempotent re-run → ErrRescueNoTarget).
	panes, err := PaneList()
	if err != nil {
		return f, err
	}
	live, ok := findPane(panes, f.PaneID)
	if !ok {
		return f, fmt.Errorf("%w: pane %s gone (already rescued or closed)", ErrRescueNoTarget, f.PaneID)
	}
	if live.Focused {
		f.Safe = false
		f.RefuseReason = "focused pane is never changed"
		return f, fmt.Errorf("%w: %s", ErrRescueUnsafe, f.RefuseReason)
	}

	switch f.Action {
	case RescueActionClose:
		if isIdentifiableAgent(live.AgentStatus) {
			f.Safe = false
			f.RefuseReason = "pane gained agent identity; refusing close"
			return f, fmt.Errorf("%w: %s", ErrRescueUnsafe, f.RefuseReason)
		}
		if f.KeepPaneID != "" {
			keep, ok := findPane(panes, f.KeepPaneID)
			if !ok || !isIdentifiableAgent(keep.AgentStatus) {
				f.Safe = false
				f.RefuseReason = "keep agent pane no longer present or identifiable"
				return f, fmt.Errorf("%w: %s", ErrRescueUnsafe, f.RefuseReason)
			}
		}
		if err := PaneClose(f.PaneID); err != nil {
			return f, err
		}
		// Prove keep agent still live; if not, report failure but cannot
		// un-close. Callers treat non-nil error as failed proof.
		if f.KeepPaneID != "" {
			after, err := PaneList()
			if err != nil {
				return f, fmt.Errorf("closed %s but could not verify keep pane: %w", f.PaneID, err)
			}
			keep, ok := findPane(after, f.KeepPaneID)
			if !ok {
				return f, fmt.Errorf("closed %s but keep pane %s is gone", f.PaneID, f.KeepPaneID)
			}
			f.AfterCwd = firstNonEmpty(keep.ForegroundCwd, keep.Cwd)
		}
		return f, nil

	case RescueActionMove:
		if !isIdentifiableAgent(live.AgentStatus) {
			f.Safe = false
			f.RefuseReason = "unknown pane is never changed"
			return f, fmt.Errorf("%w: %s", ErrRescueUnsafe, f.RefuseReason)
		}
		// Prefer live process cwd over stale diagnosis snapshot.
		beforeCwd := firstNonEmpty(live.ForegroundCwd, live.Cwd, f.BeforeCwd)
		if procs, err := PaneProcessInfo(f.PaneID); err == nil {
			for _, pr := range procs {
				if c := strings.TrimSpace(pr.Cwd); c != "" {
					beforeCwd = c
					break
				}
			}
		}
		if beforeCwd == "" {
			f.Safe = false
			f.RefuseReason = "work preservation unproven: process/foreground cwd missing"
			return f, fmt.Errorf("%w: %s", ErrRescueUnsafe, f.RefuseReason)
		}
		f.BeforeCwd = beforeCwd

		label := strings.TrimSpace(opts.Label)
		if label == "" {
			label = strings.TrimSpace(f.Label)
		}
		if label == "" {
			label = rescueLabel(live)
		}
		if err := PaneMoveToNewTab(f.PaneID, label); err != nil {
			// Move failed: original tab/pane still holds the agent.
			return f, err
		}
		// Record after cwd from the (possibly re-homed) pane.
		afterPanes, err := PaneList()
		if err == nil {
			if after, ok := findPane(afterPanes, f.PaneID); ok {
				f.AfterCwd = firstNonEmpty(after.ForegroundCwd, after.Cwd)
				f.TabID = after.TabID
			}
		}
		if f.AfterCwd == "" {
			if procs, err := PaneProcessInfo(f.PaneID); err == nil {
				for _, pr := range procs {
					if c := strings.TrimSpace(pr.Cwd); c != "" {
						f.AfterCwd = c
						break
					}
				}
			}
		}
		if f.AfterCwd == "" {
			f.AfterCwd = beforeCwd // best-effort: process may still be at before
		}
		return f, nil
	}
	return f, ErrRescueUnsafe
}

func findPane(panes []PaneEntry, id string) (PaneEntry, bool) {
	for _, p := range panes {
		if p.PaneID == id {
			return p, true
		}
	}
	return PaneEntry{}, false
}
