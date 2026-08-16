package herdr

import (
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
)

// LaunchObservation records the evidence used for the derived launch state.
type LaunchObservation struct {
	State       LaunchState
	Name        string
	PaneID      string
	TabID       string
	TerminalID  string
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
