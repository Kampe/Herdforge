package lifecycle

import (
	"os"
	"path/filepath"
	"slices"
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

	// A retired repository wrapper must not replace the supported stock client.
	wrapper := filepath.Join(repo, "bin", "herd-kaneo")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := engine.kaneoBin(); got != pathBin {
		t.Fatalf("kaneo binary = %q after wrapper install, want stock PATH client %q", got, pathBin)
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

func TestKaneoListArgsUsesCurrentPATHClientShape(t *testing.T) {
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

	got := (&Engine{KaneoBin: pathBin}).kaneoListArgs()
	want := []string{"task", "list", "--json", "--all"}
	if !slices.Equal(got, want) {
		t.Fatalf("kaneo list args = %q, want %q", got, want)
	}
}

func TestKaneoListArgsIgnoresRetiredRepositoryWrapper(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(repo, "bin", "herd-kaneo")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REPO_ROOT", repo)

	got := (&Engine{}).kaneoListArgs()
	want := []string{"task", "list", "--json", "--all"}
	if !slices.Equal(got, want) {
		t.Fatalf("kaneo list args = %q, want stock shape %q", got, want)
	}
}
