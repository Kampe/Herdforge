package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/standing"
)

func laneHasBoardWrite(lane *config.LaneDef) bool {
	if lane == nil {
		return false
	}
	for _, cap := range lane.Capabilities {
		if cap == config.CapabilityBoardWrite {
			return true
		}
	}
	return false
}

// authorizeStandingBoardWrite validates a shareable standalone worker broker
// and writes worker credentials into the confidential contain env.list before
// any tab exists. Read-only lanes and atomic-server boards skip the broker.
func authorizeStandingBoardWrite(lane *config.LaneDef, cwd string) error {
	if !laneHasBoardWrite(lane) {
		return nil
	}
	if strings.TrimSpace(os.Getenv("HERD_FENCE_ATOMIC_SERVER")) == "1" {
		return nil
	}
	claimDir := ""
	if dir, err := provider.CanonicalClaimDir(".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", "")); err == nil {
		claimDir = dir
	}
	cap, err := provider.ResolveShareableWorkerBroker(context.Background(), claimDir)
	if err != nil {
		return err
	}
	return provider.WriteShareableWorkerBrokerEnv(cwd, cap)
}

// standingCreateTab is the production standing tab seam: board-write broker
// authorization runs before Herdr tab creation.
func standingCreateTab(decision *router.LaunchDecision, lane *config.LaneDef, workspace, label, cwd string) (standing.Tab, error) {
	if decision == nil || lane == nil {
		return standing.Tab{}, errors.New("standing tab create requires prior AdmitRoute decision")
	}
	if err := authorizeStandingBoardWrite(lane, cwd); err != nil {
		return standing.Tab{}, err
	}
	req := launch.Request{
		Decision: decision,
		TaskRef:  lane.Name,
		Scope:    router.ScopeLane,
		Lane:     lane.Name,
	}
	_, tab, err := openWriteCapableTab(decision, req, lane, workspace, label, cwd)
	if err != nil {
		return standing.Tab{}, err
	}
	return standing.Tab{ID: tab.ID, Label: tab.Label, PaneID: tab.Pane.ID, Cwd: tab.Cwd}, nil
}
