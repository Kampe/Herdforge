package main

import (
	"errors"
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
