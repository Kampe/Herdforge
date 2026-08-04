package preflight

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

func TestCheckAgentStayedInWorktree(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T, repoRoot, worktree string)
		wantErr     bool
		errContains string
	}{
		{
			name: "clean — no modifications in non-repo dir",
			setup: func(t *testing.T, repoRoot, worktree string) {
			},
			wantErr: false,
		},
		{
			name: "clean — no modifications in empty repo",
			setup: func(t *testing.T, repoRoot, worktree string) {
				initMinimalRepo(t, repoRoot)
			},
			wantErr: false,
		},
		{
			name: "clean — change inside worktree only",
			setup: func(t *testing.T, repoRoot, worktree string) {
				initMinimalRepo(t, repoRoot)
				mustMkdirAll(t, worktree)
				writeFile(t, filepath.Join(worktree, ".gitkeep"), "")
				addAndCommit(t, repoRoot, "add worktree dir")
				writeFile(t, filepath.Join(worktree, "main.go"), "package main")
			},
			wantErr: false,
		},
		{
			name: "leak — change outside worktree",
			setup: func(t *testing.T, repoRoot, worktree string) {
				initMinimalRepo(t, repoRoot)
				mustMkdirAll(t, worktree)
				writeFile(t, filepath.Join(worktree, ".gitkeep"), "")
				addAndCommit(t, repoRoot, "add worktree dir")
				writeFile(t, filepath.Join(repoRoot, "cmd", "main.go"), "package main")
			},
			wantErr:     true,
			errContains: "outside worktree boundary",
		},
		{
			name: "leak — git root file modified outside worktree",
			setup: func(t *testing.T, repoRoot, worktree string) {
				initMinimalRepo(t, repoRoot)
				mustMkdirAll(t, worktree)
				writeFile(t, filepath.Join(worktree, ".gitkeep"), "")
				addAndCommit(t, repoRoot, "add worktree dir")
				writeFile(t, filepath.Join(repoRoot, "README.md"), "# leak")
			},
			wantErr:     true,
			errContains: "outside worktree boundary",
		},
		{
			name: "clean — nested worktree correctly scoped",
			setup: func(t *testing.T, repoRoot, worktree string) {
				initMinimalRepo(t, repoRoot)
				innerWorktree := filepath.Join(worktree, "sub", "dir")
				mustMkdirAll(t, innerWorktree)
				writeFile(t, filepath.Join(innerWorktree, ".gitkeep"), "")
				addAndCommit(t, repoRoot, "add nested worktree")
				writeFile(t, filepath.Join(innerWorktree, "file.go"), "package sub")
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			worktreeDir := filepath.Join(tmpDir, ".herd", "worktrees", "agent-1")

			tt.setup(t, tmpDir, worktreeDir)

			err := CheckAgentStayedInWorktree(worktreeDir, tmpDir)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errContains)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// --- helpers ---

func initMinimalRepo(t *testing.T, dir string) {
	t.Helper()
	mustRun(t, dir, "git", "init")
	mustRun(t, dir, "git", "config", "user.email", "test@test")
	mustRun(t, dir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, ".gitkeep"), "")
	mustRun(t, dir, "git", "add", ".")
	mustRun(t, dir, "git", "commit", "-m", "initial")
}

func addAndCommit(t *testing.T, dir, msg string) {
	t.Helper()
	mustRun(t, dir, "git", "add", ".")
	mustRun(t, dir, "git", "commit", "-m", msg)
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	if name == "git" {
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return
	}
	_, err := runCmd(dir, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func contains(s, substr string) bool {
	return len(substr) == 0 || s != "" && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
