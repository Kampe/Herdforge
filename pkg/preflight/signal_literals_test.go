package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDangerousSignalLiterals_Clean(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ok.go")
	if err := os.WriteFile(path, []byte("package x\nfunc f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckDangerousSignalLiterals(root); err != nil {
		t.Fatalf("clean tree: %v", err)
	}
}

func TestCheckDangerousSignalLiterals_FindsHostWide(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bad.go")
	body := "package x\nimport \"syscall\"\nfunc f() { syscall.Kill(-1, syscall.SIGTERM) }\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckDangerousSignalLiterals(root); err == nil {
		t.Fatal("expected detection of syscall.Kill(-1, ...)")
	}
}

func TestCheckDangerousSignalLiterals_SkipsTests(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bad_test.go")
	body := "package x\nimport \"syscall\"\nfunc TestX(t *testing.T) { syscall.Kill(-1, 15) }\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckDangerousSignalLiterals(root); err != nil {
		t.Fatalf("test files must be skipped: %v", err)
	}
}
