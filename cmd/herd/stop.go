package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/stop"
)

// stopWinddownReason is stable across runs so a resumed stop reuses the
// posture a previous run already made durable.
const stopWinddownReason = "herd-stop"

// herdrFleet is the live boundary for stop. It can request a stop and close a
// tab; it has no way to remove a worktree, branch, or ref, which is what makes
// "force never deletes unique work" structural rather than a promise.
type herdrFleet struct{ workspace string }

func (f herdrFleet) Agents() ([]stop.Agent, error) {
	agents, err := herdr.AgentList()
	if err != nil {
		return nil, err
	}
	return fleetInWorkspace(agents, f.workspace), nil
}

func (f herdrFleet) RequestStop(paneID string) error {
	_, err := herdr.Send(paneID, stop.StopMessage, true, 30*time.Second)
	return err
}

func (f herdrFleet) CloseTab(tabID string) error { return herdr.TabClose(tabID) }

// fleetInWorkspace projects herdr rows onto the subset stop reasons about.
func fleetInWorkspace(agents []herdr.AgentEntry, workspace string) []stop.Agent {
	var fleet []stop.Agent
	for _, a := range agents {
		if a.Workspace != workspace {
			continue
		}
		name := a.Name
		if name == "" {
			name = "unknown"
		}
		fleet = append(fleet, stop.Agent{Name: name, Status: a.Status, PaneID: a.PaneID, TabID: a.TabID})
	}
	return fleet
}

// standingLanes reads the lanes the kick loop would otherwise revive.
func standingLanes() map[string]bool {
	standing := map[string]bool{}
	if cfg, err := config.LoadConfig(".herd/herd.yaml"); err == nil && cfg != nil {
		for _, lane := range cfg.Lanes {
			if lane.Standing {
				standing[lane.Name] = true
			}
		}
	}
	return standing
}

// runStop ports bin/herd-stop: bring the herd to rest without deleting
// worktrees or branches. Dry-run by default; --execute makes wind-down durable,
// signals workers, waits out a bounded drain, and closes only tabs whose agent
// was observed settled on a fresh read.
func runStop() {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	execute := fs.Bool("execute", false, "Perform the stop (default is a dry run)")
	forceWorking := fs.Bool("force-working", false, "Also close working/starting agents (explicit operator kill; still deletes no work)")
	includeCoordinator := fs.Bool("include-coordinator", false, "Also stop the coordinator (only after handoff)")
	workspace := fs.String("workspace", "", "Herdr workspace ID (default: resolved for this repo)")
	wait := fs.Duration("wait", 2*time.Minute, "Bounded wait for signaled agents to reach a checkpoint")
	fs.Parse(os.Args[2:])

	if *wait < 0 {
		fmt.Fprintln(os.Stderr, "herd stop: --wait must not be negative")
		os.Exit(2)
	}

	ws := *workspace
	if ws == "" {
		var err error
		ws, err = herdr.RequireWorkspace(".")
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd stop: workspace unresolved: %v\n", err)
			os.Exit(1)
		}
	}

	fleetBoundary := herdrFleet{workspace: ws}
	fleet, err := fleetBoundary.Agents()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd stop: agent list: %v\n", err)
		os.Exit(1)
	}

	plan := stop.Plan(fleet, stop.Options{
		ForceWorking:       *forceWorking,
		IncludeCoordinator: *includeCoordinator,
		StandingLanes:      standingLanes(),
	})

	// SIGINT/SIGTERM during the drain aborts the wait without closing anything
	// further; the durable posture keeps the fleet quiet until a restart
	// resumes the same shutdown.
	ctx, cancelSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignals()

	actor := winddownActor()
	res, runErr := stop.Exec{
		Fleet: fleetBoundary,
		EnterWinddown: func(ctx context.Context) error {
			state, err := enterWinddown(ctx, actor, stopWinddownReason)
			if err != nil {
				return err
			}
			fmt.Printf("WINDDOWN generation=%d actor=%s reason=%s\n", state.Generation, state.Actor, state.Reason)
			return nil
		},
		Out:          os.Stdout,
		Execute:      *execute,
		ForceWorking: *forceWorking,
		Wait:         *wait,
	}.Run(ctx, plan)

	// FAC-180: summarize performed outcomes. Unfenced TabClose fails closed;
	// never claim a clean EXECUTED when closes were blocked.
	if *execute {
		fmt.Printf("herd stop: EXECUTED workspace=%s closed=%d blocked_close=%d preserved=%d protected=%d signaled=%d unproven=%d\n",
			ws, res.Closed, res.BlockedClose, res.Preserved, res.Protected, res.Signaled, res.Unproven)
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "herd stop: %v\n", runErr)
			os.Exit(1)
		}
		if res.BlockedClose > 0 {
			os.Exit(1)
		}
		return
	}
	fmt.Printf("herd stop: DRY_RUN workspace=%s would_close=%d preserved=%d protected=%d\n",
		ws, res.Closed, res.Preserved, res.Protected)
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "herd stop: %v\n", runErr)
		os.Exit(1)
	}
}
