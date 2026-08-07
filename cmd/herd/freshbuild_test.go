package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFreshBuildArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		args       []string
		wantTarget string
		wantDry    bool
		wantErr    bool
	}{
		{"target only", []string{"@scope/a"}, "@scope/a", false, false},
		{"target dry", []string{"@scope/a", "--dry-run"}, "@scope/a", true, false},
		{"dry target", []string{"--dry-run", "@scope/a"}, "@scope/a", true, false},
		{"bare dry", []string{"--dry-run"}, "", true, false},
		{"empty", nil, "", false, false},
		{"unknown flag", []string{"--nope"}, "", false, true},
		{"extra pos", []string{"a", "b"}, "", false, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, dry, err := parseFreshBuildArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if target != tc.wantTarget || dry != tc.wantDry {
				t.Fatalf("target=%q dry=%v", target, dry)
			}
		})
	}
}

func TestFreshBuildCLI_UsageNoTarget(t *testing.T) {
	binary := buildHerd(t)
	c := exec.Command(binary, "fresh-build")
	c.Dir = t.TempDir()
	out, err := c.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got output:\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("exit=%v out=%s", err, out)
	}
	if !strings.Contains(string(out), "usage: herd fresh-build") {
		t.Fatalf("usage missing:\n%s", out)
	}
}

func TestFreshBuildCLI_DryRunBareIsUsage(t *testing.T) {
	binary := buildHerd(t)
	c := exec.Command(binary, "fresh-build", "--dry-run")
	c.Dir = t.TempDir()
	out, err := c.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero, out=%s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("exit=%v out=%s", err, out)
	}
	body := string(out)
	if !strings.Contains(body, "usage: herd fresh-build") {
		t.Fatalf("expected usage for bare --dry-run:\n%s", body)
	}
}

// TestFreshBuildCLI_DryRunOnGoModule pins the honest-Go-profile contract:
// dry-run must not promise a dist/tsbuildinfo clear or claim STALE DIST.
func TestFreshBuildCLI_DryRunOnGoModule(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/tmp\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(binary, "fresh-build", ".", "--dry-run")
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	body := string(out)
	if !strings.Contains(body, "Nothing changed.") {
		t.Fatalf("expected dry-run plan:\n%s", body)
	}
	if strings.Contains(body, "STALE DIST") {
		t.Fatalf("Go dry-run must not claim STALE DIST:\n%s", body)
	}
	if strings.Contains(body, "would clear dist/") {
		t.Fatalf("Go dry-run must not promise dist clear:\n%s", body)
	}
	if !strings.Contains(body, "nothing") {
		t.Fatalf("Go dry-run must state clear=nothing:\n%s", body)
	}
}

// TestFreshBuildCLI_GoCleanDoesNotLieAboutStaleDist runs a real go build via
// the CLI and asserts the success line never diagnoses STALE DIST.
func TestFreshBuildCLI_GoCleanDoesNotLieAboutStaleDist(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/tmp\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(binary, "fresh-build", ".")
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh-build failed: %v\n%s", err, out)
	}
	body := string(out)
	if strings.Contains(body, "STALE DIST") {
		t.Fatalf("Go clean rebuild must not claim STALE DIST:\n%s", body)
	}
	if strings.Contains(body, "cleared dist") {
		t.Fatalf("Go clean rebuild must not claim cleared dist:\n%s", body)
	}
	if !strings.Contains(body, "does not diagnose stale dist") {
		t.Fatalf("expected honest Go clean message:\n%s", body)
	}
}
