package mergeadmit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProductionGoFiles_SkipsNestedWorktree proves FAC-233: a nested git
// worktree on disk is skipped structurally, while files in the real tree are
// still found.
func TestProductionGoFiles_SkipsNestedWorktree(t *testing.T) {
	tmp := t.TempDir()

	// Create a linked worktree: a directory with a .git *file*.
	wtPkg := filepath.Join(tmp, "wt", "pkg", "evil")
	if err := os.MkdirAll(wtPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "wt", ".git"), []byte("gitdir: /tmp/fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A Go file inside the worktree that should be skipped.
	wtFile := []byte("package evil\n\nvar _ = reviewledger.AdmissionOpts{}\n")
	if err := os.WriteFile(filepath.Join(wtPkg, "bypass.go"), wtFile, 0o644); err != nil {
		t.Fatal(err)
	}

	// Place the identical file in the real tree.
	realPkg := filepath.Join(tmp, "pkg", "evil")
	if err := os.MkdirAll(realPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realPkg, "bypass.go"), wtFile, 0o644); err != nil {
		t.Fatal(err)
	}

	files := productionGoFilesFromRoot(t, tmp, 0)

	var foundReal, foundWorktree bool
	for _, f := range files {
		if strings.HasPrefix(f.relPath, "pkg/evil/") {
			foundReal = true
		}
		if strings.HasPrefix(f.relPath, "wt/") {
			foundWorktree = true
		}
	}
	if foundWorktree {
		t.Fatal("scanner walked into nested worktree and found files there")
	}
	if !foundReal {
		t.Fatal("scanner failed to find the identical file in the real tree")
	}
}
