package signerboundary

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Live three-UID acceptance via Docker (supported host path when OS users are
// not provisioned on the Mac host). Proves real B UNAUTHORIZED_PEER, real R
// sign, replay deny, attach deny — no TestPeerUIDOverride.
//
// Skip when docker unavailable or HERD_FAC169_SKIP_DOCKER=1.

func TestLiveThreeUID_Docker(t *testing.T) {
	if os.Getenv("HERD_FAC169_SKIP_DOCKER") == "1" {
		t.Skip("HERD_FAC169_SKIP_DOCKER=1")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not usable")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	modRoot := filepath.Clean(filepath.Join(wd, "../.."))
	if _, err := os.Stat(filepath.Join(modRoot, "go.mod")); err != nil {
		t.Fatalf("go.mod not found from %s", modRoot)
	}

	// Use official image; allow toolchain download for go.mod 1.25+.
	img := goDockerImage
	script := `
set -eu
export GOTOOLCHAIN=auto
export PATH="/usr/local/go/bin:$PATH"
cd /src
groupadd -g 27000 herd169ipc || true
id herd169s >/dev/null 2>&1 || useradd -u 27001 -g 27000 -M -s /usr/sbin/nologin herd169s
id herd169r >/dev/null 2>&1 || useradd -u 27002 -g 27000 -M -s /usr/sbin/nologin herd169r
id herd169b >/dev/null 2>&1 || useradd -u 27003 -g 27000 -M -s /usr/sbin/nologin herd169b
usermod -aG herd169ipc herd169s || true
usermod -aG herd169ipc herd169r || true
usermod -aG herd169ipc herd169b || true
export HERD_FAC169_IN_DOCKER=1
export HERD_SIGNER_UID=27001
export HERD_REQUESTER_UID=27002
export HERD_BUILDER_UID=27003
export HERD_SIGNER_SOCK_GID=27000
go test ./pkg/signerboundary/ -count=1 -timeout 180s -run 'TestLiveThreeUID_InContainer' -v
`

	args := append([]string{"run", "--rm",
		"--user", "0:0",
		"-e", "GOTOOLCHAIN=auto",
		"-v", modRoot + ":/src",
		"-w", "/src"}, dockerGoCacheArgs(t)...)
	args = append(args, img, "bash", "-ec", script)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		if strings.Contains(msg, "Cannot connect") || strings.Contains(msg, "pull access denied") {
			t.Skipf("docker environment incomplete: %v\n%s", err, msg)
		}
		t.Fatalf("live docker e2e failed: %v\n%s", err, msg)
	}
	if !strings.Contains(string(out), "LIVE_OK") && !strings.Contains(string(out), "PASS") {
		t.Fatalf("unexpected docker output:\n%s", out)
	}
}
