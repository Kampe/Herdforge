package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/stop"
)

// runStop ports bin/herd-stop: stop the herd without deleting worktrees or
// branches. Dry-run by default; --execute performs the wind-down.
func runStop() {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	execute := fs.Bool("execute", false, "Perform the stop (default is a dry run)")
	forceWorking := fs.Bool("force-working", false, "Also close working/starting agents (explicit operator kill)")
	includeCoordinator := fs.Bool("include-coordinator", false, "Also stop the coordinator (only after handoff)")
	workspace := fs.String("workspace", "", "Herdr workspace ID (default: resolved for this repo)")
	fs.Parse(os.Args[2:])

	ws := *workspace
	if ws == "" {
		var err error
		ws, err = herdr.RequireWorkspace(".")
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd stop: workspace unresolved: %v\n", err)
			os.Exit(1)
		}
	}

	// Standing lanes get a durable hold so the kick loop cannot revive what we
	// just stopped.
	standing := map[string]bool{}
	if cfg, err := config.LoadConfig(".herd/herd.yaml"); err == nil && cfg != nil {
		for _, lane := range cfg.Lanes {
			if lane.Standing {
				standing[lane.Name] = true
			}
		}
	}

	agents, err := herdr.AgentList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd stop: agent list: %v\n", err)
		os.Exit(1)
	}
	var fleet []stop.Agent
	for _, a := range agents {
		if a.Workspace != ws {
			continue
		}
		name := a.Name
		if name == "" {
			name = "unknown"
		}
		fleet = append(fleet, stop.Agent{Name: name, Status: a.Status, PaneID: a.PaneID, TabID: a.TabID})
	}

	plan := stop.Plan(fleet, stop.Options{
		ForceWorking:       *forceWorking,
		IncludeCoordinator: *includeCoordinator,
		StandingLanes:      standing,
	})

	for _, d := range plan {
		switch d.Action {
		case stop.Protect:
			fmt.Printf("PROTECT name=%s status=%s tab=%s (%s)\n", d.Agent.Name, d.Agent.Status, d.Agent.TabID, d.Reason)
			continue
		case stop.Preserve:
			if *execute && d.Agent.PaneID != "" {
				if _, err := herdr.Send(d.Agent.PaneID, stop.StopMessage, true, 30*time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "herd stop: WARN stop request to %s unproven: %v\n", d.Agent.Name, err)
				}
			}
			fmt.Printf("PRESERVE name=%s status=%s pane=%s (%s)\n", d.Agent.Name, d.Agent.Status, d.Agent.PaneID, d.Reason)
			continue
		}

		action := "WOULD_CLOSE"
		if *execute {
			action = "CLOSE"
		}
		if *execute && d.RequestStop && d.Agent.PaneID != "" {
			if _, err := herdr.Send(d.Agent.PaneID, stop.StopMessage, true, 30*time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "herd stop: WARN stop request to %s unproven: %v\n", d.Agent.Name, err)
			}
		}
		fmt.Printf("%s name=%s status=%s tab=%s\n", action, d.Agent.Name, d.Agent.Status, d.Agent.TabID)
		if *execute && d.Agent.TabID != "" {
			if err := herdr.TabClose(d.Agent.TabID); err != nil {
				fmt.Fprintf(os.Stderr, "herd stop: close %s: %v\n", d.Agent.TabID, err)
			}
		}
	}

	s := stop.Summarize(plan)
	mode := "DRY_RUN"
	if *execute {
		mode = "EXECUTED"
	}
	fmt.Printf("herd stop: %s workspace=%s close=%d preserved_active=%d protected=%d\n",
		mode, ws, s.Close, s.Preserved, s.Protected)
}
