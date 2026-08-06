package posture

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERD_STATE_DIR", dir)
	t.Setenv("HERD_CLAUDE_ONLY", "")
	t.Setenv("HERD_NO_CLAUDE", "")
	return dir
}

// The whole reason these are files: the posture must outlive the shell that
// set it, because the coordinator drives the fleet through one-shot calls.
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
		t.Fatal("sentinel must carry the posture with no env var set")
	}
	if err := Set(ClaudeOnly, false); err != nil {
		t.Fatal(err)
	}
	if Active(ClaudeOnly) {
		t.Fatal("off must clear the sentinel")
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
		t.Fatal("explicit 0 must override an on sentinel for this invocation")
	}
	if !SentinelPresent(NoClaude) {
		t.Fatal("an env override must not mutate the persisted posture")
	}
	t.Setenv("HERD_NO_CLAUDE", "1")
	if !Active(NoClaude) {
		t.Fatal("explicit 1 must be honored")
	}
}

// Contradictory postures must fail loudly instead of silently picking one.
func TestContradictoryPosturesFailClosed(t *testing.T) {
	isolate(t)
	if err := Resolve(); err != nil {
		t.Fatalf("clean state must resolve: %v", err)
	}
	if err := Set(ClaudeOnly, true); err != nil {
		t.Fatal(err)
	}
	if err := Set(NoClaude, true); err != nil {
		t.Fatal(err)
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
