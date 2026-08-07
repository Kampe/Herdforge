package dispatch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/security"
)

// launcherSpawner adapts HerdrLauncher to security.ProcessSpawner and
// LiveAgentResolver so LaunchAgent can bind a live agent identity.
//
// StartAgent uses the bound launch.Request (main refuses raw AgentStart).
// Lookup prefers liveLookup over the post-start cache. Session id is optional
// provenance (grok never reports one); identity is name+tab+pane.
type launcherSpawner struct {
	h          HerdrLauncher
	request    launch.Request
	lastTab    string
	lastPane   string
	lastName   string
	lastKind   string
	started    map[string]*security.LiveAgentIdentity
	liveLookup func(name string) (*security.LiveAgentIdentity, error)
}

func newLauncherSpawner(h HerdrLauncher, req launch.Request) *launcherSpawner {
	s := &launcherSpawner{
		h:       h,
		request: req,
		started: map[string]*security.LiveAgentIdentity{},
	}
	s.liveLookup = func(name string) (*security.LiveAgentIdentity, error) {
		return (herdr.LiveResolver{}).Lookup(name)
	}
	return s
}

func (s *launcherSpawner) CreateTab(workspace, label, cwd string, env []string, noFocus bool) (string, string, error) {
	tab, err := s.h.TabCreateWithEnv(workspace, label, cwd, env, noFocus)
	if err != nil {
		return "", "", err
	}
	s.lastTab = tab.ID
	s.lastPane = tab.Pane.ID
	return tab.ID, tab.Pane.ID, nil
}

func (s *launcherSpawner) StartAgent(name, kind, paneID string, agentArgs []string) error {
	req := s.request
	req.Name = name
	req.PaneID = paneID
	if err := s.h.AgentStart(req, name, kind, paneID); err != nil {
		return err
	}
	s.lastName = name
	s.lastKind = kind
	// Cache live shape WITHOUT inventing a session id. ses_spawn_* was a
	// fabricated provisional that passed RefuseProvisionalWorkerSession.
	s.started[name] = &security.LiveAgentIdentity{
		Name:   name,
		Kind:   kind,
		TabID:  s.lastTab,
		PaneID: paneID,
		Status: "idle",
		// AgentSessionID empty until liveLookup reports one.
	}
	return nil
}

func (s *launcherSpawner) CloseTab(tabID string) error {
	return s.h.TabClose(tabID)
}

func isAgentNotFound(err error) bool {
	return err != nil && (errors.Is(err, security.ErrAgentNotFound) ||
		errors.Is(err, herdr.ErrAgentNotFound) ||
		strings.Contains(strings.ToLower(err.Error()), "not found"))
}

// Lookup prefers live Herdr authority. Empty agent_session is valid (grok).
// Hard live errors fail closed (no silent fallthrough to cache).
func (s *launcherSpawner) Lookup(name string) (*security.LiveAgentIdentity, error) {
	if s.liveLookup != nil {
		live, err := s.liveLookup(name)
		if err != nil {
			if !isAgentNotFound(err) {
				return nil, err // hard fail closed — do not hide behind cache
			}
			// not found: fall through to cache for hermetic unit tests
		} else if live != nil {
			cp := *live
			s.started[name] = &cp
			return live, nil
		}
	}
	if live, ok := s.started[name]; ok && live != nil {
		return live, nil
	}
	return nil, fmt.Errorf("%w: %s", security.ErrAgentNotFound, name)
}

// requireLiveIdentity re-checks live ownership via liveLookup (never cache-only).
// Session is optional: when both wantSession and live session are non-empty they
// must match; empty live session is allowed for session-less kinds (grok).
// Tab/pane when provided must match when live reports them.
func (s *launcherSpawner) requireLiveIdentity(name, wantSession, wantTab, wantPane string) (*security.LiveAgentIdentity, error) {
	if s.liveLookup == nil {
		return nil, fmt.Errorf("live identity check: no liveLookup wired")
	}
	live, err := s.liveLookup(name)
	if err != nil {
		return nil, fmt.Errorf("live identity check: %w", err)
	}
	if live == nil {
		return nil, fmt.Errorf("live identity check: agent %q missing", name)
	}
	if strings.TrimSpace(live.Name) != "" && !strings.EqualFold(live.Name, name) {
		return nil, fmt.Errorf("live identity name drift: want %s got %s", name, live.Name)
	}
	if wantTab != "" && live.TabID != "" && live.TabID != wantTab {
		return nil, fmt.Errorf("live tab drift: want %s got %s", wantTab, live.TabID)
	}
	if wantPane != "" && live.PaneID != "" && live.PaneID != wantPane {
		return nil, fmt.Errorf("live pane drift: want %s got %s", wantPane, live.PaneID)
	}
	wantSession = strings.TrimSpace(wantSession)
	gotSession := strings.TrimSpace(live.AgentSessionID)
	if wantSession != "" && gotSession != "" && wantSession != gotSession {
		return nil, fmt.Errorf("live session drift: want %s got %s", wantSession, gotSession)
	}
	// If we expected a real session and live still has none after start, that is
	// only OK when the expected binding itself was session-less (empty want).
	// Fabricated ses_spawn_* must never be the expected want.
	if wantSession != "" && gotSession == "" {
		// Session-expecting kinds lost their session — fail closed.
		// Exception: want was a live-agent: composite (session-less bind).
		if !strings.HasPrefix(wantSession, "live-agent:") {
			return nil, fmt.Errorf("live identity check: agent %q has no agent_session (expected %s)", name, wantSession)
		}
	}
	return live, nil
}

// bindingID returns the control-plane worker identity for a live agent.
// Prefer real agent_session when present; otherwise a live-confirmed
// name|tab|pane composite (not a fabricated ses_*).
func bindingID(live *security.LiveAgentIdentity) (string, error) {
	if live == nil {
		return "", fmt.Errorf("nil live identity")
	}
	return security.LiveWorkerBinding(live.Name, live.TabID, live.PaneID, live.AgentSessionID)
}
