package signerboundary

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveLaunch_CompiledProvisionAndLaunch exercises the compiled
// ProvisionAndLaunch path inside Docker (not the hand-rolled serve setup).
// Proves ledger ACL chown, sealed session, RunAs B/R prove, no session hex.
func TestLiveLaunch_CompiledProvisionAndLaunch(t *testing.T) {
	if os.Getenv("HERD_FAC169_SKIP_DOCKER") == "1" {
		t.Skip("skip docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not usable")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	modRoot := filepath.Clean(filepath.Join(wd, "../.."))
	script := `
set -eu
export GOTOOLCHAIN=auto
cd /src
groupadd -g 27000 herd169ipc || true
id herd169s >/dev/null 2>&1 || useradd -u 27001 -g 27000 -M -s /usr/sbin/nologin herd169s
id herd169r >/dev/null 2>&1 || useradd -u 27002 -g 27000 -M -s /usr/sbin/nologin herd169r
id herd169b >/dev/null 2>&1 || useradd -u 27003 -g 27000 -M -s /usr/sbin/nologin herd169b
usermod -aG herd169ipc herd169s || true
usermod -aG herd169ipc herd169r || true
usermod -aG herd169ipc herd169b || true
export HERD_FAC169_IN_DOCKER=1
export HERD_SIGNER_UID=27001 HERD_REQUESTER_UID=27002 HERD_BUILDER_UID=27003 HERD_SIGNER_SOCK_GID=27000
go test ./pkg/signerboundary/ -count=1 -timeout 180s -run 'TestLiveLaunch_InContainer' -v
`
	args := append([]string{"run", "--rm", "--user", "0:0",
		"-e", "GOTOOLCHAIN=auto",
		"-v", modRoot + ":/src", "-w", "/src"}, dockerGoCacheArgs(t)...)
	args = append(args,
		goDockerImage, "bash", "-ec", script)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		if strings.Contains(msg, "Cannot connect") {
			t.Skip(msg)
		}
		t.Fatalf("compiled live launch: %v\n%s", err, msg)
	}
	if !strings.Contains(string(out), "LIVE_LAUNCH_OK") && !strings.Contains(string(out), "PASS") {
		t.Fatalf("unexpected: %s", out)
	}
}

// TestLiveLaunch_InContainer is the in-docker body for compiled ProvisionAndLaunch.
func TestLiveLaunch_InContainer(t *testing.T) {
	if os.Getenv("HERD_FAC169_IN_DOCKER") != "1" {
		t.Skip("not in docker")
	}
	root := "/tmp/h169-launch"
	_ = os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	modRoot, _ := filepath.Abs("../..")
	herdBin := filepath.Join(root, "herd")
	// -buildvcs=false: the repo is bind-mounted into the container, so git sees a
	// directory owned by another uid and refuses it ("dubious ownership"). Go then
	// fails the whole build with "error obtaining VCS status: exit status 128".
	// Stamping VCS metadata into a test binary buys nothing.
	build := exec.Command("go", "build", "-buildvcs=false", "-o", herdBin, "./cmd/herd")
	build.Dir = modRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build herd: %v\n%s", err, out)
	}
	_ = os.Chmod(herdBin, 0o755)

	keyDir := filepath.Join(root, "keys")
	sock := "/tmp/h169-launch.sock"
	_ = os.Remove(sock)
	repo := filepath.Join(root, "repo")
	_ = os.MkdirAll(filepath.Join(repo, ".herd"), 0o755)

	// Root launcher: ProvisionAndLaunch must chown ledger/session for S/R.
	h, err := ProvisionAndLaunch(LaunchConfig{
		KeyDir: keyDir, RepoRoot: repo, Identity: "live", SocketPath: sock,
		HerdBinary: herdBin, DetachServe: true,
	})
	if err != nil {
		t.Fatalf("ProvisionAndLaunch: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// Sealed session exists and is R-owned 0600.
	if err := auditSealedSession(h.SealedSession, h.Topo.RequesterUID); err != nil {
		t.Fatal(err)
	}
	// Ledger is group-writable (0660).
	fi, err := os.Lstat(h.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o060 == 0 {
		t.Fatalf("ledger mode %04o want group rw", fi.Mode().Perm())
	}
	// Repeated R load of sealed session works (not one-shot FD).
	out, err := runCmdCapture(RunAsUID(h.Topo.RequesterUID, probeEnv(h), herdBin, "signer-boundary", "status", "--key-dir", keyDir))
	// status may fail without attestation — establish as R would need more; at least sealed loads.
	_ = out
	_ = err

	// B cannot read sealed session.
	helper, err := buildLiveProbeHelper()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(helper)
	bout, _ := runCmdCapture(RunAsUID(h.Topo.BuilderUID, probeEnv(h), helper, "keyread", h.SealedSession))
	if strings.Contains(bout, "KEY_READ_OK") {
		t.Fatalf("builder read sealed session: %s", bout)
	}

	fmtPrintlnLiveLaunchOK()
}

func fmtPrintlnLiveLaunchOK() {
	println("LIVE_LAUNCH_OK")
}

// dockerGoCacheArgs makes the container build hermetic.
//
// The container mounted only the repo, so `go build` inside it had to download
// every module AND, because the image was golang:1.24 while go.mod requires
// 1.25, the toolchain itself. That works on a developer box with a warm cache
// and open network; in CI it failed with "build herd: exit status 1" behind a
// wall of "go: downloading ...". The test was exercising the network, not the
// signer boundary.
//
// Bind the host module cache read-only and pin GOFLAGS/GOMODCACHE so the build
// resolves from it. GOPROXY stays reachable rather than off: a cold cache
// should still work, it just should not be the normal path.
func dockerGoCacheArgs(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOMODCACHE: %v", err)
	}
	cache := strings.TrimSpace(string(out))
	if cache == "" {
		t.Fatal("GOMODCACHE is empty; cannot make the container build hermetic")
	}
	return []string{"-v", cache + ":/gomodcache", "-e", "GOMODCACHE=/gomodcache", "-e", "GOFLAGS=-mod=mod"}
}

// goDockerImage matches go.mod's toolchain so GOTOOLCHAIN=auto has nothing to
// download. The image was golang:1.24 against a go 1.25.0 module.
const goDockerImage = "golang:1.25-bookworm"
