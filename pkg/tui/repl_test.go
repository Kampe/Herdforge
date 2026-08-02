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

	repl := NewREPL(inBuf, outBuf)
	if err := repl.Run(); err != nil {
		t.Fatalf("expected clean REPL execution, got err: %v", err)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, "Available commands") {
		t.Errorf("expected help output in REPL")
	}
	if !strings.Contains(outStr, "Task FAC-45 claimed") {
		t.Errorf("expected claim output in REPL")
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
