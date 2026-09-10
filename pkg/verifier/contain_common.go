package verifier

import (
	"context"
	"os/exec"
)

// commandInDir gives every ownership backend the same working-directory
// contract. Relative verifier commands and their relative fixture inputs must
// resolve from the owned candidate directory, regardless of containment mode.
func commandInDir(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd
}
