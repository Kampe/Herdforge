package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestREPL_EvalAndRun(t *testing.T) {
	inputCommands := "help\nstatus\nclaim FAC-45\nexit\n"
	inBuf := strings.NewReader(inputCommands)
	outBuf := &bytes.Buffer{}

	repl := NewREPL(inBuf, outBuf, nil)
	if err := repl.Run(); err != nil {
		t.Fatalf("expected clean REPL execution, got err: %v", err)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, "Available commands") {
		t.Errorf("expected help output in REPL")
	}
	if !strings.Contains(outStr, "[OFFLINE] claim FAC-45") {
		t.Errorf("expected offline label for claim in offline mode, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Goodbye!") {
		t.Errorf("expected exit output in REPL")
	}
}

func TestParseREPLCommand_Empty(t *testing.T) {
	cmd, args := ParseREPLCommand("   ")
	if cmd != "" || len(args) != 0 {
		t.Errorf("expected empty command for whitespace input")
	}
}

func TestREPL_OfflineModeLabelsAllLiveCommands(t *testing.T) {
	for _, cmd := range []string{"status", "lanes", "budget", "tasks", "claim FAC-99"} {
		inBuf := strings.NewReader(cmd + "\nexit\n")
		outBuf := &bytes.Buffer{}
		repl := NewREPL(inBuf, outBuf, nil)
		if err := repl.Run(); err != nil {
			t.Fatalf("Run failed for %q: %v", cmd, err)
		}
		if !strings.Contains(outBuf.String(), "[OFFLINE]") {
			t.Errorf("expected [OFFLINE] label for %q in offline mode, got: %s", cmd, outBuf.String())
		}
	}
}
