package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lanestate"
	"github.com/Kampe/Herdforge/pkg/posture"
)

// rescueArgs is the resolved rescue command line.
type rescueArgs struct {
	emptySiblings bool
	label         string
	workspace     string
	apply         bool
	asJSON        bool
	paneID        string
}

func rescueUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: herd rescue [--workspace ID] [--json]")
	fmt.Fprintln(w, "       herd rescue --apply [--workspace ID]")
	fmt.Fprintln(w, "       herd rescue <pane-id> [--label NAME] [--apply]")
	fmt.Fprintln(w, "       herd rescue --empty-siblings [--apply] [--workspace ID]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Default is dry-run: exact tab/pane IDs and reasons, no mutations.")
	fmt.Fprintln(w, "--apply repairs exactly one proven target, then exits.")
}

// parseRescueArgs resolves rescue flags with the pane-id positional allowed
// anywhere on the line. Go's flag package stops at the first non-flag token,
// so the documented `herd rescue <pane-id> --apply` would otherwise leave
// apply=false, take the dry-run branch, and exit 0 without repairing anything.
// Flags are re-parsed after each positional so the flag package itself decides
// which tokens are values (--label NAME) and which are positionals.
func parseRescueArgs(args []string) (rescueArgs, error) {
	fs := flag.NewFlagSet("rescue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	emptySiblings := fs.Bool("empty-siblings", false, "Only diagnose/close blank shell panes left beside an agent")
	label := fs.String("label", "", "Label for a rescued tab (move actions)")
	workspace := fs.String("workspace", "", "Limit diagnosis to one herdr workspace id")
	apply := fs.Bool("apply", false, "Perform one proven repair (default is dry-run)")
	// --dry-run is accepted for chainseer-script compatibility; it is the default.
	dryRun := fs.Bool("dry-run", true, "Report findings without mutating (default true; --apply overrides)")
	asJSON := fs.Bool("json", false, "Emit the diagnosis report as JSON")

	if err := fs.Parse(args); err != nil {
		return rescueArgs{}, err
	}
	// Collect positionals with flags interleaved anywhere after the first one.
	var pos []string
	rest := fs.Args()
	for len(rest) > 0 {
		pos = append(pos, rest[0])
		if err := fs.Parse(rest[1:]); err != nil {
			return rescueArgs{}, err
		}
		rest = fs.Args()
	}
	if len(pos) > 1 {
		return rescueArgs{}, fmt.Errorf("rescue takes at most one pane id, got %d (%s)", len(pos), strings.Join(pos, " "))
	}

	// --apply is the only mutation gate. Refuse only when --dry-run was set
	// AND asks for dry: `--dry-run=false --apply` is coherent, not a conflict.
	explicitDry := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "dry-run" && *dryRun {
			explicitDry = true
		}
	})
	if explicitDry && *apply {
		return rescueArgs{}, errors.New("--dry-run and --apply are mutually exclusive")
	}

	a := rescueArgs{
		emptySiblings: *emptySiblings,
		label:         *label,
		workspace:     *workspace,
		apply:         *apply,
		asJSON:        *asJSON,
	}
	if len(pos) == 1 {
		a.paneID = pos[0]
	}
	return a, nil
}

// runRescue diagnoses and repairs cramped/split agent panes.
//
//	herd rescue [--workspace ID] [--json]              dry-run diagnose (default)
//	herd rescue --apply [--workspace ID]               repair ONE proven target
//	herd rescue <pane-id> [--label NAME] [--apply]     target one pane
//	herd rescue --empty-siblings [--apply]             only blank-sibling closes
//
// Dry-run is the default. Mutation requires --apply. One agent per tab is the
// fleet invariant: a split pane makes output unreadable and pane IDs ambiguous
// to every delivery path. Healthy, focused, and unknown panes are never moved;
// blank unknown siblings may be closed only when an identifiable agent remains.
//
// -h/--help is consumed by the global gate in main.go (rescue is registered in
// subcommandUsage), so help never reaches herdr/process inspection.
func runRescue() {
	a, err := parseRescueArgs(os.Args[2:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			rescueUsage(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "herd rescue: %v\n", err)
		rescueUsage(os.Stderr)
		os.Exit(2)
	}
	doApply := a.apply

	if !herdr.IsAvailable() {
		fmt.Fprintln(os.Stderr, "herd rescue: herdr CLI not found")
		os.Exit(1)
	}

	rep, err := herdr.SnapshotRescue()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd rescue: diagnose: %v\n", err)
		os.Exit(1)
	}

	findings := rep.Findings
	if ws := strings.TrimSpace(a.workspace); ws != "" {
		findings = filterWorkspace(findings, ws)
	}
	if a.emptySiblings {
		findings = herdr.FilterEmptySiblingFindings(findings)
	}
	paneArg := a.paneID
	if paneArg != "" {
		findings = herdr.FilterPaneFindings(findings, paneArg)
		// Explicit pane with no current finding: still allow a forced move
		// dry-run description only when the pane exists and is identifiable —
		// never invent a close. Forced path uses live list for the report.
		if len(findings) == 0 && !doApply {
			if f, ok := forcedPaneFinding(paneArg, a.label); ok {
				findings = []herdr.RescueFinding{f}
			}
		}
	}

	// Re-bind Next after filters.
	next, hasNext := herdr.SelectNextRescue(findings)

	if a.asJSON {
		out := struct {
			Findings []herdr.RescueFinding `json:"findings"`
			Next     *herdr.RescueFinding  `json:"next,omitempty"`
			Apply    bool                  `json:"apply"`
		}{Findings: findings, Apply: doApply}
		if hasNext {
			n := next
			out.Next = &n
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "herd rescue: json: %v\n", err)
			os.Exit(1)
		}
		if doApply {
			if !hasNext {
				fmt.Fprintln(os.Stderr, "herd rescue: no safe target to apply")
				os.Exit(1)
			}
			if err := applyOne(next, a.label); err != nil {
				fmt.Fprintf(os.Stderr, "herd rescue: %v\n", err)
				os.Exit(1)
			}
		}
		return
	}

	if len(findings) == 0 {
		if paneArg != "" {
			fmt.Printf("herd rescue: no actionable finding for pane %s (healthy, focused, unknown, or already one-agent-per-tab)\n", paneArg)
		} else {
			fmt.Println("herd rescue: no cramped/split targets (fleet already one-agent-per-tab)")
		}
		if doApply {
			os.Exit(1)
		}
		return
	}

	for _, f := range findings {
		printFinding(f, !doApply)
	}
	if !doApply {
		if hasNext {
			fmt.Printf("herd rescue: dry-run complete; next apply target %s (%s). Re-run with --apply to repair one target.\n",
				next.PaneID, next.Kind)
		} else {
			fmt.Println("herd rescue: dry-run complete; no safe apply target (see refuse reasons above)")
		}
		return
	}

	if !hasNext {
		fmt.Fprintln(os.Stderr, "herd rescue: no safe target to apply")
		os.Exit(1)
	}
	if err := applyOne(next, a.label); err != nil {
		fmt.Fprintf(os.Stderr, "herd rescue: %v\n", err)
		os.Exit(1)
	}
}

func applyOne(f herdr.RescueFinding, label string) error {
	got, err := herdr.ApplyRescue(f, herdr.RescueOptions{Label: label})
	if err != nil {
		if errors.Is(err, herdr.ErrRescueNoTarget) || errors.Is(err, herdr.ErrRescueUnsafe) {
			return err
		}
		return err
	}
	switch got.Action {
	case herdr.RescueActionClose:
		fmt.Printf("herd rescue: closed empty sibling %s (kept %s)", got.PaneID, got.KeepPaneID)
		if got.AfterCwd != "" {
			fmt.Printf(" keep_cwd=%s", got.AfterCwd)
		}
		fmt.Println()
	case herdr.RescueActionMove:
		fmt.Printf("herd rescue: moved %s -> tab label=%s", got.PaneID, firstLabel(got, label))
		if got.BeforeCwd != "" {
			fmt.Printf(" before_cwd=%s", got.BeforeCwd)
		}
		if got.AfterCwd != "" {
			fmt.Printf(" after_cwd=%s", got.AfterCwd)
		}
		if got.TabID != "" {
			fmt.Printf(" tab=%s", got.TabID)
		}
		fmt.Println()
	default:
		fmt.Printf("herd rescue: applied %s on %s\n", got.Action, got.PaneID)
	}
	return nil
}

func firstLabel(f herdr.RescueFinding, override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	if s := strings.TrimSpace(f.Label); s != "" {
		return s
	}
	if s := strings.TrimSpace(f.AgentName); s != "" {
		return s
	}
	return "rescued"
}

func printFinding(f herdr.RescueFinding, dry bool) {
	mode := "WOULD"
	if !dry {
		mode = "PLAN"
	}
	if !f.Safe {
		fmt.Printf("herd rescue: REFUSE %s pane=%s tab=%s — %s (%s)\n",
			f.Kind, f.PaneID, f.TabID, f.RefuseReason, f.Reason)
		return
	}
	fmt.Printf("herd rescue: %s %s action=%s pane=%s tab=%s", mode, f.Kind, f.Action, f.PaneID, f.TabID)
	if f.KeepPaneID != "" {
		fmt.Printf(" keep=%s", f.KeepPaneID)
	}
	if f.AgentName != "" {
		fmt.Printf(" agent=%s", f.AgentName)
	}
	if f.BeforeCwd != "" {
		fmt.Printf(" cwd=%s", f.BeforeCwd)
	}
	fmt.Printf(" — %s\n", f.Reason)
}

func filterWorkspace(findings []herdr.RescueFinding, ws string) []herdr.RescueFinding {
	var out []herdr.RescueFinding
	for _, f := range findings {
		if f.Workspace == ws {
			out = append(out, f)
		}
	}
	return out
}

// forcedPaneFinding builds a dry-run move description for an explicit pane id
// that is currently alone (not in the diagnose set) so operators still see
// what --apply would require. Never Safe: forced apply still goes through
// DiagnoseRescue filters on re-run; this is display-only for dry-run.
func forcedPaneFinding(paneID, label string) (herdr.RescueFinding, bool) {
	panes, err := herdr.PaneList()
	if err != nil {
		return herdr.RescueFinding{}, false
	}
	for _, p := range panes {
		if p.PaneID != paneID {
			continue
		}
		f := herdr.RescueFinding{
			Kind:         "explicit",
			Action:       herdr.RescueActionNone,
			TabID:        p.TabID,
			PaneID:       p.PaneID,
			Workspace:    p.Workspace,
			AgentName:    p.Name,
			AgentStatus:  p.AgentStatus,
			BeforeCwd:    p.ForegroundCwd,
			Reason:       "explicit pane id: no split/cramped finding (already healthy or not rescuable)",
			Safe:         false,
			RefuseReason: "no proven geometry/process defect for this pane",
			Label:        label,
		}
		if f.BeforeCwd == "" {
			f.BeforeCwd = p.Cwd
		}
		return f, true
	}
	return herdr.RescueFinding{}, false
}

// runSeedLaneState ports bin/herd-seed-lane-state.
func runSeedLaneState() {
	fs := flag.NewFlagSet("seed-lane-state", flag.ExitOnError)
	fs.Parse(os.Args[2:])
	wt, lane := fs.Arg(0), fs.Arg(1)
	if wt == "" || lane == "" {
		fmt.Fprintln(os.Stderr, "usage: herd seed-lane-state <worktree-path> <lane-id>")
		os.Exit(2)
	}
	if info, err := os.Stat(wt); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "herd seed-lane-state: %s is not a directory\n", wt)
		os.Exit(2)
	}

	stateRoot := posture.StateDir()
	results := lanestate.Seed(wt, lane, stateRoot, time.Now().UTC())
	for _, r := range results {
		switch r.Outcome {
		case lanestate.Restored:
			fmt.Printf("RESTORED %s for %s (%s)\n", r.Artifact, lane, r.Detail)
		case lanestate.Seeded:
			fmt.Printf("seeded %s for %s (%s)\n", r.Artifact, lane, r.Detail)
		case lanestate.Failed:
			fmt.Fprintf(os.Stderr, "WARN could not seed %s for %s: %s\n", r.Artifact, lane, r.Detail)
		}
	}

	_, hasPriorMail := os.Stat(lanestate.MailPath(stateRoot, lane))
	if lanestate.ContinuityLost(results, hasPriorMail == nil) {
		fmt.Fprintln(os.Stderr, lanestate.ContinuityWarning(lane, lanestate.SnapshotDir(stateRoot, lane)))
	}
	// Always exit 0: a missing ledger must never keep a lane down.
	_ = filepath.Clean(wt)
}
