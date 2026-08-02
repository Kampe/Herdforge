package park

import (
	"context"
	"fmt"
	"os"
	"strings"
)

var ErrGCNotFound = fmt.Errorf("bin/herd-gc not found")

type ReapMode int

const (
	ReapDryRun ReapMode = iota
	ReapApplied
)

type ReapResult struct {
	Mode    string `json:"mode"`
	Tag     string `json:"tag"`
	Output  string `json:"output"`
	Removed bool   `json:"removed"`
}

func Reap(ctx context.Context, repoRoot, tag string, mode ReapMode) (*ReapResult, error) {
	gcPath := repoRoot + "/bin/herd-gc"
	if _, err := os.Stat(gcPath); err != nil {
		return nil, ErrGCNotFound
	}

	args := []string{gcPath}
	if mode == ReapDryRun {
		args = append(args, "--dry-run")
	} else {
		args = append(args, "refs/tags/" + tag)
	}

	cmd := execCommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("herd-gc: %w\n%s", err, string(out))
	}

	modeStr := "dry-run"
	removed := false
	if mode == ReapApplied {
		modeStr = "applied"
		removed = true
	}

	return &ReapResult{
		Mode:    modeStr,
		Tag:     tag,
		Output:  strings.TrimSpace(string(out)),
		Removed: removed,
	}, nil
}
