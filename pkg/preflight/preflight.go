package preflight

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitdir"
)

// CheckWorktreeBoundary verifies that no absolute paths leak into git tracking or config files
func CheckWorktreeBoundary(rootDir string) error {
	return CheckWorktreeBoundaryWithAllowlist(rootDir, nil)
}

// CheckWorktreeBoundaryWithAllowlist is the configurable form of the
// boundary check. Allowlist entries are repo-relative file names or globs;
// they never grant permission to scan outside rootDir.
func CheckWorktreeBoundaryWithAllowlist(rootDir string, allowlist []string) error {
	var absoluteLeakes []string

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return fmt.Errorf("failed to open repository root: %w", err)
	}
	defer root.Close()

	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if path == ".herd/bootstrap" {
				return fs.SkipDir
			}
			if name == ".git" || name == "node_modules" || name == "vendor" ||
				name == ".gemini" || name == ".qoder" || name == ".vscode" ||
				name == ".claude" || name == ".codebuddy" || name == ".kiro" {
				return fs.SkipDir
			}
			// .herd/bootstrap is a generated dependency cache. Its mirrored
			// module sources may contain host paths by design; the tracked
			// repository boundary remains covered outside this runtime subtree.
			if path == filepath.Join(".herd", "bootstrap") {
				return fs.SkipDir
			}
			fullPath := filepath.Join(rootDir, path)
			if gitdir.IsNestedGitDir(fullPath, rootDir) {
				return fs.SkipDir
			}
			return nil
		}

		if d.Name() == ".mcp.json" {
			return nil
		}

		// Only check text / config / markdown / code files
		ext := filepath.Ext(path)
		if ext == ".go" || ext == ".yaml" || ext == ".yml" || ext == ".md" || ext == ".json" {
			data, err := root.ReadFile(path)
			if err != nil {
				return nil
			}
			content := string(data)
			// Check for forbidden absolute path patterns (excluding self-check logic strings)
			isPreflightTest := strings.HasSuffix(path, "_test.go") && strings.Contains(path, "preflight")
			if containsAbsolutePathLeak(content) &&
				!strings.HasSuffix(path, "AGENTS.md") && !strings.HasSuffix(path, "preflight.go") && !isPreflightTest {
				if !allowedAbsolutePath(path, allowlist) {
					absoluteLeakes = append(absoluteLeakes, filepath.Join(rootDir, path))
				}
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

// containsAbsolutePathLeak reports filesystem path markers unless the marker
// is part of a URL path. URLs commonly contain path segments such as /home/ or
// /Users/, but those are not host-specific paths leaking from the worktree.
func containsAbsolutePathLeak(content string) bool {
	markers := []string{"/Users/", "/home/", "C:\\"}
	for _, line := range strings.Split(content, "\n") {
		for _, marker := range markers {
			for offset := 0; offset < len(line); {
				relative := strings.Index(line[offset:], marker)
				if relative < 0 {
					break
				}
				index := offset + relative
				if !isURLPathSegment(line, index) {
					return true
				}
				offset = index + len(marker)
			}
		}
	}
	return false
}

func isURLPathSegment(line string, index int) bool {
	separator := strings.LastIndex(line[:index], "://")
	if separator >= 0 {
		return !strings.ContainsAny(line[separator+3:index], " \t\r")
	}

	// Bare-domain URLs omit a scheme, but still have a domain immediately
	// before the path marker (for example, docs.example.org/home/guide).
	return bareDomainURLPrefix.MatchString(line[:index])
}

var bareDomainURLPrefix = regexp.MustCompile(`([A-Za-z0-9-]+\.)+[A-Za-z]{2,}$`)

func allowedAbsolutePath(path string, allowlist []string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	for _, raw := range allowlist {
		pattern := filepath.ToSlash(filepath.Clean(strings.TrimSpace(raw)))
		if pattern == "" || filepath.IsAbs(pattern) || pattern == ".." || strings.HasPrefix(pattern, "../") {
			continue
		}
		if ok, err := filepath.Match(pattern, path); err == nil && ok {
			return true
		}
		if pattern == path {
			return true
		}
	}
	return false
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
