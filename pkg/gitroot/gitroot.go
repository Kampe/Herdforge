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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MergeTreeWriteFlag asks git merge-tree to write and return the merged tree
// object. Keep this repository-wide Git capability rule here so conflict
// probes and landing proofs cannot drift onto different invocations.
const (
	MergeTreeWriteFlag    = "--write-tree"
	MergeTreeHeadBaseFlag = "--merge-base=HEAD"
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

// EnvProjectRoot names the canonical PROJECT CONTROL root.
//
// FAC-573: HERD_ROOT was overloaded. The launch environment sets it to the
// LANE's root — a supervisor launched with cwd
// ./review-harvest-supervisor inherits HERD_ROOT pointing there — while the
// mailbox and review-root resolvers read it as the project control root. So the
// two resolvers agreed with each other and were both wrong: the live supervisor
// resolved a lane-local mailbox, reported no pending handoffs, and five real
// ones sat unread in the project mailbox.
//
// An explicit cd could not repair it, because an inherited override outranks the
// working directory by design. That is the correct precedence for a lane root
// and the wrong precedence for a project root, which is the tell that these are
// two different values wearing one name.
const EnvProjectRoot = "HERD_PROJECT_ROOT"

// EnvLaneRoot is the lane's own root. It is deliberately NOT consulted when
// resolving the project control root.
const EnvLaneRoot = "HERD_ROOT"

// ProjectRoot is the canonical project control-plane root for startDir.
//
// Precedence, and the reasoning for it:
//
//  1. HERD_PROJECT_ROOT, when set. A dedicated name for a dedicated concept, so
//     an operator or launcher can state the project root without it being
//     confused for a lane root.
//  2. The git common directory's parent. This is worktree-INVARIANT: every
//     worktree of a repository resolves the same value, which is exactly the
//     property a control plane needs and the property a lane root lacks.
//
// HERD_ROOT is never used here. It names the lane, and trusting it is the defect
// this function exists to remove.
//
// laneOverride reports a HERD_ROOT that disagrees with the resolved project
// root, so the divergence can be surfaced rather than silently tolerated.
func ProjectRoot(ctx context.Context, startDir string) (root string, laneOverride string, err error) {
	if explicit := strings.TrimSpace(os.Getenv(EnvProjectRoot)); explicit != "" {
		abs, absErr := filepath.Abs(explicit)
		if absErr != nil {
			return "", "", fmt.Errorf("%s=%q is not resolvable: %w", EnvProjectRoot, explicit, absErr)
		}
		return filepath.Clean(abs), divergentLane(abs), nil
	}
	common, err := CommonDir(ctx, startDir)
	if err != nil {
		return "", "", err
	}
	resolved := filepath.Dir(common)
	return resolved, divergentLane(resolved), nil
}

// divergentLane returns a HERD_ROOT that names somewhere other than the project
// root. Empty when unset or in agreement.
func divergentLane(projectRoot string) string {
	lane := strings.TrimSpace(os.Getenv(EnvLaneRoot))
	if lane == "" {
		return ""
	}
	abs, err := filepath.Abs(lane)
	if err != nil {
		return lane
	}
	if filepath.Clean(abs) == filepath.Clean(projectRoot) {
		return ""
	}
	return filepath.Clean(abs)
}
