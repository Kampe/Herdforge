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

// FAC-614: this was TestHostCredsCLI_DiagnoseBlockedWithoutKeys, and it asserted
// that `hostcreds diagnose --kind grok` exits 2 BLOCKED when the API-key
// environment variables are absent.
//
// FAC-587 deliberately removed that behaviour. Every kind this fleet launches --
// claude, grok, codex, agy -- is harness-authenticated: it holds its own CLI
// login session and never presents a brokered host credential, so the absence of
// XAI_API_KEY is not evidence of anything. The old test filtered env vars and
// read that absence as "no credentials", which is the same category error
// FAC-587 fixed in the product code, left behind in its test.
//
// It has failed on origin/main ever since, on any host and in CI, and its name
// asserted a contract the code had stopped honouring.
//
// What is actually contractual now: a harness-authenticated kind reports OK via
// native_auth without env keys, and the real blocker -- a harness that is present
// but logged OUT -- is still a blocker. The second half is the safety property
// worth keeping, so it is asserted rather than dropped.
func TestHostCredsCLI_HarnessKindIsNativeAuthWithoutEnvKeys(t *testing.T) {
	bin := buildHerdForHostCreds(t)
	for _, kind := range []string{"grok", "opencode"} {
		cmd := exec.Command(bin, "hostcreds", "diagnose", "--kind", kind)
		cmd.Env = filterEnv(os.Environ(), "XAI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "HERD_HOST_CREDS")
		out, err := cmd.CombinedOutput()
		s := string(out)

		// A logged-out harness is a genuine blocker and this host may legitimately be
		// in that state, so accept it explicitly rather than letting it fail as a
		// surprise.
		if err != nil {
			if !strings.Contains(s, "BLOCKED") {
				t.Fatalf("non-zero exit for %s must carry a BLOCKED packet: %s", kind, s)
			}
			if !strings.Contains(s, "logged") && !strings.Contains(s, "login") {
				t.Fatalf("the only legitimate blocker for a harness kind is a logged-out harness: %s", s)
			}
		} else if !strings.Contains(s, "native_auth") {
			t.Fatalf("a harness-authenticated kind (%s) without env keys must report native_auth, got: %s", kind, s)
		}

		if strings.Contains(s, "sk-") {
			t.Fatalf("secret shape in diagnose output for %s", kind)
		}
	}
}

func TestHostCredsCLI_UnknownKindRejected(t *testing.T) {
	bin := buildHerdForHostCreds(t)
	cmd := exec.Command(bin, "hostcreds", "diagnose", "--kind", "unknown-kind")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected reject")
	}
	if !strings.Contains(string(out), "BLOCKED") {
		t.Fatalf("unknown kind rejection must say BLOCKED; got:\n%s", out)
	}
	// The rejection must not leak credential material.
	if security.RedactSecrets(string(out)) != string(out) {
		t.Fatalf("unknown kind rejection carries secret-shaped material:\n%s", out)
	}
}

func TestHostCredsCLI_UsageIncludesOpenCodeDiagnose(t *testing.T) {
	bin := buildHerdForHostCreds(t)
	cmd := exec.Command(bin, "hostcreds", "help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hostcreds help failed: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "opencode") {
		t.Errorf("hostcreds help must mention opencode under diagnose: %s", s)
	}
	if !strings.Contains(s, "raw HostCreds session/live broker remains unsupported for OpenCode") {
		t.Errorf("hostcreds help must clarify raw broker unsupported for OpenCode: %s", s)
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
