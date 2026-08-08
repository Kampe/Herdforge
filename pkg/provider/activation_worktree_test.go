package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanForConstructorBypasses_SkipsNestedWorktree proves FAC-233: a nested
// git worktree on disk is skipped structurally, while the identical offence in
// the real tree is still caught.
func TestScanForConstructorBypasses_SkipsNestedWorktree(t *testing.T) {
	tmp := t.TempDir()

	// Create a linked worktree: a directory with a .git *file*.
	wtPkg := filepath.Join(tmp, "wt", "cmd", "herd")
	if err := os.MkdirAll(wtPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "wt", ".git"), []byte("gitdir: /tmp/fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A Go file inside the worktree containing a banned constructor call.
	offending := []byte("package main\n\nimport \"github.com/Kampe/Herdforge/pkg/provider\"\n\nfunc main() { provider.NewMemoryProvider() }\n")
	if err := os.WriteFile(filepath.Join(wtPkg, "main.go"), offending, 0o644); err != nil {
		t.Fatal(err)
	}

	// Scanner must NOT flag the worktree copy.
	wtOffenders, _ := scanForConstructorBypasses(t, tmp)
	for _, o := range wtOffenders {
		if strings.Contains(o, "wt/") {
			t.Fatalf("scanner walked into nested worktree and flagged it: %s", o)
		}
	}

	// Now place the identical offence in the real tree.
	realPkg := filepath.Join(tmp, "cmd", "herd")
	if err := os.MkdirAll(realPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realPkg, "main.go"), offending, 0o644); err != nil {
		t.Fatal(err)
	}

	// Scanner MUST flag the real-tree copy.
	realOffenders, _ := scanForConstructorBypasses(t, tmp)
	var found bool
	for _, o := range realOffenders {
		if strings.Contains(o, "cmd/herd/main.go") {
			found = true
		}
	}
	if !found {
		t.Fatal("scanner failed to catch the identical offence in the real tree")
	}
}
