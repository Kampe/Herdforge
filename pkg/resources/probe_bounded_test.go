package resources

// FAC-829: the probe runner's two bounds. These do start short-lived
// subprocesses, so they are remote-CI work like every other dynamic test here;
// each one is designed to finish well under a second.

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipWithoutPOSIXShell(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "netbsd", "openbsd":
	default:
		t.Skipf("no POSIX shell to drive probe bounds on %s", runtime.GOOS)
	}
}

// TestBoundedProbeOutputRefusesOversizedOutput proves a runaway probe is an
// ERROR rather than a truncated value. A partially-read sysctl could parse into
// a plausible number that was never the real reading, which is precisely the
// fail-open shape this package exists to remove.
func TestBoundedProbeOutputRefusesOversizedOutput(t *testing.T) {
	skipWithoutPOSIXShell(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := boundedProbeOutput(ctx, "sh", "-c", "yes a | head -c 200000")
	if err == nil {
		t.Fatalf("a probe emitting 200000 bytes was accepted; the %d-byte cap is not enforced", maxProbeOutputBytes)
	}
	if !errors.Is(err, errProbeOutputTruncated) {
		t.Fatalf("oversized output produced %v, expected it to wrap errProbeOutputTruncated", err)
	}
}

// TestBoundedProbeOutputAcceptsNormalOutput keeps the cap test honest: a real
// probe-sized output must still come back intact.
func TestBoundedProbeOutputAcceptsNormalOutput(t *testing.T) {
	skipWithoutPOSIXShell(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := boundedProbeOutput(ctx, "sh", "-c", "printf 'vm.loadavg: { 1.23 2.34 3.45 }'")
	if err != nil {
		t.Fatalf("an ordinary probe failed: %v", err)
	}
	if !strings.Contains(out, "1.23") {
		t.Fatalf("probe output did not round-trip: %q", out)
	}
}

// TestBoundedProbeOutputReturnsPromptlyOnCancellation proves the call does not
// outlive its context by a meaningful margin.
//
// It deliberately does NOT claim the descendant is dead: WaitDelay bounds what
// THIS process waits for, and a process that ignores its parent's death is not
// something a sampler can or should forcibly stop.
func TestBoundedProbeOutputReturnsPromptlyOnCancellation(t *testing.T) {
	skipWithoutPOSIXShell(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := boundedProbeOutput(ctx, "sh", "-c", "sleep 30")
	elapsed := time.Since(started)

	if err == nil {
		t.Fatalf("a probe that outran its context returned success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the call took %s to return after a 100ms deadline; the join is not bounded", elapsed)
	}
}

// TestRunProbeCtxStillReportsTimeoutsAsErrors pins the error contract the
// admission path depends on: a timeout is an error, never an empty string a
// parser could read as a healthy default.
func TestRunProbeCtxStillReportsTimeoutsAsErrors(t *testing.T) {
	skipWithoutPOSIXShell(t)
	out, err := runProbeCtx(context.Background(), 100*time.Millisecond, "sh", "-c", "sleep 30")
	if err == nil {
		t.Fatalf("a timed-out probe returned no error")
	}
	if out != "" {
		t.Fatalf("a timed-out probe returned output %q; a parser could read that as a reading", out)
	}
	if !strings.Contains(err.Error(), "timed out or was cancelled") {
		t.Fatalf("timeout error text changed to %q; admission callers match on it", err)
	}
}
