package herdr

import (
	"fmt"
	"time"
)

// FAC-88: interrupt a spinning/stalled agent and refocus it. Does NOT kill
// the pane or tab — the refocus message IS the remediation, not a restart, so
// it is safe against standing agents. Sequence: escape (clear a stuck
// prompt / cancel), brief settle, then deliver the refocus message and verify
// the agent consumed it (flipped to working). This is what the forge calls
// when the watchdog (FAC-109/FAC-90) reports a stall, instead of a human
// closing the tab by hand.

// Shoot interrupts target (pane id or agent name), waits for the input to
// settle, then delivers refocus and verifies consumption. Returns the final
// observed status; error when the agent never resumed.
func Shoot(target, refocus string, verify bool, timeout time.Duration) (string, error) {
	// 1. Escape: cancel a stuck prompt / dismiss a suggestion box.
	if err := SendKeys(target, "escape"); err != nil {
		return "", fmt.Errorf("shoot: escape %s: %w", target, err)
	}
	// 2. Brief settle so the input box is ready for the prompt.
	time.Sleep(1 * time.Second)
	// 3. Refocus via the verified-delivery path (submit + confirm working).
	status, err := Send(target, refocus, verify, timeout)
	if err != nil {
		return status, fmt.Errorf("shoot: %s did not resume after refocus: %w", target, err)
	}
	return status, nil
}
