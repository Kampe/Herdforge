package posture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERD_STATE_DIR", dir)
	t.Setenv("HERD_FAMILY_POSTURE", "")
	t.Setenv("HERD_CLAUDE_ONLY", "")
	t.Setenv("HERD_NO_CLAUDE", "")
	return dir
}

// Durable JSON (and mirrored sentinels) must outlive the shell that set them.
func TestSentinelSurvivesWithoutAnyEnvVar(t *testing.T) {
	isolate(t)
	if Active(ClaudeOnly) {
		t.Fatal("posture must start off")
	}
	if err := Set(ClaudeOnly, true); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("HERD_CLAUDE_ONLY") // a new shell inherits nothing
	if !Active(ClaudeOnly) {
		t.Fatal("durable state must carry the posture with no env var set")
	}
	if err := Set(ClaudeOnly, false); err != nil {
		t.Fatal(err)
	}
	if Active(ClaudeOnly) {
		t.Fatal("off must clear the posture")
	}
}

// An explicit env var wins for a single invocation, in BOTH directions, so a
// one-off override never has to mutate shared state.
func TestEnvOverridesSentinelBothWays(t *testing.T) {
	isolate(t)
	if err := Set(NoClaude, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_NO_CLAUDE", "0")
	if Active(NoClaude) {
		t.Fatal("explicit 0 must override an on durable posture for this invocation")
	}
	// Durable JSON still records no-claude; env only changes effective mode.
	a, err := OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeNoClaude {
		t.Fatalf("env override must not mutate durable mode, got %s", st.Mode)
	}
	t.Setenv("HERD_NO_CLAUDE", "1")
	if !Active(NoClaude) {
		t.Fatal("explicit 1 must be honored")
	}
}

// `off` must clear DURABLE state even while a single-invocation env override
// shadows the effective mode — otherwise the CLI reports OFF while the fleet
// stays pinned.
func TestSetOffClearsDurableStateUnderEnvOverride(t *testing.T) {
	for _, tc := range []struct {
		name    string
		posture Name
		env     string
		value   string
	}{
		{"claude-only forced off for this invocation", ClaudeOnly, "HERD_CLAUDE_ONLY", "0"},
		{"claude-only shadowed by no-claude", ClaudeOnly, "HERD_NO_CLAUDE", "1"},
		{"no-claude forced off for this invocation", NoClaude, "HERD_NO_CLAUDE", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if err := Set(tc.posture, true); err != nil {
				t.Fatal(err)
			}
			t.Setenv(tc.env, tc.value)
			if err := Set(tc.posture, false); err != nil {
				t.Fatal(err)
			}
			a, err := OpenDefault()
			if err != nil {
				t.Fatal(err)
			}
			st, err := a.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode != ModeClear {
				t.Fatalf("durable mode after off = %s, want %s", st.Mode, ModeClear)
			}
			if SentinelPresent(tc.posture) {
				t.Fatal("legacy sentinel must be removed by off")
			}
		})
	}
}

// Contradictory legacy sentinels (no JSON) must fail loudly.
func TestContradictoryPosturesFailClosed(t *testing.T) {
	dir := isolate(t)
	if err := Resolve(); err != nil {
		t.Fatalf("clean state must resolve: %v", err)
	}
	// Write both legacy sentinels without JSON authority.
	for _, n := range []Name{ClaudeOnly, NoClaude} {
		path := filepath.Join(dir, string(n))
		if err := os.WriteFile(path, []byte("on\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var contradiction *ErrContradictory
	if err := Resolve(); !errors.As(err, &contradiction) {
		t.Fatalf("both postures active must fail closed, got %v", err)
	}
}

func TestBoardFrozenTriggers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_BOARD_FREEZE", "")
	if _, frozen := BoardFrozen(root); frozen {
		t.Fatal("clean repo must not be frozen")
	}
	t.Setenv("HERD_BOARD_FREEZE", "1")
	trigger, frozen := BoardFrozen(root)
	if !frozen || trigger != "env:HERD_BOARD_FREEZE=1" {
		t.Fatalf("env freeze = %q/%v", trigger, frozen)
	}
	t.Setenv("HERD_BOARD_FREEZE", "")
	if err := os.MkdirAll(filepath.Join(root, "docs", "board"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "board", "FREEZE"), []byte("held\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trigger, frozen = BoardFrozen(root)
	if !frozen || trigger != "file:docs/board/FREEZE" {
		t.Fatalf("file freeze = %q/%v", trigger, frozen)
	}
}
