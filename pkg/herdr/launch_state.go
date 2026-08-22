package herdr

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// LaunchState is Herdforge's local launch result. Herdr 0.8 does not expose a
// terminal READY/REFUSED/DIED state, so we derive one from the exact agent row
// and pane process inventory.
type LaunchState string

const (
	LaunchReady   LaunchState = "READY"
	LaunchRefused LaunchState = "REFUSED"
	LaunchDied    LaunchState = "DIED"
	// LaunchFailed is the terminal pre-start outcome for an exact pane that
	// never became a readable shell (unknown, dead, or authentication).
	LaunchFailed LaunchState = "LAUNCH_FAILED"
)

// ErrLaunchFailed is the durable sentinel for a bounded native launch refusal.
// One reason string rides on the wrapped error; callers must not retry the
// same pane after this error.
var ErrLaunchFailed = errors.New("LAUNCH_FAILED")

// IsLaunchFailed reports whether err is a terminal pre-start launch failure.
func IsLaunchFailed(err error) bool {
	return errors.Is(err, ErrLaunchFailed)
}

func launchFailed(obs LaunchObservation) error {
	reason := strings.TrimSpace(obs.Reason)
	if reason == "" {
		reason = "exact pane was not launch-ready"
	}
	return fmt.Errorf("%w: %s", ErrLaunchFailed, reason)
}

// LaunchObservation records the evidence used for the derived launch state.
type LaunchObservation struct {
	State       LaunchState
	Name        string
	PaneID      string
	TabID       string
	TerminalID  string
	Cwd         string
	AgentStatus string
	Reason      string
}

// VerifyAgentLaunch polls the exact Herdr identity after agent start. A tab
// ID alone only proves that a slot was created; the agent list and foreground
// process inventory prove that the requested agent still owns that slot.
func VerifyAgentLaunch(name, paneID string, timeout time.Duration) (LaunchObservation, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(paneID) == "" {
		return LaunchObservation{State: LaunchRefused, Name: name, PaneID: paneID, Reason: "launch identity is incomplete"}, fmt.Errorf("launch identity requires agent name and pane id")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	last := LaunchObservation{State: LaunchDied, Name: name, PaneID: paneID, Reason: "agent not present in herdr agent list"}
	for time.Now().Before(deadline) {
		agents, err := AgentList()
		if err != nil {
			last.Reason = fmt.Sprintf("agent list unavailable: %v", err)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		matches := make([]AgentEntry, 0, 1)
		for _, agent := range agents {
			if agent.Name == name && agent.PaneID == paneID {
				matches = append(matches, agent)
			}
		}
		if len(matches) > 1 {
			last.State = LaunchRefused
			last.Reason = fmt.Sprintf("agent identity is ambiguous: %d matching rows", len(matches))
			return last, fmt.Errorf("launch %s/%s is ambiguous", name, paneID)
		}
		if len(matches) == 1 {
			agent := matches[0]
			last.TabID, last.TerminalID, last.AgentStatus = agent.TabID, agent.TerminalID, agent.Status
			if agent.Status == "done" || agent.Status == "unknown" || strings.TrimSpace(agent.Status) == "" {
				// Herdr's unknown state means that it cannot currently classify
				// the pane. It is not proof of death; keep polling until the
				// bounded launch window expires.
				last.State = LaunchDied
				last.Reason = fmt.Sprintf("agent listed with non-ready status %q", agent.Status)
			} else {
				panes, paneErr := PaneList()
				if paneErr != nil {
					last.State = LaunchDied
					last.Reason = fmt.Sprintf("pane inventory unavailable: %v", paneErr)
					time.Sleep(250 * time.Millisecond)
					continue
				}
				paneFound := false
				incarnationMismatch := false
				for _, pane := range panes {
					if pane.PaneID != paneID {
						continue
					}
					paneFound = true
					if agent.TerminalID != "" && pane.TerminalID != "" && agent.TerminalID != pane.TerminalID {
						incarnationMismatch = true
						last.State = LaunchDied
						last.Reason = fmt.Sprintf("pane incarnation changed: agent=%q pane=%q", agent.TerminalID, pane.TerminalID)
						break
					}
				}
				if incarnationMismatch || !paneFound {
					if !paneFound {
						last.Reason = "agent row present but pane inventory has no matching pane"
					}
					time.Sleep(250 * time.Millisecond)
					continue
				}
				body, _ := PaneRead(paneID, 20)
				// FAC-576: the workspace trust dialog is resolvable by us and
				// must be answered BEFORE the auth check, which cannot tell it
				// apart from a credential screen. A fleet agent launches into a
				// directory Herdforge just created, so this fires on nearly
				// every launch; refusing instead meant no interactive agent
				// could start in a fresh worktree.
				if WorkspaceTrustPrompt(body) {
					if ConfirmWorkspaceTrust(paneID) {
						body, _ = PaneRead(paneID, 20)
					}
				}
				if LoginOrAuthScreen(agent.TerminalTitle, body) {
					last.State = LaunchRefused
					last.Reason = "pane is at a login or authentication screen"
					return last, fmt.Errorf("launch refused: %s", last.Reason)
				}
				processes, processErr := PaneProcessInfo(paneID)
				if processErr == nil && hasForegroundAgentProcess(processes) {
					last.State = LaunchReady
					last.Reason = "matching agent and foreground process present"
					return last, nil
				}
				last.State = LaunchDied
				last.Reason = "agent row present but no foreground agent process"
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return last, fmt.Errorf("launch %s/%s did not reach READY: %s", name, paneID, last.Reason)
}

func verifyAgentLaunch(name, paneID string, timeout time.Duration) (LaunchObservation, error) {
	return VerifyAgentLaunch(name, paneID, timeout)
}

const defaultPaneReadyTimeout = 8 * time.Second

// WaitExactPaneReady polls the exact Herdr pane (via pane list, not agent
// list) until a readable shell and cwd exist. It must run after tab create
// and before harness start. An unknown pane is retried until the bound
// expires; login/auth screens and dead panes are immediate LAUNCH_FAILED.
func WaitExactPaneReady(tabID, paneID, terminalID string, timeout time.Duration) (LaunchObservation, error) {
	last := LaunchObservation{State: LaunchFailed, TabID: tabID, PaneID: paneID, TerminalID: terminalID, Reason: "exact pane id is required"}
	if strings.TrimSpace(paneID) == "" {
		return last, launchFailed(last)
	}
	if timeout <= 0 {
		timeout = defaultPaneReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	last.Reason = "unknown pane"
	for {
		panes, err := PaneList()
		if err != nil {
			last.Reason = fmt.Sprintf("pane inventory unavailable: %v", err)
			if !sleepUntilReady(deadline) {
				return last, launchFailed(last)
			}
			continue
		}
		var found *PaneEntry
		for i := range panes {
			pane := panes[i]
			if pane.PaneID != paneID {
				continue
			}
			if tabID != "" && pane.TabID != "" && pane.TabID != tabID {
				continue
			}
			if terminalID != "" && pane.TerminalID != "" && pane.TerminalID != terminalID {
				last.State = LaunchFailed
				last.TabID, last.TerminalID = pane.TabID, pane.TerminalID
				last.Reason = fmt.Sprintf("pane incarnation changed: want %q got %q", terminalID, pane.TerminalID)
				return last, launchFailed(last)
			}
			copy := pane
			found = &copy
			break
		}
		if found == nil {
			last.State = LaunchFailed
			last.Reason = "unknown pane"
			if !sleepUntilReady(deadline) {
				return last, launchFailed(last)
			}
			continue
		}
		last.TabID, last.TerminalID = found.TabID, found.TerminalID
		body, _ := PaneRead(paneID, 20)
		// FAC-576: answer the resolvable trust dialog before the auth check, as
		// in the loop above. Both sites needed it; patching one would have left
		// whichever path a given launch takes still refusing.
		if WorkspaceTrustPrompt(body) {
			if ConfirmWorkspaceTrust(paneID) {
				body, _ = PaneRead(paneID, 20)
			}
		}
		if LoginOrAuthScreen(found.TerminalTitle, body) {
			last.State = LaunchFailed
			last.Reason = "pane is at a login or authentication screen"
			return last, launchFailed(last)
		}
		cwd := strings.TrimSpace(found.ForegroundCwd)
		if cwd == "" {
			cwd = strings.TrimSpace(found.Cwd)
		}
		processes, processErr := PaneProcessInfo(paneID)
		if processErr != nil {
			if errors.Is(processErr, ErrPaneNotFound) {
				last.State = LaunchFailed
				last.Reason = "dead pane: exact pane disappeared"
				if !sleepUntilReady(deadline) {
					return last, launchFailed(last)
				}
				continue
			}
			last.Reason = fmt.Sprintf("pane process inventory unavailable: %v", processErr)
		} else if !hasReadableShell(processes) {
			last.State = LaunchFailed
			last.Reason = "dead pane: no readable shell"
		} else if cwd == "" {
			last.State = LaunchFailed
			last.Reason = "pane cwd is not readable"
		} else {
			last.State = LaunchReady
			last.Cwd = cwd
			last.Reason = "exact pane has readable shell and cwd"
			return last, nil
		}
		if !sleepUntilReady(deadline) {
			if last.State == "" {
				last.State = LaunchFailed
			}
			return last, launchFailed(last)
		}
	}
}

func sleepUntilReady(deadline time.Time) bool {
	if !time.Now().Before(deadline) {
		return false
	}
	time.Sleep(250 * time.Millisecond)
	return true
}

// CompensateExactTab closes the exact launch-owned tab with generation-safe
// compare-and-close. Public TabClose stays refused. When Herdr has not yet
// published a generation for a tab we just created, the exact tab id is
// closed and absence is read back.
func CompensateExactTab(req CloseRequest) error {
	if strings.TrimSpace(req.TabID) == "" {
		return &CloseUnavailableError{Reason: "exact tab id is required"}
	}
	if strings.TrimSpace(req.Nonce) == "" {
		req.Nonce = "launch-fail-" + req.TabID
	}
	if req.Generation == "" && req.WorkspaceID != "" {
		if tabs, err := TabList(req.WorkspaceID); err == nil {
			for _, tab := range tabs {
				if tab.TabID == req.TabID && strings.TrimSpace(tab.Generation) != "" {
					req.Generation = strings.TrimSpace(tab.Generation)
					break
				}
			}
		}
	}
	if req.Generation != "" {
		if strings.TrimSpace(req.WorkspaceID) == "" {
			return &CloseUnavailableError{TabID: req.TabID, Reason: "workspace_id is required"}
		}
		return TabCloseCAS(req)
	}
	closeErr := tabCloseRaw(req.TabID)
	if req.WorkspaceID == "" {
		return closeErr
	}
	tabs, err := TabList(req.WorkspaceID)
	if err != nil {
		if closeErr != nil {
			return fmt.Errorf("compensate exact tab %s: %w (absence readback: %v)", req.TabID, closeErr, err)
		}
		return fmt.Errorf("compensate exact tab %s: absence readback: %w", req.TabID, err)
	}
	for _, tab := range tabs {
		if tab.TabID == req.TabID {
			if closeErr != nil {
				return closeErr
			}
			return fmt.Errorf("compensate exact tab %s: still present after close", req.TabID)
		}
	}
	return nil
}

func hasReadableShell(processes []PaneProcess) bool {
	for _, process := range processes {
		name := strings.ToLower(strings.TrimSpace(process.Name))
		switch name {
		case "zsh", "bash", "sh", "fish", "-zsh", "-bash", "-sh", "-fish":
			return true
		}
	}
	return false
}

func hasForegroundAgentProcess(processes []PaneProcess) bool {
	for _, process := range processes {
		name := strings.ToLower(strings.TrimSpace(process.Name))
		if name == "" || name == "zsh" || name == "bash" || name == "sh" || name == "fish" || name == "-zsh" || name == "-bash" {
			continue
		}
		return true
	}
	return false
}
