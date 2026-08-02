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
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binary, ".")
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

func TestInitFull(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	cmd := exec.Command(binary, "init", "--full")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd init --full failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "3-lane forge") {
		t.Errorf("expected 3-lane forge message, got %s", string(out))
	}
	// Verify all three prompts were created
	for _, prompt := range []string{".herd/prompts/smith.md", ".herd/prompts/worker.md", ".herd/prompts/reviewer.md"} {
		if _, err := os.Stat(filepath.Join(tmpDir, prompt)); os.IsNotExist(err) {
			t.Errorf("%s should exist", prompt)
		}
	}
	// Verify config has all three lanes
	cfgData, err := os.ReadFile(filepath.Join(tmpDir, ".herd", "herd.yaml"))
	if err != nil {
		t.Fatal("herd.yaml should exist after init --full")
	}
	cfgStr := string(cfgData)
	for _, lane := range []string{"forge-smith", "worker", "reviewer"} {
		if !strings.Contains(cfgStr, lane) {
			t.Errorf("config should contain lane %s", lane)
		}
	}
}

func TestInitFullTwice(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	// First init --full
	initCmd := exec.Command(binary, "init", "--full")
	initCmd.Dir = tmpDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("first init --full failed: %v, output: %s", err, out)
	}
	// Modify one prompt to test non-overwrite
	os.WriteFile(filepath.Join(tmpDir, ".herd", "prompts", "worker.md"), []byte("custom content"), 0644)
	// Second init --full
	cmd := exec.Command(binary, "init", "--full")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("second init --full failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "3-lane forge") {
		t.Errorf("expected 3-lane forge message on second run, got %s", string(out))
	}
	// Verify existing prompt was NOT overwritten
	data, err := os.ReadFile(filepath.Join(tmpDir, ".herd", "prompts", "worker.md"))
	if err != nil {
		t.Fatal("worker.md should exist")
	}
	if string(data) != "custom content" {
		t.Errorf("existing prompt should not have been overwritten, got %s", string(data))
	}
}

func TestCloneUsage(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "clone")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected error for clone without args")
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Errorf("expected usage message, got %s", string(out))
	}
}

func TestCloneHelpInUsage(t *testing.T) {
	binary := buildHerd(t)
	// Verify clone appears in help output
	out, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("herd --help failed: %v", err)
	}
	if !strings.Contains(string(out), "clone") {
		t.Errorf("help should list clone command, got %s", string(out))
	}
}
