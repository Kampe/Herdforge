package herdr

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestStaleGorootIsPinnedToThePathToolchain is the FAC-558 gate.
//
// A managed lane inherited the launcher's stale exported GOROOT, so every Go
// command inside it failed on mixed tool versions and the standing workaround
// was `env -u GOROOT` on each invocation. The lane env is delivered via
// `herdr tab create --env KEY=VALUE`, which can only SET, so neutralizing an
// inherited value means pinning it to what the PATH-resolved toolchain reports
// for itself.
func TestStaleGorootIsPinnedToThePathToolchain(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH to resolve an authoritative toolchain")
	}
	real, ok := pathResolvedGoEnv("GOROOT")
	if !ok {
		t.Skip("cannot resolve GOROOT from the PATH toolchain")
	}
	t.Setenv("GOROOT", "/nonexistent/stale/go/root")

	pins := resolveGoToolchainEnv()
	var got string
	for _, pin := range pins {
		if strings.HasPrefix(pin, "GOROOT=") {
			got = strings.TrimPrefix(pin, "GOROOT=")
		}
	}
	if got == "" {
		t.Fatal("a stale exported GOROOT must be pinned, not passed through")
	}
	if got != real {
		t.Errorf("GOROOT pinned to %q, want the PATH-resolved %q", got, real)
	}
	if got == "/nonexistent/stale/go/root" {
		t.Error("the stale value must not survive into the lane")
	}
}

// With nothing exported there is nothing stale, and injecting a value would be
// inventing a toolchain choice the operator never made.
func TestNothingExportedMeansNoPin(t *testing.T) {
	if err := os.Unsetenv("GOROOT"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("GOTOOLDIR"); err != nil {
		t.Fatal(err)
	}
	if pins := resolveGoToolchainEnv(); len(pins) != 0 {
		t.Errorf("no exported toolchain means no pin, got %v", pins)
	}
}

// An explicit caller choice must win over normalization: normalization exists
// to remove accidental staleness, not to override intent.
func TestExplicitCallerValueWins(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	t.Setenv("GOROOT", "/nonexistent/stale/go/root")
	// Reset the process-wide memo so this test observes the stale value.
	resetGoToolchainMemoForTest()

	env := withGoToolchainPinned([]string{"GOROOT=/deliberate/choice"})
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "GOROOT=") {
			count++
			if entry != "GOROOT=/deliberate/choice" {
				t.Errorf("explicit value must win, got %q", entry)
			}
		}
	}
	if count != 1 {
		t.Errorf("exactly one GOROOT entry expected, got %d in %v", count, env)
	}
}

// TestNormalizationDoesNotSuppressTheDiagnostic is FAC-558 acceptance
// criterion 2.
//
// Normalization applies only to lanes Herdforge launches. An operator who
// exports GOROOT in their own shell must still trip the preflight mismatch
// diagnostic — we normalize what we own and keep diagnosing what we do not.
// If normalization reached the ambient process environment it would silence a
// real misconfiguration everywhere.
func TestNormalizationDoesNotSuppressTheDiagnostic(t *testing.T) {
	stale := "/nonexistent/stale/go/root"
	t.Setenv("GOROOT", stale)
	resetGoToolchainMemoForTest()

	// Pins are returned for a child's env; the caller's own environment is
	// untouched.
	_ = GoToolchainEnv()
	if got := os.Getenv("GOROOT"); got != stale {
		t.Errorf("the ambient environment must not be rewritten; GOROOT is now %q", got)
	}
}
