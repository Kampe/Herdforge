package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckWorktreeBoundary_SkipsGitDir(t *testing.T) {
	tmpDir := t.TempDir()
	// Create .git directory — should be skipped entirely
	os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755)
	os.WriteFile(filepath.Join(tmpDir, ".git", "config"), []byte("path: /Users/evil"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection inside .git, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsNodeModules(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "node_modules"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "node_modules", "pkg"), []byte("path: /Users/secret"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection inside node_modules, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsVendor(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "vendor"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "vendor", "lib"), []byte("path: /Users/secret"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection inside vendor, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_IgnoresNonCheckedExts(t *testing.T) {
	tmpDir := t.TempDir()
	// .png files with absolute paths should be ignored
	os.WriteFile(filepath.Join(tmpDir, "image.png"), []byte("/Users/something"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection for non-checked extension, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_ReadError(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a dir that looks like a file extension — walk error testing
	os.MkdirAll(filepath.Join(tmpDir, "subdir"), 0755)
	os.Chmod(filepath.Join(tmpDir, "subdir"), 0000)
	defer os.Chmod(filepath.Join(tmpDir, "subdir"), 0755)

	// Put a .go file in it
	goFile := filepath.Join(tmpDir, "subdir", "test.go")
	os.WriteFile(goFile, []byte("package x"), 0644)
	os.Chmod(goFile, 0000)
	defer os.Chmod(goFile, 0644)

	// This should not panic — even if walk can't read a file inside
	err := CheckWorktreeBoundary(tmpDir)
	_ = err // walk may or may not return an error depending on OS; we just verify no panic
}

func TestCheckWorktreeBoundary_WalkError(t *testing.T) {
	err := CheckWorktreeBoundary("/nonexistent-path-xyzzy")
	if err == nil {
		t.Fatal("expected walk error for nonexistent path")
	}
}
