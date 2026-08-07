package freshbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClearChain_OnlyChainArtifacts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	c := filepath.Join(root, "c")
	for _, d := range []string{a, b, c} {
		if err := os.MkdirAll(filepath.Join(d, "dist"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "dist", "index.js"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "tsconfig.tsbuildinfo"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "extra.tsbuildinfo"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := PnpmProfile{}.ArtifactNames()
	if err := ClearChain(root, []string{a, b}, spec); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(a, "dist")); !os.IsNotExist(err) {
		t.Fatalf("a/dist should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b, "dist")); !os.IsNotExist(err) {
		t.Fatalf("b/dist should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c, "dist")); err != nil {
		t.Fatalf("c/dist (not in chain) must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a, "tsconfig.tsbuildinfo")); !os.IsNotExist(err) {
		t.Fatal("tsconfig.tsbuildinfo should be gone")
	}
	if _, err := os.Stat(filepath.Join(a, "extra.tsbuildinfo")); !os.IsNotExist(err) {
		t.Fatal("*.tsbuildinfo should be gone")
	}
	if raw, err := os.ReadFile(filepath.Join(a, "keep.txt")); err != nil || string(raw) != "keep" {
		t.Fatalf("keep.txt must survive: %v %q", err, raw)
	}
}

func TestClearChain_IdempotentMissing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ClearChain(root, []string{a}, PnpmProfile{}.ArtifactNames()); err != nil {
		t.Fatal(err)
	}
}

func TestClearChain_RejectsForbiddenDirsFilesAndGlobs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "package.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ClearChain(root, []string{a}, ArtifactSpec{Dirs: []string{"node_modules"}}); err == nil {
		t.Fatal("must refuse node_modules dir")
	}
	if err := ClearChain(root, []string{a}, ArtifactSpec{Files: []string{"go.mod"}}); err == nil {
		t.Fatal("must refuse go.mod file")
	}
	if err := ClearChain(root, []string{a}, ArtifactSpec{Files: []string{"package.json"}}); err == nil {
		t.Fatal("must refuse package.json file")
	}
	// Glob that would match go.mod
	if err := ClearChain(root, []string{a}, ArtifactSpec{Globs: []string{"go.mod"}}); err == nil {
		t.Fatal("must refuse go.mod via glob pattern")
	}
	if err := ClearChain(root, []string{a}, ArtifactSpec{Globs: []string{"*.mod"}}); err == nil {
		t.Fatal("must refuse go.mod matched by *.mod")
	}
	// Survivors still present
	if _, err := os.Stat(filepath.Join(a, "go.mod")); err != nil {
		t.Fatalf("go.mod must survive: %v", err)
	}

	outside := filepath.Join(root, "..", "nope")
	escErr := ClearChain(root, []string{outside}, PnpmProfile{}.ArtifactNames())
	if escErr == nil {
		t.Fatal("must refuse path escape")
	}
	if !strings.Contains(escErr.Error(), "escapes") {
		t.Fatalf("unexpected escape err: %v", escErr)
	}
}

func TestClearChain_EmptySpecNoop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.MkdirAll(filepath.Join(a, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ClearChain(root, []string{a}, GoProfile{}.ArtifactNames()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a, "dist")); err != nil {
		t.Fatalf("go profile must not clear dist: %v", err)
	}
}
