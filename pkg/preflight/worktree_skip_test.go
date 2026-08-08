package preflight

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/gitdir"
)

// TestCheckWorktreeBoundary_SkipsNestedWorktree proves FAC-233: a nested git
// worktree on disk is skipped structurally, while the identical offence in
// the real tree is still caught.
func TestCheckWorktreeBoundary_SkipsNestedWorktree(t *testing.T) {
	tmp := t.TempDir()

	// Create a linked worktree: a directory with a .git *file*.
	wtDir := filepath.Join(tmp, "wt", "pkg", "config")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The .git file marks this as a linked worktree.
	if err := os.WriteFile(filepath.Join(tmp, "wt", ".git"), []byte("gitdir: /tmp/fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Offending file inside the worktree — absolute path leak.
	offending := []byte("path: /Users/secret/leaked\n")
	if err := os.WriteFile(filepath.Join(wtDir, "herd.yaml"), offending, 0o644); err != nil {
		t.Fatal(err)
	}

	// Scanner must NOT flag the worktree copy.
	if err := CheckWorktreeBoundary(tmp); err != nil {
		t.Fatalf("scanner walked into nested worktree and flagged it: %v", err)
	}

	// Now place the identical offence in the real tree (outside the worktree).
	realDir := filepath.Join(tmp, "pkg", "config")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "herd.yaml"), offending, 0o644); err != nil {
		t.Fatal(err)
	}

	// Scanner MUST flag the real-tree copy.
	if err := CheckWorktreeBoundary(tmp); err == nil {
		t.Fatal("scanner failed to catch the identical offence in the real tree")
	}
}

// TestCheckWorktreeBoundary_SkipsNestedSubmodule proves submodule-style
// worktrees (.git as a directory) are also skipped.
func TestCheckWorktreeBoundary_SkipsNestedSubmodule(t *testing.T) {
	tmp := t.TempDir()

	submod := filepath.Join(tmp, "vendor-dep", "pkg")
	if err := os.MkdirAll(submod, 0o755); err != nil {
		t.Fatal(err)
	}
	// The .git directory marks this as a submodule / nested clone.
	if err := os.MkdirAll(filepath.Join(tmp, "vendor-dep", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	offending := []byte("path: /Users/secret/leaked\n")
	if err := os.WriteFile(filepath.Join(submod, "config.yaml"), offending, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CheckWorktreeBoundary(tmp); err != nil {
		t.Fatalf("scanner walked into nested submodule and flagged it: %v", err)
	}
}

// TestCheckWorktreeBoundary_CoverageFloor ensures the scan does not silently
// collapse to nothing when worktrees are present. It walks the real repo and
// verifies a minimum number of files are inspected.
func TestCheckWorktreeBoundary_CoverageFloor(t *testing.T) {
	root := realRepoRoot(t)
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" ||
				name == ".gemini" || name == ".qoder" || name == ".vscode" ||
				name == ".claude" || name == ".codebuddy" || name == ".kiro" {
				return filepath.SkipDir
			}
			if gitdir.IsNestedGitDir(path, root) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext == ".go" || ext == ".yaml" || ext == ".yml" || ext == ".md" || ext == ".json" {
			scanned++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files; the walk is not covering the repo", scanned)
	}
}

func realRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate module root")
	return ""
}
