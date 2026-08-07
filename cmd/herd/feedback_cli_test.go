package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackCLISelftest(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "feedback", "--selftest").CombinedOutput()
	if err != nil {
		t.Fatalf("herd feedback --selftest failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("expected PASS in selftest output, got %s", out)
	}
}

// With no herdr reachable on PATH, workspace resolution must fail closed
// rather than silently proceeding as if the fleet were empty.
func TestFeedbackCLIWorkspaceUnresolvedFailsClosed(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	mailDir := filepath.Join(dir, "mail")

	cmd := exec.Command(binary, "feedback", "--state-dir", stateDir, "--mail-dir", mailDir)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin", "HERD_WORKSPACE=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit when the workspace cannot be resolved, got success: %s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1", exitErr.ExitCode())
	}
	if !strings.Contains(string(out), "herd-feedback: workspace unresolved; refusing a false empty census") {
		t.Fatalf("expected the exact refusal text, got %s", out)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "current.json")); !os.IsNotExist(statErr) {
		t.Fatalf("workspace-unresolved must write nothing to state dir, stat err=%v", statErr)
	}
}
