package security

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretWritePathsAreGitIgnored proves the repo refuses to stage the
// control-plane material a production dispatch writes into the working tree.
//
// The paths come from the real path helpers, not string literals: moving a
// secret to a new location fails this test until .gitignore follows.
//
// Failure this guards: WriteControlMACSecret drops the HMAC control secret at
// <repoRoot>/.herd/control/mac.secret and SeedCoordinatorHostCreds writes
// <repoRoot>/.herd/brokers/<tab>.ctrl.json holding control_token plus
// host_creds ("Bearer $ANTHROPIC_API_KEY"). Unignored, any `git add -A` — which
// the fleet's own agents run — stages live API keys.
func TestSecretWritePathsAreGitIgnored(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	out, err := exec.Command(git, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not a git work tree")
	}
	root := strings.TrimSpace(string(out))

	// Every path a production launch writes under the repo working tree.
	// sharedRoot is "" so the helpers yield repo-relative paths.
	paths := []string{
		ControlMACSecretPath(""),                    // HMAC control secret
		BrokerControlPath("", "wF:t1"),              // control_token + host_creds
		BrokerStatePath("", "wF:t1"),                // broker endpoint state
		filepath.Join(".herd", "control", "sealed"), // sealed control envelopes
		filepath.Join(".herd", "control", "issuer-seq.json"),
		filepath.Join(".herd", "control", "sessions"),
		filepath.Join(".herd", "readiness", "fleet.json"),
		filepath.Join(".herd", "mail.jsonl"),
		filepath.Join(".herd", "control-mail.jsonl"),
	}

	for _, p := range paths {
		p := filepath.Clean(p)
		if filepath.IsAbs(p) {
			t.Fatalf("path helper returned an absolute path for empty sharedRoot: %s", p)
		}
		// --no-index: assert the rule, not whether the file happens to exist.
		cmd := exec.Command(git, "check-ignore", "--no-index", "-q", p)
		cmd.Dir = root
		if err := cmd.Run(); err != nil {
			t.Errorf("%s is NOT git-ignored — a `git add -A` would stage it", p)
		}
	}
}
