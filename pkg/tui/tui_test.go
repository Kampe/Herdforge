package tui

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestRenderDashboard(t *testing.T) {
	cfg := &config.Config{
		Project:      config.ProjectConfig{Name: "test-app", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{Type: "kaneo"},
	}

	tasks := []*provider.Task{
		{Ref: "FAC-1", Priority: provider.PriorityHigh, Status: "in-progress", Title: "Build TUI"},
	}

	output := RenderDashboard(cfg, tasks)

	if !strings.Contains(output, "HERDFORGE FLEET OPERATIONS TUI") {
		t.Errorf("expected dashboard title header")
	}
	if !strings.Contains(output, "FAC-1") || !strings.Contains(output, "Build TUI") {
		t.Errorf("expected task details rendered in dashboard")
	}
}
