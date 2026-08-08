package gitdir

import (
	"os"
	"path/filepath"
)

// IsNestedGitDir reports whether dir is a nested git repository or linked
// worktree that should be skipped during repository scans. It returns true
// when dir is not the repo root and contains a .git entry (file or directory).
//
// A linked worktree has .git as a file containing "gitdir: ..."; a submodule
// or nested clone has .git as a directory. Both are detected.
//
// repoRoot is the top-level directory of the scan and is never considered
// nested — it owns the .git that the rest of the repository shares.
func IsNestedGitDir(dir, repoRoot string) bool {
	absDir, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(filepath.Clean(repoRoot))
	if err != nil {
		return false
	}
	if absDir == absRoot {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	return true
}
