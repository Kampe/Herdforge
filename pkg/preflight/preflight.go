package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckWorktreeBoundary verifies that no absolute paths leak into git tracking or config files
func CheckWorktreeBoundary(rootDir string) error {
	var absoluteLeakes []string

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && (info.Name() == ".git" || info.Name() == "node_modules" || info.Name() == "vendor" ||
			info.Name() == ".gemini" || info.Name() == ".qoder" || info.Name() == ".vscode" ||
			info.Name() == ".claude" || info.Name() == ".codebuddy" || info.Name() == ".kiro") {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}

		if info.Name() == ".mcp.json" {
			return nil
		}

		// Only check text / config / markdown / code files
		ext := filepath.Ext(path)
		if ext == ".go" || ext == ".yaml" || ext == ".yml" || ext == ".md" || ext == ".json" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			content := string(data)
			// Check for forbidden absolute path patterns (excluding self-check logic strings)
			isPreflightTest := strings.HasSuffix(path, "_test.go") && strings.Contains(path, "preflight")
			if (strings.Contains(content, "/Users/") || strings.Contains(content, "/home/") || strings.Contains(content, "C:\\")) &&
				!strings.HasSuffix(path, "AGENTS.md") && !strings.HasSuffix(path, "preflight.go") && !isPreflightTest {
				absoluteLeakes = append(absoluteLeakes, path)
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk repository tree: %w", err)
	}

	if len(absoluteLeakes) > 0 {
		return fmt.Errorf("absolute path leaks detected in worktree files: %v", absoluteLeakes)
	}

	return nil
}

// CheckAgentStayedInWorktree returns an error if the git working tree at
// repoRoot contains tracked-file modifications outside worktreePath —
// i.e., an agent editing outside its assigned worktree.
// It runs "git status --porcelain" and flags any path not under worktreePath.
func CheckAgentStayedInWorktree(worktreePath, repoRoot string) error {
	out, err := gitStatusPorcelain(repoRoot)
	if err != nil {
		// Not a git repo or git unavailable → no enforcement possible.
		return nil
	}
	if out == "" {
		return nil
	}

	absWorktree, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("worktreePath absolute resolution: %w", err)
	}

	var leaks []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		full := filepath.Join(repoRoot, path)
		absFull, err := filepath.Abs(full)
		if err != nil {
			continue
		}
		if isUnder(absFull, absWorktree) {
			continue
		}
		leaks = append(leaks, path)
	}

	if len(leaks) > 0 {
		return fmt.Errorf("agent wrote outside worktree boundary: %v", leaks)
	}
	return nil
}

// gitStatusPorcelain runs "git status --porcelain" in dir and returns stdout.
var gitStatusPorcelain = func(dir string) (string, error) {
	return runCmd(dir, "git", "status", "--porcelain")
}

// isUnder reports whether child is strictly inside parent (both absolute).
func isUnder(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if parent == child {
		return true
	}
	parentWithSep := parent
	if !strings.HasSuffix(parentWithSep, string(filepath.Separator)) {
		parentWithSep += string(filepath.Separator)
	}
	return strings.HasPrefix(child, parentWithSep)
}

// runCmd executes a command in dir and returns trimmed stdout or an error.
func runCmd(dir, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("command %s %v: %w\n%s", name, args, err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}
