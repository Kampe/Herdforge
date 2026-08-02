package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckWorktreeBoundary_Clean(t *testing.T) {
	tmpDir := t.TempDir()
	cleanFile := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cleanFile, []byte("path: ./relative/path"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	if err := CheckWorktreeBoundary(tmpDir); err != nil {
		t.Errorf("expected clean boundary check, got err: %v", err)
	}
}

func TestCheckWorktreeBoundary_LeakDetected(t *testing.T) {
	tmpDir := t.TempDir()
	dirtyFile := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(dirtyFile, []byte("path: /Users/username/secret"), 0644); err != nil {
		t.Fatalf("failed to write dirty test file: %v", err)
	}

	if err := CheckWorktreeBoundary(tmpDir); err == nil {
		t.Errorf("expected leak detection error for absolute path, got nil")
	}
}
