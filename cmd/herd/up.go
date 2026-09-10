package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// upRuntime is the external routing/terminal boundary. The command retains
// config, posture, decision validation and receipt writing in production code.
type upRuntime interface {
	Available() bool
	Route(*config.LaneDef) (*router.LaunchDecision, error)
	Open(*router.LaunchDecision, launch.Request, *config.LaneDef, string, string, string) (*herdr.TabInfo, error)
	Ready(*herdr.TabInfo) error
	Start(string, string, string, string, launch.Request) error
	Close(string, *herdr.TabInfo) error
}
type liveUpRuntime struct{}

func (liveUpRuntime) Available() bool { return herdr.IsAvailable() }
func (liveUpRuntime) Route(lane *config.LaneDef) (*router.LaunchDecision, error) {
	return laneLaunchDecision(context.Background(), lane, nil)
}
func (liveUpRuntime) Open(d *router.LaunchDecision, r launch.Request, l *config.LaneDef, w, n, c string) (*herdr.TabInfo, error) {
	_, t, e := openWriteCapableTab(d, r, l, w, n, c)
	return t, e
}
func (liveUpRuntime) Ready(t *herdr.TabInfo) error {
	_, e := waitExactPaneBeforeStart(t, nativePaneReadyTimeout)
	return e
}
func (liveUpRuntime) Start(t, n, h, p string, r launch.Request) error {
	return herdr.StartPreparedAgent(t, n, h, p, r)
}
func (liveUpRuntime) Close(w string, t *herdr.TabInfo) error { return compensateExactLaunchTab(w, t) }

// runUpCommand is the shipped `up` command, with only host operations injected
// so its provenance contract can be exercised without starting real agents.
func runUpCommand(laneName string, runtime upRuntime, out io.Writer) error {
	if err := requireFleetAdmission(context.Background()); err != nil {
		return err
	}
	cfg, err := config.LoadConfig(config.DefaultConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	var lane *config.LaneDef
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Name == laneName {
			lane = &cfg.Lanes[i]
			break
		}
	}
	if lane == nil {
		return fmt.Errorf("lane %q not found in config", laneName)
	}
	if !runtime.Available() {
		return fmt.Errorf("herdr CLI not found")
	}
	workspace, err := resolveBuilderWorkspace(".")
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	repository := repositoryIdentityForLaunch(cfg)
	if repository == "" {
		return fmt.Errorf("repository identity unavailable")
	}
	if lane.Worktree == "" {
		return fmt.Errorf("isolated worktree required")
	}
	cwd := filepath.Join(".", lane.Worktree)
	name := standing.AgentNameForRepository(lane.Name, repository)
	// FAC-767: hook policy discovery resolves .herd/harness-hooks.json
	// relative to the process's own cwd unless scoped here. Without this,
	// `up` validates the lane's launch against the coordinator's own
	// (canonical) pin file instead of the exact target worktree's, so a
	// stale canonical pin can reject a launch even when the target
	// worktree already has a freshly refreshed one. `herd review` has
	// scoped this the same way since FAC-onboarding; `up` never did.
	restoreHooks := useHarnessHooksFromWorktree(cwd)
	defer restoreHooks()
	var tab *herdr.TabInfo
	decision, err := launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, lane, true, runtime.Route, func(d *router.LaunchDecision) error {
		var e error
		tab, e = runtime.Open(d, launch.Request{Decision: d, TaskRef: lane.Name, Scope: router.ScopeLane, Repository: repository, Lane: lane.Name}, lane, workspace, name, cwd)
		return e
	})
	if err != nil {
		return fmt.Errorf("launch route rejected: %w", err)
	}
	if err := validateDecisionBeforeSideEffect(decision, lane.Name); err != nil {
		return err
	}
	if err := runtime.Ready(tab); err != nil {
		if closeErr := runtime.Close(workspace, tab); closeErr != nil {
			return fmt.Errorf("pane readiness: %w; compensation failed: %v", err, closeErr)
		}
		return err
	}
	if err := startStandingAgent(standing.Tab{ID: tab.ID, PaneID: tab.Pane.ID, Cwd: cwd}, name, standing.Route{Decision: decision}, lane, repository, runtime.Start); err != nil {
		return fmt.Errorf("failed to start lane: %w", err)
	}
	_, err = fmt.Fprintf(out, "Lane '%s' started: tab=%s pane=%s agent=%s\n", lane.Name, tab.ID, tab.Pane.ID, name)
	return err
}
