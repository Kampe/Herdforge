package park

import (
	"context"
	"fmt"
	"os"
	"strings"
)

var ErrGCNotFound = fmt.Errorf("bin/herd-gc not found")

type ReapResult struct {
	Mode    string `json:"mode"`
	Output  string `json:"output"`
	Applied bool   `json:"applied"`
}

// Reap delegates park/parked branch cleanup entirely to bin/herd-gc, which
// owns branch deletion by git-cherry patch-equivalence and is worktree-safe.
// Reap itself never deletes anything.
func Reap(ctx context.Context, repoRoot string, apply bool) (*ReapResult, error) {
	gcPath := repoRoot + "/bin/herd-gc"
	if _, err := os.Stat(gcPath); err != nil {
		return nil, ErrGCNotFound
	}

	args := []string{gcPath}
	if apply {
		args = append(args, "--apply", "--yes")
	}

	cmd := execCommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("herd-gc: %w\n%s", err, string(out))
	}

	mode := "dry-run"
	if apply {
		mode = "applied"
	}

	return &ReapResult{Mode: mode, Output: strings.TrimSpace(string(out)), Applied: apply}, nil
}
