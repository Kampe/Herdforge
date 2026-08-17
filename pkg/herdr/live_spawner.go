package herdr

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/security"
)

// LiveSpawner implements security.ProcessSpawner against the herdr CLI.
// Production starts must use AgentStartWithDecision via LaunchRequest on the
// spawn request (main refuses raw AgentStart). When no request is bound,
// StartAgent fails closed rather than inventing a Decision.
type LiveSpawner struct {
	// Request, when non-nil, is the validated launch.Request for StartAgent.
	// Dispatch binds it before security.LaunchAgent.
	Request *launch.Request
}

// CreateTab implements security.ProcessSpawner.
func (s LiveSpawner) CreateTab(workspace, label, cwd string, env []string, noFocus bool) (tabID, paneID string, err error) {
	tab, err := TabCreateForTaskEnv(workspace, label, cwd, env, noFocus)
	if err != nil {
		return "", "", err
	}
	return tab.ID, tab.Pane.ID, nil
}

// StartAgent implements security.ProcessSpawner.
// Prefer AgentStartWithDecision when Request is bound (main's production path).
// agentArgs are ignored when Request.Decision.Argv already carries the model.
func (s LiveSpawner) StartAgent(name, kind, paneID string, agentArgs []string) error {
	if s.Request != nil {
		req := *s.Request
		req.Name = name
		req.PaneID = paneID
		if req.Decision != nil && kind != "" && req.Decision.Harness == "" {
			// Do not invent harness; Decision must already be complete.
		}
		return AgentStartWithDecision(name, kind, paneID, req)
	}
	return agentStartProcess(name, kind, paneID, agentArgs...)
}

// CloseTab implements security.ProcessSpawner.
func (LiveSpawner) CloseTab(tabID string) error {
	// Live launches must never fall back to an unfenced close. A raw close can
	// tear down a recycled tab while its broker/CA is still referenced by the
	// child, leaving the harness with a stale SSL_CERT_FILE and an auth loop.
	return hardCloseTab(tabID, "")
}

// LiveResolver implements security.LiveAgentResolver against herdr.
type LiveResolver struct{}

// Lookup implements security.LiveAgentResolver.
func (LiveResolver) Lookup(name string) (*security.LiveAgentIdentity, error) {
	a, err := LookupAgent(name)
	if err != nil {
		return nil, err
	}
	sid := strings.TrimSpace(a.Session.Value)
	// Some kinds expose no agent_session until a model turn; terminal_id is a
	// process binding but is NOT a model session — FAC-133 RefuseProvisional
	// rejects herdr-term: prefixes for control binding.
	return &security.LiveAgentIdentity{
		Name:           a.Name,
		Kind:           a.Kind,
		TabID:          a.TabID,
		PaneID:         a.PaneID,
		Status:         a.Status,
		AgentSessionID: sid,
		TerminalID:     "", // not exposed as model session
	}, nil
}

// CloseTab implements security.LiveAgentResolver.
func (LiveResolver) CloseTab(tabID string) error {
	return hardCloseTab(tabID, "")
}

// AgentRead returns recent terminal output for a live agent (login/auth detection).
func AgentRead(target string, lines int) (string, error) {
	if lines <= 0 {
		lines = 40
	}
	args := []string{"agent", "read", target, "--source", "recent", "--lines", fmt.Sprintf("%d", lines)}
	out, err := runHerdr(args...)
	if err != nil {
		return "", fmt.Errorf("herdr agent read: %s: %w", out, err)
	}
	return out, nil
}
