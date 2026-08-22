// Package gitroot answers "where is this repository" — once.
//
// FAC-565: `git rev-parse --path-format=absolute --git-common-dir` was written
// in twelve places across the tree, and two of them omitted the absolute format
// and hand-rolled the absolutization afterwards. Every copy was individually
// defensible and collectively they are one rule written twelve times, which is
// the defect class behind the handoff-mailbox and review-root divergences: two
// authorities disagreeing about where a project lives.
//
// This package is deliberately a LEAF. The natural home looked like
// pkg/worktree, but worktree imports claim which imports lifecycle, and
// lifecycle is one of the callers — so putting it there created an import
// cycle. A rule shared by every layer cannot live in a layer.
package gitroot

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// CommonDir returns the absolute shared git directory for the repository
// containing startDir.
//
// Always absolute. Always fail-closed: an empty result is an error rather than
// a path that would silently resolve relative to the caller's cwd.
func CommonDir(ctx context.Context, startDir string) (string, error) {
	if strings.TrimSpace(startDir) == "" {
		startDir = "."
	}
	cmd := exec.CommandContext(ctx, "git", "-C", startDir,
		"rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git common dir from %q: %v (%s)", startDir, err, strings.TrimSpace(string(out)))
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", fmt.Errorf("git common dir from %q: empty result", startDir)
	}
	// Older git can still answer relatively despite the flag; normalize rather
	// than trusting it, since a relative answer is exactly the divergence this
	// package exists to remove.
	if !filepath.IsAbs(common) {
		abs, absErr := filepath.Abs(filepath.Join(startDir, common))
		if absErr != nil {
			return "", fmt.Errorf("git common dir from %q: %w", startDir, absErr)
		}
		common = abs
	}
	return filepath.Clean(common), nil
}

// Toplevel returns the absolute working-tree root containing startDir.
func Toplevel(ctx context.Context, startDir string) (string, error) {
	if strings.TrimSpace(startDir) == "" {
		startDir = "."
	}
	cmd := exec.CommandContext(ctx, "git", "-C", startDir, "rev-parse", "--show-toplevel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git toplevel from %q: %v (%s)", startDir, err, strings.TrimSpace(string(out)))
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", fmt.Errorf("git toplevel from %q: empty result", startDir)
	}
	return filepath.Clean(top), nil
}
