package tui

import (
	"testing"

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
