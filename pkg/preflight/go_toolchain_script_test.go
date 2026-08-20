package preflight

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-486: CheckGoToolchain is only reachable once Go has already compiled, so
// under a real GOROOT mismatch it never runs — the compiler fails first. The
// pre-compile guard is scripts/check-go-toolchain.zsh, and it must be exercised
// as the real script. Faking the probe here would repeat the exact mistake this
// ticket repairs.
const goToolchainScript = "../../scripts/check-go-toolchain.zsh"

func runGoToolchainScript(t *testing.T, goroot string, setGOROOT bool) (string, error) {
	t.Helper()
	cmd := exec.Command(goToolchainScript)
	env := withoutEnv(os.Environ(), "GOROOT")
	if setGOROOT {
		env = append(env, "GOROOT="+goroot)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestGoToolchainScript_RejectsMismatchedGOROOT(t *testing.T) {
	// A directory that is definitively not the PATH-resolved GOROOT.
	bogus := t.TempDir()
	// Real GOROOT/VERSION carries a trailing "time <stamp>" line; reproduce it
	// so the diagnostic is asserted against the actual on-disk format.
	if err := os.WriteFile(filepath.Join(bogus, "VERSION"), []byte("go1.0.0\ntime 2026-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatalf("seed VERSION: %v", err)
	}

	resolved, err := runGoToolchainCommand(withoutEnv(os.Environ(), "GOROOT"), "env", "GOROOT")
	if err != nil {
		t.Fatalf("resolve PATH GOROOT: %v (%s)", err, resolved)
	}

	out, err := runGoToolchainScript(t, bogus, true)
	if err == nil {
		t.Fatalf("expected non-zero exit for mismatched GOROOT, got success; output: %s", out)
	}
	// Both roots AND both versions: a diagnostic that degrades to "(g)" or
	// "version unavailable" still exits non-zero, so exit status alone would
	// not catch it.
	if strings.Contains(out, "version unavailable") {
		t.Errorf("diagnostic degraded to \"version unavailable\"; output: %s", out)
	}
	for _, want := range []string{bogus, "go1.0.0", strings.TrimSpace(resolved), "unset GOROOT"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic missing %q; output: %s", want, out)
		}
	}
}

func TestGoToolchainScript_AllowsUnsetGOROOT(t *testing.T) {
	if out, err := runGoToolchainScript(t, "", false); err != nil {
		t.Fatalf("unset GOROOT must pass, got %v; output: %s", err, out)
	}
}

func TestGoToolchainScript_AllowsMatchingGOROOT(t *testing.T) {
	resolved, err := runGoToolchainCommand(withoutEnv(os.Environ(), "GOROOT"), "env", "GOROOT")
	if err != nil {
		t.Fatalf("resolve PATH GOROOT: %v (%s)", err, resolved)
	}
	if out, err := runGoToolchainScript(t, strings.TrimSpace(resolved), true); err != nil {
		t.Fatalf("matching GOROOT must pass, got %v; output: %s", err, out)
	}
}
