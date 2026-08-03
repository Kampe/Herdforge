package tui

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestRenderDashboard_NilCfg(t *testing.T) {
	tasks := []*provider.Task{
		{Ref: "FAC-1", Priority: provider.PriorityHigh, Status: "in-progress", Title: "Test"},
	}
	output := RenderDashboard(nil, tasks)

	if output == "" {
		t.Fatal("expected non-empty output even with nil cfg")
	}
}

func TestRenderDashboardWithFleetSurfacesUnknownAndNonBuilderSeats(t *testing.T) {
	output := RenderDashboardWithFleet(nil, nil, herdr.FleetStatus{Working: 1, Capacity: 1, Standing: 1, Preserved: 1, Recovering: 1, ControlSeats: 1, Unknown: 1})
	for _, want := range []string{"working=1", "capacity=1", "standing=1", "preserved=1", "recovering=1", "control=1", "unknown=1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("dashboard missing %q: %s", want, output)
		}
	}
}

func TestRenderDashboard_NilTasks(t *testing.T) {
	output := RenderDashboard(nil, nil)
	if output == "" {
		t.Fatal("expected non-empty output for nil tasks")
	}
}

func TestRenderDashboard_NoTasks(t *testing.T) {
	output := RenderDashboard(nil, []*provider.Task{})
	if output == "" {
		t.Fatal("expected non-empty output for empty tasks")
	}
}
