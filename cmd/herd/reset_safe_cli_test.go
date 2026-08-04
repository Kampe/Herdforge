package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

const wantResetSafeUsage = "Usage: herd reset-safe <worktree-path>"

func TestResetSafeHelpByteContract(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "reset-safe", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reset-safe help failed: %v\n%s", err, out)
	}
	if got, want := string(out), wantResetSafeUsage+"\n"; got != want {
		t.Fatalf("help bytes = %q, want %q", got, want)
	}
}

func TestResetSafeUsageExitAndByteContract(t *testing.T) {
	binary := buildHerd(t)
	for _, args := range [][]string{{"reset-safe"}, {"reset-safe", "one", "two"}} {
		cmd := exec.Command(binary, args...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
			t.Fatalf("%v exit = %v, want 2", args, err)
		}
		if got, want := string(out), wantResetSafeUsage+"\n"; got != want {
			t.Fatalf("%v bytes = %q, want %q", args, got, want)
		}
	}
}

func TestResetSafeRoutesOperationalErrors(t *testing.T) {
	binary := buildHerd(t)
	worktree := filepath.Join(t.TempDir(), "missing-worktree")
	cmd := exec.Command(binary, "reset-safe", worktree)
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("missing worktree unexpectedly succeeded")
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("operational error exit = %v, want 1", err)
	}
	want := "herd-reset-safe: " + worktree + " does not exist\n"
	if got := string(out); got != want {
		t.Fatalf("operational error bytes = %q, want %q", got, want)
	}
}
