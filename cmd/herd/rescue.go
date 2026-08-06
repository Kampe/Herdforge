package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lanestate"
	"github.com/Kampe/Herdforge/pkg/posture"
)

// runRescue ports bin/herd-rescue: fix cramped or split agent panes.
//
//	herd rescue <pane-id> [--label NAME]   move a pane into its own full tab
//	herd rescue --empty-siblings           close blank shells left beside an agent
//
// One agent per tab is the fleet invariant: a split pane makes an agent's
// output unreadable and its pane ID ambiguous to every delivery path.
func runRescue() {
	fs := flag.NewFlagSet("rescue", flag.ExitOnError)
	emptySiblings := fs.Bool("empty-siblings", false, "Close blank shell panes left beside an agent pane")
	label := fs.String("label", "", "Label for the rescued tab (default: the agent's own name)")
	dryRun := fs.Bool("dry-run", false, "Report what would change without touching anything")
	fs.Parse(os.Args[2:])

	if *emptySiblings {
		// `tab create` + `agent start` can leave a blank shell beside the agent.
		// Close ONLY panes with no agent identity, and only when a live agent
		// pane remains in that tab — never the last pane, and never a pane that
		// might be an unrecognised agent.
		panes, err := herdr.PaneList()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd rescue: pane list: %v\n", err)
			os.Exit(1)
		}
		byTab := map[string][]herdr.PaneEntry{}
		for _, p := range panes {
			byTab[p.TabID] = append(byTab[p.TabID], p)
		}
		closed := 0
		for tab, group := range byTab {
			if len(group) < 2 {
				continue
			}
			keep := ""
			for _, p := range group {
				if p.AgentStatus != "" && p.AgentStatus != "unknown" {
					keep = p.PaneID
					break
				}
			}
			if keep == "" {
				// Nothing in this tab is identifiably an agent. Closing here
				// would be guessing, so leave the whole tab alone.
				continue
			}
			for _, p := range group {
				if p.PaneID == keep {
					continue
				}
				if p.AgentStatus != "" && p.AgentStatus != "unknown" {
					continue
				}
				if *dryRun {
					fmt.Printf("herd rescue: would close empty sibling %s (tab %s)\n", p.PaneID, tab)
					closed++
					continue
				}
				if err := herdr.PaneClose(p.PaneID); err != nil {
					fmt.Fprintf(os.Stderr, "herd rescue: close %s: %v\n", p.PaneID, err)
					continue
				}
				fmt.Printf("herd rescue: closed empty sibling %s\n", p.PaneID)
				closed++
			}
		}
		verb := "closed"
		if *dryRun {
			verb = "would close"
		}
		fmt.Printf("herd rescue: %s %d empty sibling pane(s)\n", verb, closed)
		return
	}

	pane := fs.Arg(0)
	if pane == "" {
		fmt.Fprintln(os.Stderr, "usage: herd rescue <pane-id> [--label NAME]  OR  herd rescue --empty-siblings")
		os.Exit(2)
	}
	name := *label
	if name == "" {
		name = "rescued"
		if agents, err := herdr.AgentList(); err == nil {
			for _, a := range agents {
				if a.PaneID == pane && a.Name != "" {
					name = a.Name
					break
				}
			}
		}
	}
	if *dryRun {
		fmt.Printf("herd rescue: would move %s -> new tab label=%s\n", pane, name)
		return
	}
	if err := herdr.PaneMoveToNewTab(pane, name); err != nil {
		fmt.Fprintf(os.Stderr, "herd rescue: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd rescue: moved %s -> tab label=%s\n", pane, name)
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
