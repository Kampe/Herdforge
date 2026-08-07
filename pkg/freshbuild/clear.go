package freshbuild

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClearChain removes only profile-declared artifacts inside chain dirs.
// Fail-closed: any deletion error is returned (a partial clear that then
// reports CLEAN would misdiagnose a real error as stale dist). Paths outside
// root are rejected. Missing artifacts are not errors (idempotent clear).
func ClearChain(root string, dirs []string, spec ArtifactSpec) error {
	root = filepath.Clean(root)
	if root == "" {
		return fmt.Errorf("freshbuild: clear requires repository root")
	}
	if len(spec.Dirs) == 0 && len(spec.Files) == 0 && len(spec.Globs) == 0 {
		// Profiles that declare no artifacts (e.g. pure Go source builds)
		// perform no destructive work.
		return nil
	}
	for _, d := range dirs {
		d = filepath.Clean(d)
		if err := assertUnderRoot(root, d); err != nil {
			return err
		}
		for _, name := range spec.Dirs {
			name = strings.TrimSpace(name)
			if name == "" || strings.Contains(name, string(os.PathSeparator)) || name == ".." || name == "." {
				return fmt.Errorf("freshbuild: refuse unsafe artifact dir name %q", name)
			}
			// Destructive boundary: never wipe node_modules, .git, src, etc.
			if forbiddenArtifact(name) {
				return fmt.Errorf("freshbuild: refuse destructive artifact %q", name)
			}
			target := filepath.Join(d, name)
			if err := assertUnderRoot(root, target); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("freshbuild: remove %s: %w", target, err)
			}
		}
		for _, name := range spec.Files {
			name = strings.TrimSpace(name)
			if name == "" || strings.Contains(name, string(os.PathSeparator)) || name == ".." {
				return fmt.Errorf("freshbuild: refuse unsafe artifact file name %q", name)
			}
			target := filepath.Join(d, name)
			if err := assertUnderRoot(root, target); err != nil {
				return err
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("freshbuild: remove %s: %w", target, err)
			}
		}
		for _, pattern := range spec.Globs {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" || strings.Contains(pattern, string(os.PathSeparator)) {
				return fmt.Errorf("freshbuild: refuse unsafe artifact glob %q", pattern)
			}
			matches, err := filepath.Glob(filepath.Join(d, pattern))
			if err != nil {
				return fmt.Errorf("freshbuild: glob %s: %w", pattern, err)
			}
			for _, m := range matches {
				if err := assertUnderRoot(root, m); err != nil {
					return err
				}
				// Only remove files matching the glob; never follow into dirs
				// unless the basename is an explicit Dir entry.
				st, err := os.Lstat(m)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return err
				}
				if st.IsDir() {
					continue
				}
				if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("freshbuild: remove %s: %w", m, err)
				}
			}
		}
	}
	return nil
}

func assertUnderRoot(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return fmt.Errorf("freshbuild: path %q escapes repository root %q", path, root)
	}
	return nil
}

func forbiddenArtifact(name string) bool {
	switch strings.ToLower(name) {
	case "node_modules", ".git", "src", "lib", "vendor", "pkg", "cmd",
		"internal", "testdata", ".herd", "go.mod", "go.sum", "package.json",
		"pnpm-lock.yaml", "pnpm-workspace.yaml":
		return true
	default:
		return false
	}
}
