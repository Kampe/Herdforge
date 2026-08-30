package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/pulse"
)

func TestFAC614RealPulseAndStatusCensusReportsPausedNotWorking(t *testing.T) {
	agents := []herdr.AgentEntry{
		{Name: "forge-orchestrator", Status: "working", PaneID: "wB:p45J", Workspace: "wB"},
		{Name: "forge-builder", Status: "working", PaneID: "wB:p46J", Workspace: "wB"},
		{Name: "forge-unknown", Status: "working", PaneID: "wB:p47J", Workspace: "wB"},
	}
	readPane := func(paneID string, _ int) (string, error) {
		switch paneID {
		case "wB:p45J":
			return "Goal paused (/goal resume)", nil
		case "wB:p46J":
			return "Pursuing goal (23m)", nil
		case "wB:p47J":
			return "", errors.New("pane unavailable")
		default:
			t.Fatalf("unexpected pane read %q", paneID)
			return "", nil
		}
	}

	classified := classifyPulseAndStatusAgents(agents, readPane)
	if got := classified[0].Status; got != string(pulse.StatusPaused) {
		t.Fatalf("paused lane status = %q, want %q", got, pulse.StatusPaused)
	}
	if got := classified[1].Status; got != "working" {
		t.Fatalf("healthy lane status = %q, want working", got)
	}
	if got := classified[2].Status; got != "working" {
		t.Fatalf("unreadable pane status = %q, want original working (unknown is not paused)", got)
	}

	fleet := herdr.ProjectLiveFleetStatus(classified, nil, "wB", 3)
	if fleet.Paused != 1 || fleet.Working != 2 {
		t.Fatalf("fleet paused=%d working=%d, want paused=1 working=2", fleet.Paused, fleet.Working)
	}
}

// This drives the production pulse observation path, not the classification
// helper. A non-Busy lane can still hold a provider context warning, so pane
// acquisition must not be gated on raw Busy classification.
func TestFAC614ReadPulseHerdrPreservesNonBusyContextWarning(t *testing.T) {
	paneReads := 0
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case command == "agent list":
			return `{"result":{"agents":[{"name":"forge-idle","agent_status":"idle","pane_id":"wK:p1","workspace_id":"wK"}],"type":"agents"}}`, nil
		case command == "workspace list":
			return `{"result":{"workspaces":[{"workspace_id":"wK","label":"herd","focused":true}]}}`, nil
		case strings.HasPrefix(command, "pane read "):
			paneReads++
			return `{"result":{"text":"provider: context window full"}}`, nil
		case strings.HasPrefix(command, "pane process-info "):
			return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
		case strings.HasPrefix(command, "agent explain "):
			return `{"state":"idle","visible_idle":true}`, nil
		default:
			t.Fatalf("unexpected herdr command %q", command)
			return "", nil
		}
	})
	t.Cleanup(restore)

	obs := readPulseHerdr(context.Background(), nil)
	if !obs.Known || obs.Error != "" {
		t.Fatalf("pulse Herdr observation = known %t error %q", obs.Known, obs.Error)
	}
	if len(obs.Agents) != 1 {
		t.Fatalf("pulse Herdr agents = %d, want 1", len(obs.Agents))
	}
	if paneReads != 1 {
		t.Fatalf("non-Busy pane reads = %d, want 1", paneReads)
	}
	if got := obs.Agents[0].Status; got != pulse.StatusHealthyIdle {
		t.Fatalf("non-Busy agent status = %q, want %q", got, pulse.StatusHealthyIdle)
	}
	if got := obs.Agents[0].ContextWarning; got != "provider: context window full" {
		t.Fatalf("non-Busy agent context warning = %q, want provider warning", got)
	}
}
