// Package posture owns the durable provider-family execution policy
// (`herd posture claude-only|no-claude|clear|status`) and the board mutation
// freeze probe used by board-mutating tools.
//
// Family posture is a generation-fenced JSON authority (actor, reason, scope,
// optional expiry). Route, resolve, and dispatch all call Effective + Allow so
// the allowed candidate set cannot diverge across entry points. Env overrides
// (HERD_CLAUDE_ONLY / HERD_NO_CLAUDE) still win for a single invocation so a
// one-off never has to mutate shared state.
//
// This is an execution policy only — it does not rewrite historical agent
// metadata or ledger rows.
package posture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StateDir is the durable posture directory. Mirrors chainseer's
// ${HERD_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/herdforge/herd}.
func StateDir() string {
	if d := strings.TrimSpace(os.Getenv("HERD_STATE_DIR")); d != "" {
		return d
	}
	base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// No home and no XDG: keep it repo-relative rather than absolute.
			return filepath.Join(".herd", "state")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "herdforge", "herd")
}

// Name identifies one legacy posture sentinel (still mirrored for status UX).
type Name string

const (
	ClaudeOnly Name = "claude-only"
	NoClaude   Name = "no-claude"
)

// EnvVar is the single-invocation override for a posture.
func (n Name) EnvVar() string {
	switch n {
	case ClaudeOnly:
		return "HERD_CLAUDE_ONLY"
	case NoClaude:
		return "HERD_NO_CLAUDE"
	}
	return ""
}

// SentinelPath is the legacy durable file mirrored from the JSON authority.
func (n Name) SentinelPath() string { return filepath.Join(StateDir(), string(n)) }

// envTruthy reports the explicit env value and whether it was set at all.
// Any non-empty HERD_CLAUDE_ONLY is authoritative, so "0" explicitly turns the
// posture OFF for that invocation even when durable state is on.
func envTruthy(key string) (value, set bool) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no", "off":
		return false, true
	}
	return true, true
}

// Active reports whether the named legacy switch is effectively on. Prefer
// Effective for new code: it fails closed on corrupt state and returns Mode.
func Active(n Name) bool {
	mode, _, err := Effective(context.Background())
	if err != nil {
		// Fail closed for consumers that only check the bool: a corrupt
		// contradictory posture must not look "off" for either switch.
		return false
	}
	switch n {
	case ClaudeOnly:
		return mode == ModeClaudeOnly
	case NoClaude:
		return mode == ModeNoClaude
	}
	return false
}

// SentinelPresent reports only the legacy file, ignoring env and JSON.
func SentinelPresent(n Name) bool {
	_, err := os.Stat(n.SentinelPath())
	return err == nil
}

// EnvOverride reports whether an explicit env var is overriding durable state.
func EnvOverride(n Name) (value, set bool) { return envTruthy(n.EnvVar()) }

// Set turns a legacy named posture on or off by writing generation-fenced JSON.
// Prefer Authority.Update from the CLI so actor/reason/generation are explicit.
// This convenience path uses actor "legacy-set" and bumps generation.
func Set(n Name, on bool) error {
	ctx := context.Background()
	a, err := OpenDefault()
	if err != nil {
		return err
	}
	current, err := a.Read(ctx)
	gen := uint64(1)
	if err == nil {
		gen = current.Generation + 1
	} else if !errors.Is(err, ErrStateMissing) {
		return err
	}
	var mode Mode
	var reason string
	if on {
		switch n {
		case ClaudeOnly:
			mode = ModeClaudeOnly
			reason = "legacy-set-claude-only"
		case NoClaude:
			mode = ModeNoClaude
			reason = "legacy-set-no-claude"
		default:
			return fmt.Errorf("posture: unknown name %q", n)
		}
	} else {
		// Turning one switch off clears DURABLE state. Effective() would be
		// shadowed by a single-invocation env override, which made `off` a
		// silent no-op that left the fleet pinned (the operator is told OFF).
		durable, _, durErr := effectiveDurable(ctx)
		if durErr != nil {
			return durErr
		}
		switch {
		case n == ClaudeOnly && durable == ModeClaudeOnly:
			mode = ModeClear
			reason = "legacy-set-claude-only-off"
		case n == NoClaude && durable == ModeNoClaude:
			mode = ModeClear
			reason = "legacy-set-no-claude-off"
		default:
			// Already off durably: nothing to clear.
			return nil
		}
	}
	_, err = a.Update(ctx, mode, "legacy-set", reason, defaultScope, gen, nil)
	return err
}

// ErrContradictory is returned when both routing postures are active.
type ErrContradictory struct{ ClaudeOnlyPath, NoClaudePath string }

func (e *ErrContradictory) Error() string {
	return fmt.Sprintf("herd: CONTRADICTORY POSTURES, claude-only and no-claude are both active\n"+
		"  claude-only: %s\n  no-claude:   %s\n"+
		"  clear: herd posture clear --reason 'resolve contradiction'",
		e.ClaudeOnlyPath, e.NoClaudePath)
}

// Resolve fails closed when the two routing postures contradict each other.
func Resolve() error {
	_, _, err := Effective(context.Background())
	return err
}

// BoardFrozen ports herd_board_frozen: reports the active freeze trigger and
// whether the board is frozen. Prefer pkg/boardfreeze for the generation-fenced
// gate; this remains the env/file probe board-mutating shell tools call.
func BoardFrozen(repoRoot string) (trigger string, frozen bool) {
	if strings.TrimSpace(os.Getenv("HERD_BOARD_FREEZE")) == "1" {
		return "env:HERD_BOARD_FREEZE=1", true
	}
	if strings.TrimSpace(repoRoot) == "" {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "docs", "board", "FREEZE")); err == nil {
		return "file:docs/board/FREEZE", true
	}
	return "", false
}

// Now is exposed for tests that need the same clock notion as Effective expiry.
func Now() time.Time { return time.Now().UTC() }
