package herdr

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Port of bin/herd-send: submit text to a herdr agent pane and confirm the
// agent CONSUMED it. Status is only one part of the proof: verified delivery
// also requires the submitted task text to appear in pane readback. A submit
// that lands in a dead pane, or remains staged as pasted text, must not look
// successful.

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
// up to timeout for prompt-correlated pane evidence. The agent must report a
// consumption transition relative to the pre-send baseline, and the exact
// task text must appear in pane readback. If it never proves consumption, it
// returns an error so a caller can escalate. It does NOT answer trust/approval
// dialogs.
func Send(target, text string, verify bool, timeout time.Duration) (string, error) {
	return sendInWorkspace(target, text, verify, timeout, "")
}

// SendStatus is the status-only delivery path for operator control nudges
// (stop, spin, and feedback). Those messages are not task assignments and do
// not have task-specific pane text to prove. Assignment delivery must use
// Send, which requires both the baseline-correlated status transition and
// pane readback of the submitted task text.
func SendStatus(target, text string, verify bool, timeout time.Duration) (string, error) {
	return sendStatusInWorkspace(target, text, verify, timeout, "")
}

// SendInWorkspace submits to a target already selected from an explicit
// workspace, as used by stop and other workspace-scoped operators.
func SendInWorkspace(target, text string, verify bool, timeout time.Duration, workspace string) (string, error) {
	return sendInWorkspace(target, text, verify, timeout, workspace)
}

// SendStatusInWorkspace is the workspace-scoped control-nudge variant of
// SendInWorkspace.
func SendStatusInWorkspace(target, text string, verify bool, timeout time.Duration, workspace string) (string, error) {
	return sendStatusInWorkspace(target, text, verify, timeout, workspace)
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
		qualifier = " (consumption confirmed; task text observed in pane)"
	case "submitted":
		qualifier = " (UNVERIFIED: --no-verify)"
	case "queued":
		qualifier = " (queued but not consumed; explicit retry or defer required)"
	}
	route := target
	if strings.TrimSpace(workspace) != "" {
		route = fmt.Sprintf("%s [workspace=%s]", target, workspace)
	}
	return fmt.Sprintf("herd send: %s -> %s%s", route, status, qualifier)
}

// taskTextObserved reports whether pane readback contains the submitted task
// or one of its executable command lines. Long packets are often wrapped or
// clipped from the recent pane tail, so requiring the entire packet would
// reject a genuinely consumed assignment. The baseline comparison remains
// mandatory at the call site, which prevents old pane text from proving a new
// delivery.
func taskTextObserved(text, pane string) bool {
	if strings.TrimSpace(text) == "" || strings.Contains(pane, text) {
		return strings.TrimSpace(text) == "" || strings.Contains(pane, text)
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimLeft(line, "`>*-0123456789. )\t")
		if len(line) < 4 || !isExecutableTaskLine(line) {
			continue
		}
		if strings.Contains(pane, line) {
			return true
		}
	}
	return false
}

func isExecutableTaskLine(line string) bool {
	for _, prefix := range []string{"go ", "git ", "make ", "herd ", "cd ", "./", "bash ", "zsh "} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
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
	baselinePane := ""
	if verify {
		baselinePane, err = PaneRead(resolved.PaneID, 120)
		if err != nil {
			return "", fmt.Errorf("agent '%s' pre-send pane readback failed: %w", resolvedTarget, err)
		}
	}
	// A working standing lane may be inside its durable /goal turn. An
	// addressed assignment must own the next turn, not sit behind that goal;
	// Escape pauses the standing turn before the assignment is written. If the
	// preemption cannot be delivered, refuse loudly instead of reporting a
	// successful send for text that can only remain queued.
	if strings.EqualFold(strings.TrimSpace(resolved.Status), "working") {
		if err := SendKeys(resolvedTarget, "Escape"); err != nil {
			return "deferred", fmt.Errorf("agent '%s' assignment explicitly deferred: cannot preempt standing goal: %w", resolvedTarget, err)
		}
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
	lastPane := ""
	staged := false
	for time.Now().Before(deadline) {
		st, err := liveStatusScopedIn(resolvedTarget, workspace)
		if err == nil {
			last = st
			pane, paneErr := PaneRead(resolved.PaneID, 120)
			if paneErr == nil {
				lastPane = pane
				staged = strings.Contains(strings.ToLower(pane), "pasted text") && !taskTextObserved(text, pane)
			}
			if (st == "working" || st == "done") && paneErr == nil &&
				!taskTextObserved(text, baselinePane) && taskTextObserved(text, pane) {
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
	if staged || strings.Contains(strings.ToLower(lastPane), "pasted text") {
		return "queued", fmt.Errorf("agent '%s' queued-but-not-consumed: task text remained staged/unsubmitted in the pane (last status %q)", resolvedTarget, last)
	}
	return "queued", fmt.Errorf("agent '%s' queued-but-not-consumed: task-specific consumption was not observed in the pane (last status %q)", resolvedTarget, last)
}

func sendStatusInWorkspace(target, text string, verify bool, timeout time.Duration, workspace string) (string, error) {
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
	_ = SendKeys(resolvedTarget, "Enter")
	if !verify {
		return "submitted", nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	last := "unknown"
	for time.Now().Before(deadline) {
		st, statusErr := liveStatusScopedIn(resolvedTarget, workspace)
		if statusErr == nil {
			last = st
			if st == "working" || st == "done" {
				return st, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return last, fmt.Errorf("agent '%s' never confirmed consumption (last status %q)", resolvedTarget, last)
}
