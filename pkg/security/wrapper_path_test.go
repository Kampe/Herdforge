package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRequireWrapperOnPATH carries FAC-190's PreparedOS.WrapperResolves check
// into the FAC-133 Control launch path. Install writing the wrapper is not the
// same as the agent executing it — if the real harness binary sits earlier on
// the constructed PATH, the agent runs outside the seatbelt while every
// downstream proof attests a wrapper nothing execs.
func TestRequireWrapperOnPATH(t *testing.T) {
	mkexec := func(dir, name string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pathEnv := func(dirs ...string) []string {
		return []string{"HOME=/nowhere", "PATH=" + strings.Join(dirs, string(os.PathListSeparator))}
	}

	t.Run("wrapper first on PATH", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "contain", "bin")
		wrapper := mkexec(binDir, "pi")
		mkexec(filepath.Join(root, "usr", "bin"), "pi") // real binary, shadowed
		if err := requireWrapperOnPATH(pathEnv(binDir, filepath.Join(root, "usr", "bin")), "pi", wrapper); err != nil {
			t.Fatalf("wrapper first on PATH must pass: %v", err)
		}
	})

	t.Run("real binary shadows wrapper", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "contain", "bin")
		wrapper := mkexec(binDir, "pi")
		realDir := filepath.Join(root, "usr", "bin")
		real := mkexec(realDir, "pi")
		err := requireWrapperOnPATH(pathEnv(realDir, binDir), "pi", wrapper)
		if err == nil {
			t.Fatal("a real binary ahead of the wrapper must fail closed")
		}
		if !strings.Contains(err.Error(), real) || !strings.Contains(err.Error(), "unsandboxed") {
			t.Fatalf("error must name the shadowing binary: %v", err)
		}
	})

	t.Run("wrapper missing", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "contain", "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		err := requireWrapperOnPATH(pathEnv(binDir), "pi", filepath.Join(binDir, "pi"))
		if err == nil || !strings.Contains(err.Error(), "not installed") {
			t.Fatalf("missing wrapper must fail closed, got: %v", err)
		}
	})

	t.Run("wrapper not executable", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "contain", "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(binDir, "pi")
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := requireWrapperOnPATH(pathEnv(binDir), "pi", wrapper)
		if err == nil || !strings.Contains(err.Error(), "not executable") {
			t.Fatalf("non-executable wrapper must fail closed, got: %v", err)
		}
	})

	t.Run("wrapper dir absent from PATH", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "contain", "bin")
		wrapper := mkexec(binDir, "pi")
		other := filepath.Join(root, "elsewhere")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		err := requireWrapperOnPATH(pathEnv(other), "pi", wrapper)
		if err == nil || !strings.Contains(err.Error(), "not reachable on the agent PATH") {
			t.Fatalf("unreachable wrapper must fail closed, got: %v", err)
		}
	})

	t.Run("no PATH in env", func(t *testing.T) {
		root := t.TempDir()
		wrapper := mkexec(filepath.Join(root, "bin"), "pi")
		err := requireWrapperOnPATH([]string{"HOME=/nowhere"}, "pi", wrapper)
		if err == nil || !strings.Contains(err.Error(), "no PATH") {
			t.Fatalf("missing PATH must fail closed, got: %v", err)
		}
	})
}
