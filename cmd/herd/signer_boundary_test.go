package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignerBoundaryCLI_EstablishBlocksWithoutProvision(t *testing.T) {
	bin := buildHerdTestBinary(t)
	// Clear ambient provision env for this process tree.
	cmd := exec.Command(bin, "signer-boundary", "establish",
		"--repo", t.TempDir(),
		"--key-dir", t.TempDir(),
		"--identity", "x")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("establish without separate-uid must fail, out=%s", out)
	}
	// Real assertion: a generic non-zero exit (e.g. a usage error) must not pass.
	if !strings.Contains(string(out), "BLOCKED") || !strings.Contains(string(out), "HERD_SIGNER_UID") {
		t.Fatalf("establish must fail closed on unprovisioned OS authority, got: %s", out)
	}
}

func TestSignerBoundaryCLI_StatusFailClosed(t *testing.T) {
	bin := buildHerdTestBinary(t)
	cmd := exec.Command(bin, "signer-boundary", "status", "--key-dir", t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("status must fail closed, out=%s", out)
	}
	if !strings.Contains(string(out), "NOT READY") {
		t.Fatalf("status must report NOT READY, got: %s", out)
	}
	if !strings.Contains(string(out), "signer boundary not established") ||
		!strings.Contains(string(out), "isolation.json") ||
		!strings.Contains(string(out), "herd signer-boundary establish") {
		t.Fatalf("status must diagnose missing attestation and remediation, got: %s", out)
	}
}

func TestSignerBoundaryCLI_Help(t *testing.T) {
	bin := buildHerdTestBinary(t)
	cmd := exec.Command(bin, "signer-boundary", "help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// help exits 0
		t.Log(err)
	}
	if !strings.Contains(string(out), "sign-verdict") || !strings.Contains(string(out), "session-key") {
		t.Fatalf("help should document sign-verdict / session-key, got %s", out)
	}
	if !strings.Contains(string(out), "admission-ledger") {
		t.Fatalf("help must document durable admission-ledger: %s", out)
	}
	if strings.Contains(string(out), "HERD_SESSION_KEY_HEX") {
		t.Fatal("help must not advertise session hex on stdout")
	}
}

func TestSignerBoundaryCLI_ServeRequiresAdmissionLedger(t *testing.T) {
	bin := buildHerdTestBinary(t)
	// HERD_SIGNER_UID must equal this uid, and a session key must be supplied,
	// or serve exits earlier and never reaches the ledger requirement (the
	// previous version of this test asserted nothing about the ledger).
	me := os.Getuid()
	dir := t.TempDir()
	cmd := exec.Command(bin, "signer-boundary", "serve",
		"--key", filepath.Join(dir, "k"),
		"--socket", filepath.Join(dir, "s.sock"), "--session-key-stdin")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		fmt.Sprintf("HERD_SIGNER_UID=%d", me),
		fmt.Sprintf("HERD_REQUESTER_UID=%d", me+100001),
		fmt.Sprintf("HERD_BUILDER_UID=%d", me+200002),
		fmt.Sprintf("HERD_SIGNER_SOCK_GID=%d", os.Getgid()),
	}
	cmd.Stdin = strings.NewReader(strings.Repeat("ab", 32) + "\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("serve without ledger must fail: %s", out)
	}
	// Must fail specifically on the durable admission channel, not the uid gate.
	if strings.Contains(string(out), "must run as HERD_SIGNER_UID") {
		t.Fatalf("test did not reach the ledger gate (stopped at uid check): %s", out)
	}
	if !strings.Contains(string(out), "admission-ledger") {
		t.Fatalf("serve must require the durable admission ledger, got: %s", out)
	}
}

// A discarded flag-parse error would leave --wipe-key false while the command
// still reports success (CLAUDE.md invariant #2: fail closed).
func TestSignerBoundaryCLI_UnknownFlagFailsClosed(t *testing.T) {
	bin := buildHerdTestBinary(t)
	cmd := exec.Command(bin, "signer-boundary", "revoke",
		"--key-dir", t.TempDir(), "--wipe-ke")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unknown flag must exit non-zero, got: %s", out)
	}
	if strings.Contains(string(out), "revoked") {
		t.Fatalf("must not report success on a misspelled flag: %s", out)
	}
}

func TestSignerBoundaryCLI_NoSignOracle(t *testing.T) {
	bin := buildHerdTestBinary(t)
	cmd := exec.Command(bin, "signer-boundary", "sign-bytes")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("sign-bytes must be rejected")
	}
	if !strings.Contains(string(out), "sign-verdict") {
		t.Fatalf("want sign-verdict guidance: %s", out)
	}
}

func buildHerdTestBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herd-test")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = wd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}
