package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/procsignal"
)

// MaxCLIStderrBytes caps captured CLI stderr so credential-bearing or
// voluminous error dumps cannot flood logs or status projections.
const MaxCLIStderrBytes = 4096

// CLIResult is the bounded output of one CLI invocation.
type CLIResult struct {
	Stdout []byte
	Stderr []byte
}

// RunCLI executes name+args under ctx, placing the child in its own process
// group so cancel/timeout kills the entire tree (not just the direct child).
// On deadline or cancel it returns a *TimeoutError; stdout/stderr are still
// populated when available.
//
// stderr is truncated to MaxCLIStderrBytes. Never log the raw args slice when
// it may contain tokens — callers own redaction of mutation payloads.
func RunCLI(ctx context.Context, name string, args ...string) (*CLIResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Own process group so Cancel can SIGKILL the whole tree (shell stubs,
	// grandchildren). Without Setpgid, CommandContext only signals the
	// direct child and hung descendants survive (FAC-150 hang class).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// FAC-174: claim the live *os.Process as an owned group leader, then
		// cancel through the opaque handle (raw int cancel is not exported).
		return procsignal.CancelSpawnedProcess(cmd.Process)
	}
	// Bound pipe drain after kill so CombinedOutput-style waits cannot stall
	// on a grandchild holding stdout open.
	cmd.WaitDelay = 100 * time.Millisecond

	err := cmd.Run()
	res := &CLIResult{
		Stdout: stdout.Bytes(),
		Stderr: truncateBytes(stderr.Bytes(), MaxCLIStderrBytes),
	}
	if err == nil {
		return res, nil
	}

	// Prefer typed timeout when the context died; still wrap other exits.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, &TimeoutError{
			Cause: ctxErr,
		}
	}
	// exec may surface ExitError after kill even when ctx is already done on
	// some kernels; check both.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return res, &TimeoutError{Cause: err}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = err.Error()
		}
		return res, fmt.Errorf("%s: %s", name, msg)
	}
	return res, fmt.Errorf("%s: %w", name, err)
}

// RunCLIOutput is a convenience wrapper returning stdout only.

// RunCLIEnv is RunCLI with optional extra environment entries (KEY=value).
// Used to transport HERD_FENCE / HERD_OP into production Kaneo CLI mutates
// so fence meta is not dropped when use_cli: true (FAC-147).
func RunCLIEnv(ctx context.Context, extraEnv []string, name string, args ...string) (*CLIResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return procsignal.CancelSpawnedProcess(cmd.Process)
	}
	cmd.WaitDelay = 100 * time.Millisecond

	err := cmd.Run()
	res := &CLIResult{
		Stdout: stdout.Bytes(),
		Stderr: truncateBytes(stderr.Bytes(), MaxCLIStderrBytes),
	}
	if err == nil {
		return res, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, &TimeoutError{Cause: ctxErr}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return res, &TimeoutError{Cause: err}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = err.Error()
		}
		return res, fmt.Errorf("%s: %s", name, msg)
	}
	return res, err
}

var kaneoRunCLIEnv = RunCLIEnv

func RunCLIOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	res, err := RunCLI(ctx, name, args...)
	if res == nil {
		return nil, err
	}
	if err != nil {
		return res.Stdout, err
	}
	return res.Stdout, nil
}

func truncateBytes(b []byte, n int) []byte {
	if n <= 0 || len(b) <= n {
		// Return a copy so callers cannot mutate the buffer backing store.
		out := make([]byte, len(b))
		copy(out, b)
		return out
	}
	out := make([]byte, n)
	copy(out, b[:n])
	return out
}
