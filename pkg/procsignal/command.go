package procsignal

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// CommandContext starts a command in an owned process group. Cancelling the
// context kills the group, including descendants that could otherwise keep
// pipes open and make Cmd.Wait hang after the direct child exits.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return CancelSpawnedProcess(cmd.Process)
	}
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd
}
