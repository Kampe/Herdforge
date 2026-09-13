package resources

// FAC-829: give the OS probes a finite capture and a finite join.
//
// runProbeCtx used exec.CommandContext(...).Output(), which has two unbounded
// edges that a resident sampler cannot carry:
//
//  1. Output() accumulates stdout with no ceiling. A probe that printed without
//     stopping would be copied into memory without limit.
//  2. CommandContext kills the DIRECT child when the context expires, but Wait
//     still blocks on the stdout copy. A descendant that inherited the pipe
//     keeps it open, so the call can outlive its own deadline.
//
// Both are fixed here with mechanisms that actually exist: a capped writer, and
// Cmd.WaitDelay, which bounds how long Wait will wait for the I/O copy after
// the process is gone and then closes the descriptors itself.
//
// What is deliberately NOT claimed: this cannot forcibly stop an arbitrary
// non-cooperative process, and it signals nothing it does not own. It bounds
// what THIS process spends -- memory and waiting -- and returns an error. A
// descendant that ignores its parent's death is the operator's to deal with;
// the observer's obligation is to stop paying for it, and to refuse rather than
// report a number it does not have.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const (
	// maxProbeOutputBytes caps a single probe's stdout. The largest real
	// output here is `sysctl -n vm.swapusage`, well under 128 bytes; 64KiB is
	// four orders of magnitude of headroom and still finite.
	maxProbeOutputBytes = 64 << 10
	// maxProbeStderrBytes keeps a failing probe's diagnostics useful without
	// letting a chatty failure become the memory cost.
	maxProbeStderrBytes = 4 << 10
	// probeWaitDelay bounds the join after the process is gone. It is short:
	// by the time it applies, the probe has already failed its deadline, and
	// the only question left is how long this process keeps waiting on a pipe
	// somebody else is holding.
	probeWaitDelay = 250 * time.Millisecond
)

// errProbeOutputTruncated is returned when a probe exceeded its output cap. It
// is an ERROR, never a truncated value: a partially-read sysctl could parse
// into a plausible number that was never the real reading.
var errProbeOutputTruncated = errors.New("probe output exceeded its byte cap; refusing to parse a truncated reading")

// cappedBuffer accepts up to limit bytes and records that it overflowed. It
// never grows past limit, so a runaway writer cannot consume memory through it.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.overflow {
		// Report the bytes as accepted so the copier does not treat the cap as
		// a write error and abort differently from the way we want; the
		// overflow flag is what the caller acts on.
		return len(p), nil
	}
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.overflow = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.overflow = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// boundedProbeOutput runs one native OS probe with a finite output cap, a
// finite post-mortem join, and cancellation.
//
// The context governs the process; WaitDelay governs how long this process will
// wait for the pipe after that. Both are finite, which is the whole point.
func boundedProbeOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout := &cappedBuffer{limit: maxProbeOutputBytes}
	stderr := &cappedBuffer{limit: maxProbeStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Bound the join. Without this, Wait blocks until every descendant holding
	// the stdout pipe closes it, which is unbounded by construction.
	cmd.WaitDelay = probeWaitDelay

	err := cmd.Run()
	if stdout.overflow {
		return "", fmt.Errorf("%s: %w (cap %d bytes)", name, errProbeOutputTruncated, maxProbeOutputBytes)
	}
	if err != nil {
		if msg := stderr.String(); msg != "" {
			return "", fmt.Errorf("%w: %s", err, firstLine(msg))
		}
		return "", err
	}
	return stdout.String(), nil
}

// firstLine keeps a diagnostic to one line so an error string stays bounded
// even when the cap did not trip.
func firstLine(s string) string {
	if i := bytes.IndexByte([]byte(s), '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
