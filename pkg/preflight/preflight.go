package preflight

import (
	"fmt"
	"os"
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
		if info.IsDir() && (info.Name() == ".git" || info.Name() == "node_modules" || info.Name() == "vendor") {
			return filepath.SkipDir
		}
		if info.IsDir() {
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
