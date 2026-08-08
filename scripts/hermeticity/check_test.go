package main

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile writes a Go test file into dir and returns its path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// findingsContain reports whether any finding's message contains substr.
func findingsContain(findings []finding, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f.message, substr) {
			return true
		}
	}
	return false
}

// TestScannerDetectsLookPathWithoutSkip verifies that exec.LookPath without
// t.Skip is flagged. This test MUST fail if the scanner stops detecting this
// pattern — it is the FAC-215 "required codex/claude/grok on PATH" defect.
func TestScannerDetectsLookPathWithoutSkip(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "bad_test.go", `package bad
import (
	"os/exec"
	"testing"
)
func TestRequiresMissingBinary(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatal("codex not found")
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if !findingsContain(findings, "exec.LookPath without t.Skip") {
		t.Fatalf("expected LookPath-without-skip finding, got %v", findings)
	}
}

// TestScannerPassesLookPathWithSkip verifies that exec.LookPath followed by
// t.Skip is NOT flagged. This must not produce a false positive.
func TestScannerPassesLookPathWithSkip(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "good_test.go", `package good
import (
	"os/exec"
	"testing"
)
func TestSkipsMissingBinary(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if len(findings) != 0 {
		t.Fatalf("expected no findings for LookPath+Skip, got %v", findings)
	}
}

// TestScannerDetectsFragileBinaryCommandWithoutGuard verifies that
// exec.Command on a fragile binary without LookPath+Skip is flagged.
func TestScannerDetectsFragileBinaryCommandWithoutGuard(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "bad_test.go", `package bad
import (
	"os/exec"
	"testing"
)
func TestRunsDocker(t *testing.T) {
	out, err := exec.Command("docker", "info").CombinedOutput()
	if err != nil {
		t.Fatalf("docker: %v\n%s", err, out)
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if !findingsContain(findings, `exec.Command("docker") without`) {
		t.Fatalf("expected fragile-binary finding, got %v", findings)
	}
}

// TestScannerPassesFragileBinaryWithGuard verifies that exec.Command on a
// fragile binary WITH a preceding LookPath+Skip is NOT flagged.
func TestScannerPassesFragileBinaryWithGuard(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "good_test.go", `package good
import (
	"os/exec"
	"testing"
)
func TestRunsDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	out, err := exec.Command("docker", "info").CombinedOutput()
	if err != nil {
		t.Fatalf("docker: %v\n%s", err, out)
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if len(findings) != 0 {
		t.Fatalf("expected no findings for docker with guard, got %v", findings)
	}
}

// TestScannerPassesGitCommand verifies that exec.Command("git") is NOT
// flagged — git is always present on CI.
func TestScannerPassesGitCommand(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "good_test.go", `package good
import (
	"os/exec"
	"testing"
)
func TestGitRevParse(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	_ = out
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if len(findings) != 0 {
		t.Fatalf("expected no findings for git command, got %v", findings)
	}
}

// TestScannerDetectsDockerPlatformFlag verifies that the Docker 28+ only
// --platform flag on image inspect is flagged.
func TestScannerDetectsDockerPlatformFlag(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "bad_test.go", `package bad
import (
	"testing"
)
func TestDockerInspect(t *testing.T) {
	cmd := "docker image inspect --platform linux/amd64 golang:1.25"
	_ = cmd
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if !findingsContain(findings, "image inspect") || !findingsContain(findings, "--platform") {
		t.Fatalf("expected docker --platform finding, got %v", findings)
	}
}

// TestScannerDetectsArgvPositionAssertion verifies that index-based argv
// assertions with flag strings are flagged — the FAC-173 pattern.
func TestScannerDetectsArgvPositionAssertion(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "bad_test.go", `package bad
import (
	"testing"
)
func TestArgvPosition(t *testing.T) {
	argv := []string{"codex", "--disable", "multi_agent", "--model", "gpt-5"}
	if argv[1] != "--model" {
		t.Fatal("expected --model at position 1")
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if !findingsContain(findings, "index-based argv assertion") {
		t.Fatalf("expected argv position finding, got %v", findings)
	}
}

// TestScannerPassesArgvPositionWithSuppression verifies that a
// //hermetic:allow-argv-position comment suppresses the finding.
func TestScannerPassesArgvPositionWithSuppression(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "good_test.go", `package good
import (
	"testing"
)
func TestArgvPosition(t *testing.T) {
	argv := []string{"codex", "--disable", "multi_agent", "--model", "gpt-5"}
	if argv[3] != "--model" { //hermetic:allow-argv-position fixed contract
		t.Fatal("expected --model at position 3")
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if len(findings) != 0 {
		t.Fatalf("expected no findings with suppression, got %v", findings)
	}
}

// TestScannerPassesCleanTest verifies that a fully hermetic test file
// produces zero findings.
func TestScannerPassesCleanTest(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "clean_test.go", `package clean
import (
	"testing"
)
func TestPureLogic(t *testing.T) {
	got := 2 + 2
	if got != 4 {
		t.Fatalf("2+2 = %d, want 4", got)
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if len(findings) != 0 {
		t.Fatalf("expected no findings for clean test, got %v", findings)
	}
}

// TestScannerDoesNotMatchMethodLookPath verifies that a method call named
// LookPath (e.g. cfg.LookPath()) is NOT flagged as exec.LookPath.
func TestScannerDoesNotMatchMethodLookPath(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "good_test.go", `package good
import (
	"testing"
)
type Config struct{ BinaryName string }
func (c *Config) LookPath() (string, error) { return "", nil }
func TestMethodLookPath(t *testing.T) {
	cfg := &Config{BinaryName: "missing-binary-xyzzy"}
	_, err := cfg.LookPath()
	if err == nil {
		t.Fatal("expected error")
	}
}
`)
	fset := token.NewFileSet()
	findings := scanDir(fset, dir)
	if findingsContain(findings, "exec.LookPath") {
		t.Fatalf("method LookPath should not be flagged, got %v", findings)
	}
}
