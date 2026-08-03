package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// ProviderLaneState is the production projection of task-board health for the
// forge control plane (FAC-150). Distinct from lifecycle.StateBlocked so the
// daemon can stay responsive while refusing to claim more work.
type ProviderLaneState string

const (
	// ProviderOK — board calls are healthy.
	ProviderOK ProviderLaneState = "ok"
	// ProviderBlocked — last external call timed out / ambiguous; do not claim.
	// Projected as BLOCKED(provider_timeout).
	ProviderBlocked ProviderLaneState = "blocked"
	// ProviderRecovering — attempting board I/O after a block without claiming.
	ProviderRecovering ProviderLaneState = "recovering"
)

// ProviderHealth is the durable-in-process health surface consumers read.
type ProviderHealth struct {
	State     ProviderLaneState
	Class     provider.OpFailureClass
	LastError string
	UpdatedAt time.Time
}

// String projects the operator/fleet label.
func (h ProviderHealth) String() string {
	switch h.State {
	case ProviderBlocked:
		if h.Class == provider.OpAmbiguous {
			return "BLOCKED(provider_ambiguous)"
		}
		return "BLOCKED(provider_timeout)"
	case ProviderRecovering:
		return "recovering"
	default:
		return "ok"
	}
}

// providerHealth tracks timeout → BLOCKED → recovering → ok for an Engine.
type providerHealth struct {
	mu    sync.Mutex
	state ProviderLaneState
	class provider.OpFailureClass
	err   string
	at    time.Time
}

func (p *providerHealth) snapshot() ProviderHealth {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProviderHealth{State: p.state, Class: p.class, LastError: p.err, UpdatedAt: p.at}
}

// observe applies ClassifyOpError to the lane state machine.
// nil err: recovering → ok; blocked without attempt stays blocked until
// beginRecovery is called.
// timeout/ambiguous: → blocked.
func (p *providerHealth) observe(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.at = time.Now()
	if err == nil {
		p.class = provider.OpOK
		p.err = ""
		if p.state == ProviderRecovering {
			p.state = ProviderOK
		}
		// First success from OK stays OK; success never silently clears
		// Blocked without an explicit recovery attempt (beginRecovery).
		return
	}
	class := provider.ClassifyOpError(err)
	p.class = class
	p.err = err.Error()
	if class == provider.OpTimeout || class == provider.OpAmbiguous {
		p.state = ProviderBlocked
	}
}

// beginRecovery moves blocked → recovering so the next board call is a probe
// without claiming work. No-op unless currently blocked.
func (p *providerHealth) beginRecovery() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == ProviderBlocked {
		p.state = ProviderRecovering
		p.at = time.Now()
	}
}

// isBlocked reports whether claim/dispatch must be suppressed.
func (p *providerHealth) isBlocked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == ProviderBlocked
}

// engineDeadlines resolves configurable deadlines from config.
func engineDeadlines(cfg *config.Config) provider.Deadlines {
	if cfg == nil {
		return provider.DefaultDeadlines()
	}
	g, l, m, c, r, err := cfg.TaskProvider.Deadlines.Resolved()
	if err != nil {
		// Validate should have caught this; fall back to defaults fail-soft
		// for long-running daemons if config was mutated in-process.
		return provider.DefaultDeadlines()
	}
	return provider.DeadlinesFromParts(g, l, m, c, r)
}

// applyConfiguredDeadlines pins package defaults/overrides onto the live adapter.
func applyConfiguredDeadlines(cfg *config.Config, tp provider.TaskProvider) {
	provider.ApplyDeadlines(tp, engineDeadlines(cfg))
}

// formatProviderStepError wraps a board error with the projected class label.
func formatProviderStepError(op string, err error) error {
	if err == nil {
		return nil
	}
	class := provider.ClassifyOpError(err)
	if class == provider.OpTimeout || class == provider.OpAmbiguous {
		return fmt.Errorf("%s: BLOCKED(provider_timeout): %w", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
