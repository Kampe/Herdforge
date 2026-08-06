// Package posture ports the chainseer fleet-wide routing postures
// (bin/herd-claude-only, bin/herd-no-claude) and the board mutation freeze
// (bin/herd-board-frozen).
//
// These are FILE sentinels, not environment variables, and that is the whole
// point. The posture started as HERD_CLAUDE_ONLY=1, but an env export dies with
// its shell and a coordinator drives the fleet through one-shot Bash calls. One
// forgotten prefix leaked a lane onto a pool the operator had ordered held
// (chainseer, 2026-07-23). A sentinel survives shells, so a bare launch carries
// the posture with no prefix at all. Turning a posture off is then a deliberate,
// visible act rather than something that reverts when a shell exits.
//
// An explicit environment variable still wins for a single invocation, so a
// one-off override in either direction never has to mutate shared state.
package posture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is the durable posture directory. Mirrors chainseer's
// ${HERD_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/<repo>/herd}.
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

// Name identifies one posture sentinel.
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

// SentinelPath is the durable file backing a posture.
func (n Name) SentinelPath() string { return filepath.Join(StateDir(), string(n)) }

// envTruthy reports the explicit env value and whether it was set at all.
// Chainseer treats any non-empty HERD_CLAUDE_ONLY as authoritative, so "0"
// explicitly turns the posture OFF for that invocation even when the sentinel
// exists. Preserve that: it is how a one-off override works in both directions.
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

// Active reports the EFFECTIVE posture the launchers will see.
func Active(n Name) bool {
	if v, set := envTruthy(n.EnvVar()); set {
		return v
	}
	_, err := os.Stat(n.SentinelPath())
	return err == nil
}

// SentinelPresent reports only the persisted file, ignoring any env override.
// `status` needs both so an env override can never be mistaken for the
// persisted posture — exactly the case where the file would otherwise mislead.
func SentinelPresent(n Name) bool {
	_, err := os.Stat(n.SentinelPath())
	return err == nil
}

// EnvOverride reports whether an explicit env var is overriding the sentinel.
func EnvOverride(n Name) (value, set bool) { return envTruthy(n.EnvVar()) }

// Set turns a posture on or off durably.
func Set(n Name, on bool) error {
	path := n.SentinelPath()
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("posture %s off: %w", n, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("posture %s on: create state dir: %w", n, err)
	}
	body := fmt.Sprintf("%s enabled %s\n", n, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("posture %s on: %w", n, err)
	}
	return nil
}

// ErrContradictory is returned when both routing postures are active.
type ErrContradictory struct{ ClaudeOnlyPath, NoClaudePath string }

func (e *ErrContradictory) Error() string {
	return fmt.Sprintf("herd: CONTRADICTORY POSTURES, claude-only and no-claude are both active\n"+
		"  claude-only: %s\n  no-claude:   %s\n"+
		"  clear one: herd claude-only off   OR   herd no-claude off",
		e.ClaudeOnlyPath, e.NoClaudePath)
}

// Resolve fails closed when the two routing postures contradict each other.
// One says route everything to Claude, the other says Claude is out; whichever
// won, the operator did not ask for it. Fail loudly at the only place that can
// see both, rather than silently picking.
func Resolve() error {
	if Active(ClaudeOnly) && Active(NoClaude) {
		return &ErrContradictory{
			ClaudeOnlyPath: ClaudeOnly.SentinelPath(),
			NoClaudePath:   NoClaude.SentinelPath(),
		}
	}
	return nil
}

// BoardFrozen ports herd_board_frozen: reports the active freeze trigger and
// whether the board is frozen. Every board-mutating tool consults this instead
// of reimplementing the check.
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
