package lifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKaneoBinFallsBackToPATHWhenRepositoryWrapperUnavailable(t *testing.T) {
	repo := t.TempDir()
	pathBin := filepath.Join(t.TempDir(), "kaneo")
	if err := os.WriteFile(pathBin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REPO_ROOT", repo)
	t.Setenv("PATH", filepath.Dir(pathBin))

	engine := &Engine{}
	if got := engine.kaneoBin(); got != pathBin {
		t.Fatalf("kaneo binary = %q, want PATH client %q", got, pathBin)
	}

	// An installed wrapper remains preferred over PATH, preserving the
	// repository's configured adapter semantics.
	wrapper := filepath.Join(repo, "bin", "herd-kaneo")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := engine.kaneoBin(); got != wrapper {
		t.Fatalf("kaneo binary = %q, want repository wrapper %q", got, wrapper)
	}
}

func TestKaneoBinIgnoresNonExecutableRepositoryWrapper(t *testing.T) {
	repo := t.TempDir()
	pathBin := filepath.Join(t.TempDir(), "kaneo")
	if err := os.WriteFile(pathBin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(repo, "bin", "herd-kaneo")
	if err := os.WriteFile(wrapper, []byte("not executable"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REPO_ROOT", repo)
	t.Setenv("PATH", filepath.Dir(pathBin))

	if got := (&Engine{}).kaneoBin(); got != pathBin {
		t.Fatalf("kaneo binary = %q, want PATH client %q", got, pathBin)
	}
}
