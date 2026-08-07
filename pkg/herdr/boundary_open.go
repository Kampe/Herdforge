package herdr

import (
	"fmt"

	"github.com/Kampe/Herdforge/pkg/launch"
)

// TabOpener adapts TabCreateForTask to launch.TabOpener so write-capable
// callers can open tabs only through launch.Open (FAC-139).
type TabOpener struct{}

// OpenTab creates a task tab with required cwd and returns tab/pane ids.
func (TabOpener) OpenTab(workspace, label, cwd string, noFocus bool, env ...string) (tabID, paneID string, err error) {
	tab, err := TabCreateForTask(workspace, label, cwd, noFocus, env...)
	if err != nil {
		return "", "", err
	}
	if tab == nil || tab.ID == "" || tab.Pane.ID == "" {
		return "", "", fmt.Errorf("herdr tab create returned incomplete identity")
	}
	return tab.ID, tab.Pane.ID, nil
}

// OpenWriteCapableTab admits the launch boundary then creates the tab.
// Callers must pass a current tool-probe PASS receipt for write-capable roles.
func OpenWriteCapableTab(spec launch.BoundarySpec) (*launch.Plan, *TabInfo, error) {
	plan, tabID, paneID, err := launch.Open(TabOpener{}, spec)
	if err != nil {
		return nil, nil, err
	}
	return plan, &TabInfo{ID: tabID, Label: plan.Label, Cwd: plan.Cwd, Pane: PaneInfo{ID: paneID, TabID: tabID}}, nil
}
