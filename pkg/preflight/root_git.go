package preflight

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckRootGitConfig refuses a Git view that does not describe root itself.
// A local core.worktree redirect is especially dangerous because ordinary Git
// cleanliness checks can then report the state of a different worktree.
func CheckRootGitConfig(root string) error {
	want, err := canonicalPath(root)
	if err != nil {
		return fmt.Errorf("resolve repository root %q: %w", root, err)
	}

	top, err := runCmd(root, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		// Static preflight supports a newly scaffolded directory, but must not
		// turn a present and corrupted Git checkout into a false pass.
		if _, statErr := os.Lstat(filepath.Join(root, ".git")); os.IsNotExist(statErr) {
			return nil
		}
		return fmt.Errorf("inspect Git checkout at %q: %w", root, err)
	}
	got, err := canonicalPath(top)
	if err != nil {
		return fmt.Errorf("resolve Git toplevel %q: %w", top, err)
	}
	if got != want {
		return fmt.Errorf("Git toplevel mismatch: expected repository root %q, got %q (check core.worktree)", want, got)
	}

	cmd := exec.Command("git", "config", "--local", "--get", "core.worktree")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("read local core.worktree: %w", err)
	}
	configured := strings.TrimSpace(string(out))
	if configured != "" {
		return fmt.Errorf("shared root Git config redirects core.worktree to %q; refusing to operate on a redirected checkout", configured)
	}
	return nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(resolved), nil
}
