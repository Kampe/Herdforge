//go:build !linux

package verifier

import (
	"context"
	"os/exec"
)

func ownershipCommand(ctx context.Context, dir string, argv []string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "sh", argv...), nil
}

func ownershipInfoExpected() bool { return false }
