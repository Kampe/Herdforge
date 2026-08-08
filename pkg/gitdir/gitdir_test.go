package gitdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsNestedGitDir_LinkedWorktree(t *testing.T) {
	tmp := t.TempDir()
	nested := filepath.Join(tmp, "lane-wt")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: /tmp/some/gitdir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsNestedGitDir(nested, tmp) {
		t.Fatal("linked worktree with .git file should be detected as nested")
	}
}

func TestIsNestedGitDir_SubmoduleStyle(t *testing.T) {
	tmp := t.TempDir()
	nested := filepath.Join(tmp, "submodule")
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsNestedGitDir(nested, tmp) {
		t.Fatal("submodule with .git directory should be detected as nested")
	}
}

func TestIsNestedGitDir_RepoRootNotNested(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if IsNestedGitDir(tmp, tmp) {
		t.Fatal("repo root itself must not be considered nested")
	}
}

func TestIsNestedGitDir_PlainDirNotNested(t *testing.T) {
	tmp := t.TempDir()
	plain := filepath.Join(tmp, "pkg", "provider")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if IsNestedGitDir(plain, tmp) {
		t.Fatal("ordinary directory without .git must not be considered nested")
	}
}

func TestIsNestedGitDir_RelativePaths(t *testing.T) {
	tmp := t.TempDir()
	nested := filepath.Join(tmp, "wt")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: ...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	if !IsNestedGitDir("wt", ".") {
		t.Fatal("relative paths should resolve correctly")
	}
}
