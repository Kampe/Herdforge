package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type DashboardState struct {
	ProjectName  string
	ProviderType string
	ActiveTasks  []*provider.Task
	LastUpdated  time.Time
	Fleet        herdr.FleetStatus
}

func RenderDashboard(cfg *config.Config, tasks []*provider.Task) string {
	// No live reconciliation snapshot was supplied; surface UNKNOWN instead
	// of rendering a false idle/capacity-success projection.
	return RenderDashboardWithFleet(cfg, tasks, herdr.FleetStatus{Unknown: 1})
}

func RenderDashboardWithFleet(cfg *config.Config, tasks []*provider.Task, fleet herdr.FleetStatus) string {
	var sb strings.Builder

	sb.WriteString("========================================================================\n")
	sb.WriteString("                   HERDFORGE FLEET OPERATIONS TUI                       \n")
	sb.WriteString("========================================================================\n")
	if cfg != nil {
		sb.WriteString(fmt.Sprintf(" Project Name : %s\n", cfg.Project.Name))
		sb.WriteString(fmt.Sprintf(" Default Branch: %s\n", cfg.Project.DefaultBranch))
		sb.WriteString(fmt.Sprintf(" Task Engine   : %s\n", cfg.TaskProvider.Type))
		sb.WriteString(fmt.Sprintf(" Configured Lanes: %d\n", len(cfg.Lanes)))
	}
	sb.WriteString(fmt.Sprintf(" Fleet Lanes   : working=%d queued=%d capacity=%d standing=%d preserved=%d recovering=%d control=%d unknown=%d\n",
		fleet.Working, fleet.Queued, fleet.Capacity, fleet.Standing, fleet.Preserved, fleet.Recovering, fleet.ControlSeats, fleet.Unknown))
	sb.WriteString("------------------------------------------------------------------------\n")
	sb.WriteString(" ACTIVE TASK QUEUE                                                      \n")
	sb.WriteString("------------------------------------------------------------------------\n")

	if len(tasks) == 0 {
		sb.WriteString(" (No pending tasks in queue)\n")
	} else {
		sb.WriteString(fmt.Sprintf(" %-8s | %-10s | %-12s | %s\n", "REF", "PRIORITY", "STATUS", "TITLE"))
		sb.WriteString(" ---------+------------+--------------+---------------------------------\n")
		for _, t := range tasks {
			sb.WriteString(fmt.Sprintf(" %-8s | %-10s | %-12s | %s\n", t.Ref, t.Priority, t.Status, t.Title))
		}
	}

	sb.WriteString("========================================================================\n")
	sb.WriteString(" Controls: [Q]uit | [R]efresh | [P]ause Fleet                           \n")
	sb.WriteString("========================================================================\n")

	return sb.String()
}
