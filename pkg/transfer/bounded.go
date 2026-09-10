package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Process and output bounds. Every git/lsof child of a reclaim pass runs
// under a deadline with a capped output buffer; an overflowing or timed-out
// child is an unknown, never a pass.
const (
	defaultGitTimeout  = 30 * time.Second
	gitOutputCapBytes  = int64(8) << 20
	lsofOutputCapBytes = int64(1) << 20
	// waitDelay bounds pipe draining after a deadline kill so a stuck child
	// cannot wedge Wait forever.
	childWaitDelay = 2 * time.Second
)

// defaultLsofTimeout is a var so hermetic tests can bound it tightly.
var defaultLsofTimeout = 10 * time.Second

// boundedBuffer caps retained output; overflow is flagged, not absorbed.
type boundedBuffer struct {
	limit    int64
	buf      bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(b.buf.Len())+int64(len(p)) > b.limit {
		b.overflow = true
		if room := b.limit - int64(b.buf.Len()); room > 0 {
			_, _ = b.buf.Write(p[:int(room)])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// runBounded executes one child under a deadline and output cap. Timeout,
// overflow, or unexpected exit surfaces as an error the caller must treat as
// unknown/retain, never as proof.
func runBounded(ctx context.Context, timeout time.Duration, outCap int64, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := gitCommand(cctx, dir, args...)
	return runBoundedCmd(cmd, cctx, &boundedBuffer{limit: outCap}, args[0], timeout, outCap)
}

// runBoundedCmd wraps an existing command with the shared deadline/overflow
// contract.
func runBoundedCmd(cmd *exec.Cmd, cctx context.Context, out *boundedBuffer, name string, timeout time.Duration, outCap int64) (string, error) {
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = childWaitDelay
	runErr := cmd.Run()
	if cctx.Err() != nil && !errors.Is(cctx.Err(), context.Canceled) {
		return out.String(), fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if out.overflow {
		return out.String(), fmt.Errorf("%s output exceeded %d bytes", name, outCap)
	}
	if runErr != nil {
		return out.String(), fmt.Errorf("%s: %w", name, runErr)
	}
	return out.String(), nil
}

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	return cmd
}

func execLookPath(name string) (string, error) { return exec.LookPath(name) }
