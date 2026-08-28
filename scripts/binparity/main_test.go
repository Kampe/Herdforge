package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandResolvesDefaultSourceFromGitRoot(t *testing.T) {
	bin := buildBinParity(t)
	root := t.TempDir()
	herdRoot := initGitRepository(t, filepath.Join(root, "Herdforge"))
	sourceRoot := filepath.Join(root, "chainseer", "bin")
	writeExecutable(t, filepath.Join(sourceRoot, "probe"))
	writeManifest(t, filepath.Join(herdRoot, defaultManifest), "bin/probe")

	out, err := runBinParity(bin, herdRoot)
	if err != nil {
		t.Fatalf("shipped command did not resolve the default source from the git root: %v\n%s", err, out)
	}
	if !strings.Contains(out, "binparity: PASS") {
		t.Fatalf("shipped command output=%q, want PASS", out)
	}
}

func TestCommandHonorsExplicitSourceOverride(t *testing.T) {
	bin := buildBinParity(t)
	root := t.TempDir()
	herdRoot := initGitRepository(t, filepath.Join(root, "Herdforge"))
	sourceRoot := filepath.Join(root, "authorized-source")
	writeExecutable(t, filepath.Join(sourceRoot, "probe"))
	writeManifest(t, filepath.Join(herdRoot, defaultManifest), "bin/probe")

	out, err := runBinParity(bin, herdRoot, "CHAINSEER_BIN="+sourceRoot)
	if err != nil {
		t.Fatalf("shipped command rejected CHAINSEER_BIN override: %v\n%s", err, out)
	}
	if !strings.Contains(out, "binparity: PASS") {
		t.Fatalf("shipped command output=%q, want PASS", out)
	}
}

func TestCommandDistinguishesUnavailableSourceFromParityMismatch(t *testing.T) {
	bin := buildBinParity(t)

	t.Run("source unavailable", func(t *testing.T) {
		root := t.TempDir()
		herdRoot := initGitRepository(t, filepath.Join(root, "Herdforge"))
		writeManifest(t, filepath.Join(herdRoot, defaultManifest))

		out, err := runBinParity(bin, herdRoot)
		assertExitCode(t, err, exitSourceUnavailable)
		if !strings.Contains(out, "SOURCE_UNAVAILABLE") || strings.Contains(out, "PARITY_MISMATCH") {
			t.Fatalf("unavailable-source output has wrong class: %q", out)
		}
	})

	t.Run("optional source unavailable", func(t *testing.T) {
		root := t.TempDir()
		herdRoot := initGitRepository(t, filepath.Join(root, "Herdforge"))
		writeManifest(t, filepath.Join(herdRoot, defaultManifest))

		out, err := runBinParity(bin, herdRoot, "CHAINSEER_PARITY_OPTIONAL=1")
		if err != nil {
			t.Fatalf("optional unavailable source must skip: %v\n%s", err, out)
		}
		if !strings.Contains(out, "SKIP_SOURCE_UNAVAILABLE") || strings.Contains(out, "PARITY_MISMATCH") {
			t.Fatalf("optional-source output has wrong class: %q", out)
		}
	})

	t.Run("parity mismatch", func(t *testing.T) {
		root := t.TempDir()
		herdRoot := initGitRepository(t, filepath.Join(root, "Herdforge"))
		writeExecutable(t, filepath.Join(root, "chainseer", "bin", "probe"))
		writeManifest(t, filepath.Join(herdRoot, defaultManifest))

		out, err := runBinParity(bin, herdRoot)
		assertExitCode(t, err, exitParityMismatch)
		if !strings.Contains(out, "PARITY_MISMATCH") || strings.Contains(out, "SOURCE_UNAVAILABLE") {
			t.Fatalf("parity-mismatch output has wrong class: %q", out)
		}
	})
}

func buildBinParity(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "binparity")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build shipped binparity command: %v\n%s", err, out)
	}
	return bin
}

func initGitRepository(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", root, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return root
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, path string, entries ...string) {
	t.Helper()
	m := Manifest{
		Version:               1,
		Task:                  "FAC-309",
		SourceRoot:            "chainseer/bin",
		SourceRevision:        "fixture",
		SourceExecutableCount: len(entries),
	}
	for _, entry := range entries {
		m.Entries = append(m.Entries, Disposition{
			Path:        entry,
			Disposition: "chainseer_product_exemption",
			Rationale:   "test fixture",
		})
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runBinParity(bin, workDir string, overrides ...string) (string, error) {
	cmd := exec.Command(bin)
	cmd.Dir = workDir
	cmd.Env = cleanCommandEnv(overrides...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func cleanCommandEnv(overrides ...string) []string {
	drop := map[string]bool{
		"CHAINSEER_BIN":             true,
		"CHAINSEER_PARITY_OPTIONAL": true,
		"HERD_PROJECT_ROOT":         true,
		"HERD_ROOT":                 true,
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && !drop[key] {
			env = append(env, entry)
		}
	}
	return append(env, overrides...)
}

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("command error=%v, want exit status %d", err, want)
	}
	if got := exitErr.ExitCode(); got != want {
		t.Fatalf("exit status=%d, want %d", got, want)
	}
}

func TestValidateManifestDispositionRequirements(t *testing.T) {
	base := Manifest{Version: 1, Task: "FAC-309", SourceRoot: "chainseer/bin", SourceExecutableCount: 1, Entries: []Disposition{{Path: "bin/x", Disposition: "herdforge_command_replacement", Replacement: "herd status", Ticket: "FAC-304", Rationale: "ported control-plane command"}}}
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr bool
	}{
		{"valid", func(*Manifest) {}, false},
		{"missing replacement", func(m *Manifest) { m.Entries[0].Replacement = "" }, true},
		{"missing generic ticket", func(m *Manifest) {
			m.Entries[0].Disposition = "generic_capability"
			m.Entries[0].Replacement = ""
			m.Entries[0].Ticket = ""
		}, true},
		{"absolute path", func(m *Manifest) { m.Entries[0].Path = "/bin/x" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.mutate(&m)
			if (validateManifest(m) != nil) != tc.wantErr {
				t.Fatalf("validate error mismatch")
			}
		})
	}
}

func TestAuditSourceDetectsMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Task: "FAC-309", SourceRoot: "chainseer/bin", SourceExecutableCount: 0}
	if err := auditSource(dir, m); err == nil {
		t.Fatal("audit accepted an unmanifested executable")
	}
}
