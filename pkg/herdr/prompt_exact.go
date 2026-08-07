package herdr

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/security"
)

// PromptBinding is the exact live identity a prompt may target.
// Delivery refuses if the live herdr list no longer matches every non-empty field.
type PromptBinding struct {
	Name           string
	TabID          string
	PaneID         string
	AgentSessionID string // required: real model session (not herdr-term/pane)
	Kind           string // optional expected agent kind
}

// Validate checks binding fields are non-provisional.
func (b PromptBinding) Validate() error {
	if strings.TrimSpace(b.Name) == "" {
		return fmt.Errorf("prompt binding: name required")
	}
	if err := security.RefuseProvisionalWorkerSession(b.AgentSessionID); err != nil {
		return fmt.Errorf("prompt binding: %w", err)
	}
	if !RealModelSessionID(b.AgentSessionID) {
		return fmt.Errorf("prompt binding: AgentSessionID %q is not a real model session", b.AgentSessionID)
	}
	return nil
}

// Target returns the herdr CLI target (agent name). Session binding is enforced
// via live re-check, not by stuffing the session id into the target string
// (herdr prompt targets name/pane, not session values).
func (b PromptBinding) Target() string {
	return strings.TrimSpace(b.Name)
}

// VerifyLive resolves the agent and ensures tab/pane/session/kind still match.
func (b PromptBinding) VerifyLive() (*AgentEntry, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	a, err := LookupAgent(b.Name)
	if err != nil {
		return nil, fmt.Errorf("prompt binding live lookup: %w", err)
	}
	if b.TabID != "" && a.TabID != "" && a.TabID != b.TabID {
		return nil, fmt.Errorf("prompt binding tab drift: want %s got %s", b.TabID, a.TabID)
	}
	if b.PaneID != "" && a.PaneID != "" && a.PaneID != b.PaneID {
		return nil, fmt.Errorf("prompt binding pane drift: want %s got %s", b.PaneID, a.PaneID)
	}
	if b.Kind != "" && a.Kind != "" && !strings.EqualFold(a.Kind, b.Kind) {
		return nil, fmt.Errorf("prompt binding kind drift: want %s got %s", b.Kind, a.Kind)
	}
	sid := strings.TrimSpace(a.Session.Value)
	// Prefer exact agent_session.value match; terminal fallback is not sufficient.
	if sid == "" {
		return nil, fmt.Errorf("prompt binding: live agent has no agent_session.value (cannot prove exact session)")
	}
	if sid != b.AgentSessionID {
		return nil, fmt.Errorf("prompt binding session drift: want %s got %s", b.AgentSessionID, sid)
	}
	if LoginOrAuthScreen(a.TerminalTitle, "") {
		return nil, fmt.Errorf("prompt binding: agent at login/auth screen (not model-ready)")
	}
	return a, nil
}

// AgentPromptExact submits a prompt only after live binding verification.
// Re-checks binding after prompt returns (session must not drift mid-call).
func AgentPromptExact(b PromptBinding, text string, wait bool) (string, error) {
	if _, err := b.VerifyLive(); err != nil {
		return "", err
	}
	out, err := AgentPrompt(b.Target(), text, wait)
	// Post-check: still the same session (best-effort; prompt may change status).
	if _, verr := b.VerifyLive(); verr != nil {
		// Login after prompt is a hard failure even if prompt "succeeded".
		if err == nil {
			return out, fmt.Errorf("post-prompt binding: %w", verr)
		}
		return out, fmt.Errorf("%v; post-prompt binding: %w", err, verr)
	}
	return out, err
}

// DeliverAndProveExact is DeliverAndProve with session-exact pre/post checks.
func DeliverAndProveExact(b PromptBinding, text string, timeout time.Duration) (*PromptReceipt, error) {
	if _, err := b.VerifyLive(); err != nil {
		return &PromptReceipt{Target: b.Target(), Verified: true}, err
	}
	rec, err := DeliverAndProve(b.Target(), text, timeout)
	if rec != nil {
		rec.Target = b.Target()
	}
	if _, verr := b.VerifyLive(); verr != nil {
		if err == nil {
			return rec, fmt.Errorf("post-delivery binding: %w", verr)
		}
		return rec, fmt.Errorf("%v; post-delivery binding: %w", err, verr)
	}
	return rec, err
}

// BindingFromSpawn builds a PromptBinding from launch result fields.
func BindingFromSpawn(name, tabID, paneID, sessionID, kind string) PromptBinding {
	return PromptBinding{
		Name: name, TabID: tabID, PaneID: paneID,
		AgentSessionID: sessionID, Kind: kind,
	}
}
