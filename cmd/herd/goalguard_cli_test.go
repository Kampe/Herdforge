package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoalGuardCLISetCheckAndClear(t *testing.T) {
	state := filepath.Join(t.TempDir(), "goal.json")
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"herd", "goal-guard", "--set", "--state", state, "--lane", "forge-worker", "--task", "FAC-308", "--owner", "coordinator", "--generation", "4", "--max", "1"}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}

	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	input, err := os.CreateTemp(t.TempDir(), "evidence-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(`{"lane":"forge-worker","task":"FAC-308","owner":"coordinator","generation":4,"lease_held":true,"now":"2026-08-16T02:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	os.Args = []string{"herd", "goal-guard", "--check", "--state", state}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}

	os.Args = []string{"herd", "goal-guard", "--clear", "--state", state}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("clear left state behind: %v", err)
	}
}

func TestGoalGuardCLIMalformedEvidenceFailsClosed(t *testing.T) {
	state := filepath.Join(t.TempDir(), "goal.json")
	oldArgs, oldStdin := os.Args, os.Stdin
	defer func() { os.Args, os.Stdin = oldArgs, oldStdin }()
	os.Args = []string{"herd", "goal-guard", "--check", "--state", state}
	input, err := os.CreateTemp(t.TempDir(), "evidence-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(strings.Repeat("{", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	if err := runGoalGuard(); err == nil {
		t.Fatal("malformed evidence must fail closed")
	}
	_ = input.Close()
}
