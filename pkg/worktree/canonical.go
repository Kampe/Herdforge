package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveCanonicalRoot resolves the true repository root regardless of
// process cwd or package-relative invocation (FAC-152). It never trusts a
// literal "." string joined into a path: it asks Git to walk up from
// startDir and resolve --git-common-dir, which correctly returns the main
// repository's .git even when startDir is deep inside a linked worktree
// (e.g. <task-worktree>/pkg/dispatch) — exactly the case that previously let
// a dispatch invocation compute its worktree pool relative to the current
// task worktree instead of the shared repository, producing the nested
// pkg/dispatch/.herd/worktrees/fac-1 lane.
//
// override (typically $HERD_ROOT) short-circuits Git discovery entirely and
// is normalized the same way. An empty startDir defaults to ".".
func ResolveCanonicalRoot(ctx context.Context, startDir, override string) (string, error) {
	if override != "" {
		return normalizePath(override), nil
	}
	if startDir == "" {
		startDir = "."
	}
	cmd := execCommandContext(ctx, "git", "-C", startDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve canonical root from %q: git-common-dir: %v (%s)", startDir, err, strings.TrimSpace(string(out)))
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", fmt.Errorf("resolve canonical root from %q: empty git-common-dir", startDir)
	}
	parent := filepath.Dir(common)
	if parent == "" || parent == "." {
		return "", fmt.Errorf("resolve canonical root from %q: unresolvable parent of %q", startDir, common)
	}
	return normalizePath(parent), nil
}

// ContainmentError explains why a candidate worktree destination was
// refused before any git mutation (FAC-152).
type ContainmentError struct {
	Destination string
	ContainedBy string
	Branch      string
	Reason      string
}

func (e *ContainmentError) Error() string {
	if e.Branch != "" {
		return fmt.Sprintf("worktree destination %q rejected: %s (contained by %q, branch %s)",
			e.Destination, e.Reason, e.ContainedBy, e.Branch)
	}
	return fmt.Sprintf("worktree destination %q rejected: %s (contained by %q)",
		e.Destination, e.Reason, e.ContainedBy)
}

// RejectContainedDestination fails closed when a candidate worktree
// destination would nest inside another registered worktree — the exact
// shape observed at pkg/dispatch/.herd/worktrees/fac-1 nested inside the
// FAC-64 task worktree. poolRoot is the manager's configured root; nesting
// under poolRoot itself is the expected pool layout (e.g.
// <root>/.herd/worktrees/<task>) and is allowed. Nesting under any OTHER
// registered worktree — a source lane the command happened to run from, a
// sibling task worktree, or a package subtree inside one — is refused
// before git worktree add ever runs. Symlink and case-only path aliases are
// normalized away so they cannot be used to route around the check.
func RejectContainedDestination(poolRoot, destination string, registered []*WorktreeInfo) error {
	if poolRoot == "" || destination == "" {
		return fmt.Errorf("containment check: poolRoot and destination are required")
	}
	if !isContainedIn(destination, poolRoot) {
		return &ContainmentError{
			Destination: destination,
			ContainedBy: poolRoot,
			Reason:      "destination is outside the configured worktree pool root",
		}
	}
	normRoot := normalizePath(poolRoot)
	for _, wt := range registered {
		if wt == nil || wt.Path == "" {
			continue
		}
		if pathsEqual(normalizePath(wt.Path), normRoot) {
			continue // the pool root's own nested pool directory is expected
		}
		if isContainedIn(destination, wt.Path) {
			return &ContainmentError{
				Destination: destination,
				ContainedBy: wt.Path,
				Branch:      wt.Branch,
				Reason:      "destination is nested inside another registered worktree",
			}
		}
	}
	return nil
}

// isContainedIn reports whether child is exactly parent or nested within
// it, after symlink normalization (normalizePath, reap.go) and, on
// case-insensitive filesystems (macOS/Windows), case-folded comparison.
func isContainedIn(child, parent string) bool {
	nc := normalizePath(child)
	np := normalizePath(parent)
	if pathsEqual(nc, np) {
		return true
	}
	sep := string(filepath.Separator)
	npWithSep := np
	if !strings.HasSuffix(npWithSep, sep) {
		npWithSep += sep
	}
	if caseInsensitiveFS() {
		return len(nc) >= len(npWithSep) && strings.EqualFold(nc[:len(npWithSep)], npWithSep)
	}
	return strings.HasPrefix(nc, npWithSep)
}

func pathsEqual(a, b string) bool {
	if a == b {
		return true
	}
	if caseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return false
}

func caseInsensitiveFS() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}
