package herdr

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Port of bin/herd-send: submit text to a herdr agent pane and confirm the
// agent CONSUMED it (flipped to working — or done, for agents that finish the
// ask inside the poll window). A submit that lands in a dead pane looks
// identical to a delivered one, which is how packets silently vanished; the
// verify loop is the point of this command.

// StatusFromList extracts the agent_status for a target (name or pane id).
// Pure so the selftest can pin extraction without a live herdr.
func StatusFromList(agents []AgentEntry, target string) string {
	for _, a := range agents {
		if a.Name == target || a.PaneID == target {
			return a.Status
		}
	}
	return ""
}

func liveStatus(target string) (string, error) {
	agents, err := AgentList()
	if err != nil {
		return "", err
	}
	st := StatusFromList(agents, target)
	if st == "" {
		return "", fmt.Errorf("no agent '%s' found", target)
	}
	return st, nil
}

func liveStatusScoped(target string) (string, error) {
	return liveStatusScopedIn(target, "")
}

func liveStatusScopedIn(target, expected string) (string, error) {
	agent, err := requireAgentWorkspaceIn(target, expected)
	if err != nil {
		return "", err
	}
	return agent.Status, nil
}

// requireAgentWorkspace resolves the target against the live fleet before a
// prompt is sent. Agent names are globally scoped by Herdr, so a matching
// agent in another workspace must never be allowed to absorb this repo's
// delivery. Duplicate names spanning workspaces are rejected as ambiguous
// even when one candidate happens to be local.
func requireAgentWorkspace(target string) (AgentEntry, error) {
	return requireAgentWorkspaceIn(target, "")
}

func requireAgentWorkspaceIn(target, explicit string) (AgentEntry, error) {
	if explicit != "" {
		return requireAgentInWorkspace(target, explicit)
	}
	expected, err := registeredWorkspace(".")
	if err != nil {
		return AgentEntry{}, err
	}
	if expected == "" {
		expected = strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
	}
	if expected == "" {
		expected, err = RequireWorkspace(".")
		if err != nil {
			return AgentEntry{}, err
		}
	}
	return requireAgentInWorkspace(target, expected)
}

func requireAgentInWorkspace(target, expected string) (AgentEntry, error) {
	agents, err := AgentList()
	if err != nil {
		return AgentEntry{}, err
	}

	// A bare lane label has two plausible live names: the label itself and
	// Herdforge's canonical forge-<label> standing name. Resolve both before
	// applying workspace filtering. Otherwise an exact registration in one
	// repository can hide a forge-derived registration in another repository,
	// making a successful prompt look like proof of correct delivery.
	exactMatches := make([]AgentEntry, 0, 1)
	derivedMatches := make([]AgentEntry, 0, 1)
	for _, agent := range agents {
		if agent.Name == target || agent.PaneID == target {
			exactMatches = append(exactMatches, agent)
		}
	}
	if !strings.Contains(target, ":") && !strings.HasPrefix(target, "forge-") {
		derived := "forge-" + target
		for _, agent := range agents {
			if agent.Name == derived {
				derivedMatches = append(derivedMatches, agent)
			}
		}
	}
	if len(exactMatches) == 1 && len(derivedMatches) > 0 {
		return AgentEntry{}, fmt.Errorf("agent '%s' is ambiguous: exact live agent %q and forge-derived live agent %q; specify the registered name explicitly", target, exactMatches[0].Name, derivedMatches[0].Name)
	}
	if len(exactMatches) == 0 && len(derivedMatches) > 1 {
		return AgentEntry{}, fmt.Errorf("agent '%s' is ambiguous: forge-derived name matches %d live agents; specify the registered name explicitly", target, len(derivedMatches))
	}

	matches := exactMatches
	if len(matches) == 0 {
		matches = derivedMatches
	}
	if len(matches) == 0 {
		return AgentEntry{}, fmt.Errorf("no agent '%s' found", target)
	}
	for _, agent := range matches {
		// Older Herdr fixtures and live versions may omit workspace_id. There is
		// no mismatched workspace to reject in that case; retain the existing
		// target resolution behavior until Herdr supplies the identity.
		if agent.Workspace != "" && agent.Workspace != expected {
			return AgentEntry{}, fmt.Errorf("agent '%s' resolves to workspace %q, but repo is registered to workspace %q; refusing cross-workspace delivery", target, agent.Workspace, expected)
		}
	}
	if len(matches) > 1 {
		return AgentEntry{}, fmt.Errorf("agent '%s' resolves to multiple agents in workspace %q; refusing ambiguous delivery", target, expected)
	}
	return matches[0], nil
}

// SendKeys presses keys in an agent pane (used for the single Enter nudge:
// a stray suggestion/dialog can swallow a submit).
func SendKeys(target, keys string) error {
	out, err := runHerdr("agent", "send-keys", target, keys)
	if err != nil {
		return fmt.Errorf("herdr agent send-keys: %s: %w", out, err)
	}
	return nil
}

// Send submits text via `herdr agent prompt` and, when verify is set, polls
// up to timeout for the pane to report working/done. If it never flips, it
// presses Enter once and re-checks. Returns the final observed status; error
// when consumption was never confirmed so a caller can escalate. It does NOT
// answer trust/approval dialogs.
func Send(target, text string, verify bool, timeout time.Duration) (string, error) {
	return sendInWorkspace(target, text, verify, timeout, "")
}

// SendInWorkspace submits to a target already selected from an explicit
// workspace, as used by stop and other workspace-scoped operators.
func SendInWorkspace(target, text string, verify bool, timeout time.Duration, workspace string) (string, error) {
	return sendInWorkspace(target, text, verify, timeout, workspace)
}

// FormatSendResult renders the operator-facing delivery result.
func FormatSendResult(target, status string) string {
	return formatSendResult(target, "", status)
}

// FormatSendResultInWorkspace renders an explicitly authorized cross-workspace
// delivery with the workspace in the receipt. Keeping this separate from the
// ordinary formatter makes the authorization visible to operators and leaves
// existing same-workspace output stable.
func FormatSendResultInWorkspace(target, workspace, status string) string {
	return formatSendResult(target, workspace, status)
}

func formatSendResult(target, workspace, status string) string {
	qualifier := ""
	switch status {
	case "working", "done":
		qualifier = " (delivery confirmed)"
	case "submitted":
		qualifier = " (UNVERIFIED: --no-verify)"
	}
	route := target
	if strings.TrimSpace(workspace) != "" {
		route = fmt.Sprintf("%s [workspace=%s]", target, workspace)
	}
	return fmt.Sprintf("herd send: %s -> %s%s", route, status, qualifier)
}

func sendInWorkspace(target, text string, verify bool, timeout time.Duration, workspace string) (string, error) {
	resolved, err := requireAgentWorkspaceIn(target, workspace)
	if err != nil {
		return "", err
	}
	resolvedTarget := resolved.Name
	if resolvedTarget == "" {
		resolvedTarget = target
	}
	if _, err := AgentPrompt(resolvedTarget, text, false); err != nil {
		return "", err
	}
	// Herdr can return after writing TEXT while the pane composer is still
	// processing it.  Submit once immediately so a following status poll does
	// not observe text stranded in the composer (FAC-388).
	_ = SendKeys(resolvedTarget, "Enter")
	if !verify {
		return "submitted", nil
	}

	poll := 2 * time.Second
	deadline := time.Now().Add(timeout)
	nudged := true
	last := "unknown"
	for time.Now().Before(deadline) {
		st, err := liveStatusScopedIn(resolvedTarget, workspace)
		if err == nil {
			last = st
			if st == "working" || st == "done" {
				return st, nil
			}
		}
		if !nudged && time.Now().Add(poll).After(deadline.Add(-timeout/2)) {
			// Halfway through the window with no flip: one Enter nudge.
			_ = SendKeys(resolvedTarget, "Enter")
			nudged = true
		}
		time.Sleep(poll)
	}
	return last, fmt.Errorf("agent '%s' never confirmed consumption (last status %q)", resolvedTarget, last)
}
