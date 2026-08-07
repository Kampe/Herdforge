package security

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The platform gate must not claim support it cannot deliver. Review finding 6
// showed the gate is a pure string switch, so it happily passed on a platform
// where the binary cannot exist.
//
// This cross-compiles pkg/security for each GOOS the gate declares supported.
// A platform in the supported list that does not build is a lie in the gate.
//
// Scope note: the converse (every unsupported GOOS builds and fails closed at
// runtime) is NOT achievable inside FAC-170. pkg/security depends on
// pkg/harness → pkg/agentpolicy, which uses syscall.Flock/LOCK_EX and is
// unix-only. Making the package build on Windows means porting a shared
// package this ticket does not own, so AC 8 stays partially met and is
// reported as such rather than papered over.
func TestPlatformGate_SupportedListActuallyBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the package for several GOOS values")
	}
	supported := []string{"darwin", "linux", "freebsd", "openbsd", "netbsd"}
	for _, goos := range supported {
		if err := platformSupportsHostCredsBrokerFor(goos); err != nil {
			t.Fatalf("%s is in the supported list but the gate denies it: %v", goos, err)
		}
	}
	for _, goos := range supported {
		if goos == runtime.GOOS {
			continue // already proven by this test binary existing
		}
		cmd := exec.Command("go", "build", "./")
		cmd.Env = append(cmd.Environ(), "GOOS="+goos, "GOARCH=amd64")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("gate claims %s is supported but the package does not build there:\n%s",
				goos, strings.TrimSpace(string(out)))
		}
	}
}

// An unsupported GOOS must produce a typed BLOCKED reason with the platform in
// a stable code, and readiness must short-circuit before any credential probe.
// Deleting the default-deny arm of the switch fails this.
func TestPlatformGate_UnsupportedFailsClosed(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "freebsd", "openbsd", "netbsd"} {
		if err := platformSupportsHostCredsBrokerFor(goos); err != nil {
			t.Fatalf("%s must be supported: %v", goos, err)
		}
	}
	for _, goos := range []string{"windows", "js", "plan9", "solaris", ""} {
		err := platformSupportsHostCredsBrokerFor(goos)
		be, ok := err.(*BlockedError)
		if !ok {
			t.Fatalf("%s: want *BlockedError, got %T (%v)", goos, err, err)
		}
		if be.Reason != BlockUnsupportedPlat {
			t.Fatalf("%s: reason %q want %q", goos, be.Reason, BlockUnsupportedPlat)
		}
		if be.Code != "goos:"+goos {
			t.Fatalf("%s: code %q", goos, be.Code)
		}
	}
}

func TestDiagnose_UnsupportedPlatformBlocked(t *testing.T) {
	prev := hostCredsGOOS
	hostCredsGOOS = "windows"
	defer func() { hostCredsGOOS = prev }()

	if ok, reason := PlatformHostCredsStatus(); ok || reason != "platform_unsupported:goos:windows" {
		t.Fatalf("status ok=%v reason=%q", ok, reason)
	}
	d := DiagnoseKindAuthReadinessWith("grok", nil)
	if d.Brokerable {
		t.Fatal("unsupported platform must not be brokerable")
	}
	if d.Class != KindAuthPlatform || d.ReasonCode != "platform_unsupported" {
		t.Fatalf("class=%s reason=%s", d.Class, d.ReasonCode)
	}
	if len(d.HostCredsPresent) != 0 || d.AuthorityClass != "none" {
		t.Fatalf("credential probing must not run: %+v", d)
	}
	line := FormatKindAuthBlocker(d)
	if !strings.Contains(line, "BLOCKED") {
		t.Fatalf("blocker line not typed: %q", line)
	}
	if RedactSecrets(line) != line {
		t.Fatalf("blocker line carries secret-shaped material: %q", line)
	}
}
