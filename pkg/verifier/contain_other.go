//go:build !linux

package verifier

import (
	"context"
	"os/exec"
)

func ownershipCommand(ctx context.Context, dir string, argv []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "sh", argv...)
	cmd.Dir = dir
	return cmd, nil
}

func ownershipInfoExpected() bool { return false }
