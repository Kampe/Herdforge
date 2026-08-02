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

func TestCheckWorktreeBoundary_DetectLeak(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "leak.go")
	os.WriteFile(goFile, []byte("// path: /Users/evil/secret\npackage x\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err == nil {
		t.Fatal("expected leak detection for /Users/ path in .go file")
	}
}

func TestCheckWorktreeBoundary_DetectHomeLeak(t *testing.T) {
	tmpDir := t.TempDir()
	yamlFile := filepath.Join(tmpDir, "config.yaml")
	os.WriteFile(yamlFile, []byte("path: /home/ec2-user/secret\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err == nil {
		t.Fatal("expected leak detection for /home/ path in .yaml file")
	}
}

func TestCheckWorktreeBoundary_SkipsPreflightTest(t *testing.T) {
	tmpDir := t.TempDir()
	// File named preflight_something_test.go should be skipped even with /Users/ content
	testFile := filepath.Join(tmpDir, "preflight_helper_test.go")
	os.WriteFile(testFile, []byte("// path: /Users/test\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected preflight test file to be skipped, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsAGENTSMD(t *testing.T) {
	tmpDir := t.TempDir()
	agentFile := filepath.Join(tmpDir, "AGENTS.md")
	os.WriteFile(agentFile, []byte("path: /Users/something\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected AGENTS.md to be skipped, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsPreflightSource(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "preflight.go")
	os.WriteFile(srcFile, []byte("path: /Users/something\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected preflight.go to be skipped, got: %v", err)
	}
}
