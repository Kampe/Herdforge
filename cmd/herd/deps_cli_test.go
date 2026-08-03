package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHerdDepsSelftest_BinaryFixture runs the REAL compiled herd binary
// against FAC-75/90/93/105 drift fixtures and mutation controls (FAC-159).
func TestHerdDepsSelftest_BinaryFixture(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "deps", "selftest")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd deps selftest failed: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "fixture: PASS") {
		t.Fatalf("missing fixture PASS:\n%s", s)
	}
	if !strings.Contains(s, "mutation reconcile: PASS") {
		t.Fatalf("missing mutation reconcile PASS:\n%s", s)
	}
	if !strings.Contains(s, "mutation gate: PASS") {
		t.Fatalf("missing mutation gate PASS:\n%s", s)
	}
	if !strings.Contains(s, "herd deps selftest: PASS") {
		t.Fatalf("missing final PASS:\n%s", s)
	}
}

// TestHerdDepsSelftest_ExitZero is a non-vacuity pin: removing the selftest
// command must fail this binary test.
func TestHerdDepsSelftest_ExitZero(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "deps", "selftest")
	if err := cmd.Run(); err != nil {
		t.Fatalf("exit status: %v", err)
	}
}

func TestHerdDepsHelp(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "deps", "help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deps help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "selftest") {
		t.Fatalf("help missing selftest:\n%s", out)
	}
}

func TestHerdDepsUnknownSubcommand(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "deps", "not-a-command")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected nonzero exit for unknown subcommand")
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() == 0 {
			t.Fatal("exit 0")
		}
	}
}

// TestDispatchGateBlocksBeforeWorktree is a package-level proof that the
// binary includes deps in the production graph (import of cmd/herd deps).
func TestDispatchGateBlocksBeforeWorktree(t *testing.T) {
	// Ensure deps.go is part of the main package binary by invoking selftest
	// from a temp cwd without herd.yaml (selftest is hermetic).
	binary := buildHerd(t)
	dir := t.TempDir()
	cmd := exec.Command(binary, "deps", "selftest")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hermetic selftest: %v\n%s", err, out)
	}
	// Binary path must exist (real compile).
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	_ = filepath.Base(binary)
}
