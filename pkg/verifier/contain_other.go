//go:build !linux

package verifier

import (
	"context"
	"os/exec"
)

func ownershipCommand(ctx context.Context, dir string, argv []string) (*exec.Cmd, error) {
	cmd := commandInDir(ctx, dir, "sh", argv...)
	return cmd, nil
}

func ownershipInfoExpected() bool { return false }
