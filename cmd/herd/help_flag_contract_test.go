package main

import (
	"strings"
	"testing"
)

// TestHarvestMergeHelpNamesRealFlags is the FAC-570 regression.
//
// harvest-merge's help was one line with NO flag list, so the only way to learn
// its options was to read the source. That is how I came to tell a caller to use
// --candidate-sha, a flag that does not exist: nothing in the public contract
// could contradict me, and the caller hit an unknown-flag failure.
//
// The help must name the flags an operator needs, and must not name flags the
// binary does not accept.
func TestHarvestMergeHelpNamesRealFlags(t *testing.T) {
	help, ok := subcommandUsage["harvest-merge"]
	if !ok {
		t.Fatal("harvest-merge must have registered help")
	}
	for _, flag := range []string{
		"--candidate", "--candidate-range", "--verify-landed", "--ref", "--base",
	} {
		if !strings.Contains(help, flag) {
			t.Fatalf("help must document %s; an undocumented flag surface is how a nonexistent one got taught", flag)
		}
	}
	// The correction itself must be visible, because the wrong name is already
	// in circulation in prior operator guidance.
	if !strings.Contains(help, "NOT") || !strings.Contains(help, "--candidate-sha") {
		t.Fatal("help must explicitly correct the invented --candidate-sha name")
	}
}

// A one-line help entry for a command with many flags is the shape that caused
// this. Flag-bearing commands must document them.
func TestFlagBearingCommandsDocumentFlags(t *testing.T) {
	// harvest-merge is the proven case; board-done carries the override quartet
	// a caller cannot guess.
	for _, name := range []string{"harvest-merge", "board-done"} {
		help := subcommandUsage[name]
		if !strings.Contains(help, "--") {
			t.Fatalf("%s help documents no flags at all: %q", name, help)
		}
	}
}
