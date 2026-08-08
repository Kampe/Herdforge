package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/gitdir"
)

// TestCheckDangerousSignalLiterals_SkipsNestedWorktree proves FAC-233: a
// nested git worktree on disk is skipped structurally, while the identical
// offence in the real tree is still caught.
func TestCheckDangerousSignalLiterals_SkipsNestedWorktree(t *testing.T) {
	tmp := t.TempDir()

	// Create a linked worktree: a directory with a .git *file*.
	wtDir := filepath.Join(tmp, "wt", "pkg", "kill")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "wt", ".git"), []byte("gitdir: /tmp/fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	offending := []byte("package kill\nimport \"syscall\"\nfunc f() { syscall.Kill(-1, syscall.SIGTERM) }\n")
	if err := os.WriteFile(filepath.Join(wtDir, "kill.go"), offending, 0o644); err != nil {
		t.Fatal(err)
	}

	// Scanner must NOT flag the worktree copy.
	if err := CheckDangerousSignalLiterals(tmp); err != nil {
		t.Fatalf("scanner walked into nested worktree and flagged it: %v", err)
	}

	// Now place the identical offence in the real tree.
	realDir := filepath.Join(tmp, "pkg", "kill")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "kill.go"), offending, 0o644); err != nil {
		t.Fatal(err)
	}

	// Scanner MUST flag the real-tree copy.
	if err := CheckDangerousSignalLiterals(tmp); err == nil {
		t.Fatal("scanner failed to catch the identical offence in the real tree")
	}
}

// TestCheckDangerousSignalLiterals_CoverageFloor ensures the scan covers the
// real repo and does not silently collapse to nothing.
func TestCheckDangerousSignalLiterals_CoverageFloor(t *testing.T) {
	root := realRepoRoot(t)
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "vendor" || name == "node_modules" ||
				name == "bin" || (len(name) > 0 && name[0] == '.') {
				return filepath.SkipDir
			}
			if gitdir.IsNestedGitDir(path, root) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) == ".go" && !strings.HasSuffix(path, "_test.go") {
			scanned++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d production go files; the walk is not covering the repo", scanned)
	}
}
