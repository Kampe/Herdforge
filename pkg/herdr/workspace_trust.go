package herdr

import (
	"strings"
	"time"
)

// FAC-576: Claude Code asks whether to trust a directory the first time it runs
// there. A fleet reviewer launches into a review surface Herdforge just created,
// so the dialog appears on essentially every exact-SHA review — and the
// launch-state check reads "trust this folder" as an auth screen and refuses the
// launch. The reported symptom was "pane is at a login or authentication screen"
// on a host where claude was fully logged in.
//
// Neither --permission-mode bypassPermissions nor --dangerously-skip-permissions
// suppresses it; both were tried live and the dialog still appeared. It is
// skipped only in non-interactive mode, which a reviewer pane is not.
//
// So it is answered rather than avoided. That is defensible precisely here and
// nowhere else: the directory is one Herdforge created itself, inside its own
// repository, moments earlier. Confirming trust for a directory we authored is
// not a security decision taken blindly — and the alternative is that no
// interactive fleet agent can ever start in a fresh worktree.

// workspaceTrustNeedles identify Claude Code's workspace trust dialog
// specifically.
//
// All must be present. Requiring the numbered options as well as the phrase is
// what keeps this from matching a genuine credential screen that merely mentions
// trust: a real login prompt does not offer "1. Yes, I trust this folder".
var workspaceTrustNeedles = []string{
	"trust this folder",
	"no, exit",
}

// WorkspaceTrustPrompt reports whether a pane is at the workspace trust dialog,
// as opposed to a login or credential screen.
//
// The distinction matters because the two demand opposite responses: a trust
// dialog is resolvable by this process, and a login screen is not resolvable by
// anyone but the operator.
func WorkspaceTrustPrompt(body string) bool {
	s := strings.ToLower(body)
	for _, n := range workspaceTrustNeedles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}

// trustConfirmAttempts bounds how many times trust is confirmed for one launch.
// One Enter should clear it; more than a couple means the dialog is not what we
// think it is, and repeating forever would hide that.
const trustConfirmAttempts = 3

// ConfirmWorkspaceTrust answers the workspace trust dialog if the pane is at it.
//
// Returns true when a dialog was found AND cleared. A pane that is not at the
// dialog returns false with no keystrokes sent — this must never press Enter
// speculatively, because an Enter into a live session submits whatever is in the
// composer.
func ConfirmWorkspaceTrust(paneID string) bool {
	if strings.TrimSpace(paneID) == "" {
		return false
	}
	for attempt := 0; attempt < trustConfirmAttempts; attempt++ {
		body, err := PaneRead(paneID, 40)
		if err != nil {
			return false
		}
		if !WorkspaceTrustPrompt(body) {
			// Cleared (or never present). Only report success if we actually
			// answered something on a previous pass.
			return attempt > 0
		}
		// Option 1 ("Yes, I trust this folder") is preselected, so Enter
		// confirms. Send it to the pane rather than the agent: the dialog is
		// the harness's own TUI, not a prompt turn.
		if err := PaneSendKeys(paneID, "Enter"); err != nil {
			return false
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return false
}
