package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

const nativePaneReadyTimeout = 8 * time.Second

// waitExactPaneBeforeStart polls the exact created pane until a readable
// shell and cwd exist. The harness must not start before this returns READY.
func waitExactPaneBeforeStart(tab *herdr.TabInfo, timeout time.Duration) (herdr.LaunchObservation, error) {
	if tab == nil || strings.TrimSpace(tab.Pane.ID) == "" {
		obs := herdr.LaunchObservation{State: herdr.LaunchFailed, Reason: "exact pane id is required"}
		return obs, fmt.Errorf("%w: %s", herdr.ErrLaunchFailed, obs.Reason)
	}
	if timeout <= 0 {
		timeout = nativePaneReadyTimeout
	}
	return herdr.WaitExactPaneReady(tab.ID, tab.Pane.ID, tab.Pane.TerminalID, timeout)
}

// compensateExactLaunchTab closes the exact launch-owned tab with
// generation-safe compare-and-close. Public TabClose is refused.
func compensateExactLaunchTab(workspace string, tab *herdr.TabInfo) error {
	if tab == nil || strings.TrimSpace(tab.ID) == "" {
		return fmt.Errorf("%w: exact tab id is required", herdr.ErrLaunchFailed)
	}
	nonce := "launch-fail-" + tab.ID
	if gen := strings.TrimSpace(tab.Generation); gen != "" {
		nonce += "-" + gen
	}
	paneIDs := []string(nil)
	if strings.TrimSpace(tab.Pane.ID) != "" {
		paneIDs = []string{tab.Pane.ID}
	}
	return herdr.CompensateExactTab(herdr.CloseRequest{
		WorkspaceID: workspace,
		TabID:       tab.ID,
		Generation:  tab.Generation,
		PaneIDs:     paneIDs,
		Nonce:       nonce,
	})
}
