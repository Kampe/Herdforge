package posture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Exclusion records one candidate dropped by family posture, with a reason.
type Exclusion struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Family   string `json:"family,omitempty"`
	Reason   string `json:"reason"`
}

// Candidate is a provider/model/family tuple the router scores.
type Candidate struct {
	Provider string
	Model    string
	Family   string
}

// Effective reports the mode every route/resolve/dispatch entry point must use.
//
// Order of authority:
//  1. Explicit HERD_CLAUDE_ONLY / HERD_NO_CLAUDE env (single-invocation override).
//     Contradictory env (both forced on) fails closed. An explicit "0" forces
//     that mode OFF for this invocation even when durable state is on.
//  2. Durable JSON state (generation-fenced). Missing state = clear.
//     Corrupt/unknown state fails closed.
//  3. Legacy file sentinels (pre-JSON) when JSON is missing. Contradictory
//     sentinels fail closed.
func Effective(ctx context.Context) (mode Mode, state State, err error) {
	if err := ctx.Err(); err != nil {
		return "", State{}, err
	}
	co, coSet := envTruthy(ClaudeOnly.EnvVar())
	nc, ncSet := envTruthy(NoClaude.EnvVar())
	if coSet && ncSet && co && nc {
		return "", State{}, &ErrContradictory{
			ClaudeOnlyPath: "env:" + ClaudeOnly.EnvVar(),
			NoClaudePath:   "env:" + NoClaude.EnvVar(),
		}
	}

	base, baseState, baseErr := effectiveDurable(ctx)
	if baseErr != nil {
		return "", State{}, baseErr
	}

	// No env: durable (or legacy) is authoritative.
	if !coSet && !ncSet {
		return base, baseState, nil
	}

	// Env force-on wins over durable entirely (single-invocation pin).
	if coSet && co {
		return ModeClaudeOnly, State{Mode: ModeClaudeOnly, Reason: "env:" + ClaudeOnly.EnvVar(), Scope: defaultScope}, nil
	}
	if ncSet && nc {
		return ModeNoClaude, State{Mode: ModeNoClaude, Reason: "env:" + NoClaude.EnvVar(), Scope: defaultScope}, nil
	}

	// Env force-off: clear that mode for this invocation only.
	mode = base
	if coSet && !co && mode == ModeClaudeOnly {
		mode = ModeClear
	}
	if ncSet && !nc && mode == ModeNoClaude {
		mode = ModeClear
	}
	return mode, State{Mode: mode, Reason: "env-override", Scope: defaultScope, Actor: baseState.Actor, Generation: baseState.Generation}, nil
}

func effectiveDurable(ctx context.Context) (Mode, State, error) {
	a, err := OpenDefault()
	if err != nil {
		return "", State{}, err
	}
	state, err := a.Read(ctx)
	if err == nil {
		return state.EffectiveMode(time.Now().UTC()), state, nil
	}
	if !errors.Is(err, ErrStateMissing) {
		// Corrupt / unreadable: fail closed.
		return "", State{}, fmt.Errorf("%w: %v", ErrUnknownState, err)
	}
	// Legacy sentinels only when JSON has never been written.
	co := SentinelPresent(ClaudeOnly)
	nc := SentinelPresent(NoClaude)
	if co && nc {
		return "", State{}, &ErrContradictory{
			ClaudeOnlyPath: ClaudeOnly.SentinelPath(),
			NoClaudePath:   NoClaude.SentinelPath(),
		}
	}
	switch {
	case co:
		return ModeClaudeOnly, State{Mode: ModeClaudeOnly, Reason: "legacy-sentinel", Scope: defaultScope}, nil
	case nc:
		return ModeNoClaude, State{Mode: ModeNoClaude, Reason: "legacy-sentinel", Scope: defaultScope}, nil
	default:
		return ModeClear, State{Mode: ModeClear, Scope: defaultScope}, nil
	}
}

// Allow reports whether a candidate is permitted under mode, and if not, why.
//
// Claude-only: native provider "claude" only. Proxy family and every other
// surface (including agy/lazer Anthropic models) are rejected — no proxy
// fallthrough when the operator ordered native Claude.
//
// No-claude: every Anthropic-family candidate is excluded (native claude,
// agy claude-*, lazer claude-*, …). Provider name alone is not enough.
func Allow(mode Mode, provider, model, family string) (ok bool, reason string) {
	provider = strings.TrimSpace(provider)
	family = strings.TrimSpace(family)
	switch mode {
	case ModeClear, "":
		return true, ""
	case ModeClaudeOnly:
		if provider != "claude" {
			return false, "claude-only: non-native provider"
		}
		if family == "proxy" {
			return false, "claude-only: proxy family forbidden"
		}
		if family != "" && family != "anthropic" {
			return false, "claude-only: non-anthropic family"
		}
		return true, ""
	case ModeNoClaude:
		if family == "anthropic" {
			return false, "no-claude: anthropic family excluded"
		}
		if provider == "claude" {
			return false, "no-claude: native claude excluded"
		}
		return true, ""
	default:
		// Unknown mode must fail closed at the Effective layer; if a caller
		// passes a garbage mode directly, refuse every candidate.
		return false, "unknown family posture mode"
	}
}

// Filter applies family posture to a candidate set before model scoring.
// Kept order is stable. Exclusions carry reasons for status/diagnostics.
// Empty kept is not an error here — callers fail closed when a mode requires
// a healthy route and none remains.
func Filter(mode Mode, candidates []Candidate) (kept []Candidate, excluded []Exclusion) {
	for _, c := range candidates {
		ok, reason := Allow(mode, c.Provider, c.Model, c.Family)
		if ok {
			kept = append(kept, c)
			continue
		}
		excluded = append(excluded, Exclusion{
			Provider: c.Provider,
			Model:    c.Model,
			Family:   c.Family,
			Reason:   reason,
		})
	}
	return kept, excluded
}

// FilterProviders is the provider-list form used when models are not yet
// resolved. Claude-only collapses to native claude only. No-claude drops
// native claude; Anthropic-via-other-providers is enforced later with family
// once the model is known (see Filter / Allow).
func FilterProviders(mode Mode, providers []string) (kept []string, excluded []Exclusion) {
	for _, p := range providers {
		switch mode {
		case ModeClaudeOnly:
			if p == "claude" {
				kept = append(kept, p)
			} else {
				excluded = append(excluded, Exclusion{Provider: p, Reason: "claude-only: non-native provider"})
			}
		case ModeNoClaude:
			if p == "claude" {
				excluded = append(excluded, Exclusion{Provider: p, Family: "anthropic", Reason: "no-claude: native claude excluded"})
			} else {
				kept = append(kept, p)
			}
		default:
			kept = append(kept, p)
		}
	}
	// Claude-only with no claude in the waterfall still injects native claude
	// so scoring can try the only allowed surface (and fail closed if unhealthy).
	if mode == ModeClaudeOnly && len(kept) == 0 {
		kept = []string{"claude"}
	}
	return kept, excluded
}

// ModeLabel is a short human string for logs and route rationales.
func ModeLabel(mode Mode) string {
	switch mode {
	case ModeClaudeOnly:
		return "claude-only"
	case ModeNoClaude:
		return "no-claude"
	case ModeClear:
		return "clear"
	default:
		return string(mode)
	}
}
