package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildHerd(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "herd")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v, output: %s", err, out)
	}
	return binary
}

func TestVersionFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("herd --version failed: %v", err)
	}
	if !strings.Contains(string(out), "herd version") {
		t.Errorf("expected version output, got %s", string(out))
	}
}

func TestHelpFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("herd --help failed: %v", err)
	}
	if !strings.Contains(string(out), "Herdforge") {
		t.Errorf("expected help output, got %s", string(out))
	}
}

func TestVFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "-v").CombinedOutput()
	if err != nil {
		t.Fatalf("herd -v failed: %v", err)
	}
	if !strings.Contains(string(out), "herd version") {
		t.Errorf("expected version output, got %s", string(out))
	}
}

func TestHFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "-h").CombinedOutput()
	if err != nil {
		t.Fatalf("herd -h failed: %v", err)
	}
	if !strings.Contains(string(out), "Herdforge") {
		t.Errorf("expected help output, got %s", string(out))
	}
}

func TestUnknownCommand(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "nonexistent")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit error for unknown command")
	}
	if !strings.Contains(string(out), "unknown subcommand") {
		t.Errorf("expected unknown subcommand message, got %s", string(out))
	}
}

func TestInit(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	cmd := exec.Command(binary, "init")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd init failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "Scaffolded") {
		t.Errorf("expected scaffold message, got %s", string(out))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".herd", "herd.yaml")); os.IsNotExist(err) {
		t.Error(".herd/herd.yaml should exist")
	}
}

func TestInitTwice(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	// First init
	initCmd := exec.Command(binary, "init")
	initCmd.Dir = tmpDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("first init failed: %v, output: %s", err, out)
	}
	// Second init in same dir
	cmd := exec.Command(binary, "init")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("second init failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "already exists") {
		t.Errorf("expected 'already exists' message, got %s", string(out))
	}
}

func TestStatusUninitialized(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	cmd := exec.Command(binary, "status")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status in uninitialized dir failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "Uninitialized") {
		t.Errorf("expected Uninitialized, got %s", string(out))
	}
}

func TestStatusInitialized(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	initCmd := exec.Command(binary, "init")
	initCmd.Dir = tmpDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init failed: %v, output: %s", err, out)
	}
	cmd := exec.Command(binary, "status")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "Active") {
		t.Errorf("expected Active, got %s", string(out))
	}
}

func TestNoArgs(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary).CombinedOutput()
	if err != nil {
		t.Fatalf("herd with no args failed: %v", err)
	}
	if !strings.Contains(string(out), "Herdforge") {
		t.Errorf("expected help output on no args, got %s", string(out))
	}
}
