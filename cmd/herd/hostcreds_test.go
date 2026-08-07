package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/security"
)

func TestHostCredsCLI_Selftest(t *testing.T) {
	bin := buildHerdForHostCreds(t)
	cmd := exec.Command(bin, "hostcreds", "selftest")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("selftest exit=%v out=%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("want PASS, got %s", out)
	}
	if strings.Contains(string(out), "selftest-secret") {
		t.Fatal("secret leaked in selftest output")
	}
}

func TestHostCredsCLI_DiagnoseBlockedWithoutKeys(t *testing.T) {
	bin := buildHerdForHostCreds(t)
	cmd := exec.Command(bin, "hostcreds", "diagnose", "--kind", "grok")
	cmd.Env = filterEnv(os.Environ(), "XAI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "HERD_HOST_CREDS")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit without keys; out=%s", out)
	}
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 {
		t.Fatalf("want exit 2 BLOCKED, got %v out=%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "BLOCKED") {
		t.Fatalf("want BLOCKED packet: %s", s)
	}
	if strings.Contains(s, "sk-") {
		t.Fatal("secret shape in diagnose output")
	}
}

func TestHostCredsCLI_OpenCodeRejected(t *testing.T) {
	bin := buildHerdForHostCreds(t)
	cmd := exec.Command(bin, "hostcreds", "diagnose", "--kind", "opencode")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected reject")
	}
	// diagnose --kind opencode reports BLOCKED via DiagnoseKindAuthReadiness
	// (class config) or an explicit out-of-scope reject. This was a t.Logf, so
	// the wording was unguarded and the branch could never fail the test.
	if !strings.Contains(string(out), "out of scope") && !strings.Contains(string(out), "BLOCKED") {
		t.Fatalf("opencode rejection must say BLOCKED or out of scope; got:\n%s", out)
	}
	// The rejection must not leak credential material.
	if security.RedactSecrets(string(out)) != string(out) {
		t.Fatalf("opencode rejection carries secret-shaped material:\n%s", out)
	}
}

func buildHerdForHostCreds(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herd")
	// go test ./cmd/herd runs with package dir as cwd.
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build herd: %v\n%s", err, out)
	}
	return bin
}

func filterEnv(env []string, drop ...string) []string {
	deny := map[string]bool{}
	for _, d := range drop {
		deny[d] = true
	}
	var out []string
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			continue
		}
		k := e[:i]
		if deny[k] {
			continue
		}
		out = append(out, e)
	}
	// Ensure keys are empty, not just absent (some loaders treat missing differently).
	for _, d := range drop {
		out = append(out, d+"=")
	}
	return out
}
