package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ftpdHerdWorktreesRepo builds a hermetic repo with origin/main and two
// worktrees ahead of main, one touching a shared file.
func ftpdHerdWorktreesRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Init bare repo that looks like a remote
	runGitT(t, dir, "init", "--bare", "bare.git")

	// Clone into the "principal" repo (this is what herd worktrees scans)
	principal := filepath.Join(dir, "repo")
	runGitT(t, principal, "config", "user.email", "t@kampe.kluster")
	runGitT(t, principal, "config", "user.name", "FAC-104 Test")
	runGitT(t, principal, "config", "commit.gpgSign", "false")
	runGitT(t, principal, "config", "tag.gpgSign", "false")
	writeRepoFile(t, principal, "README.md", "root\n")

	// Push main so origin/main exists
	runGitT(t, principal, "push", "origin", "main")

	// Create worktree branch: fac-101 (ahead by 2 commits, no collision)
	runGitT(t, principal, "branch", "herd/fac-101", "main")
	wt101 := filepath.Join(dir, "wt101")
	runGitT(t, principal, "worktree", "add", wt101, "herd/fac-101")
	runGitT(t, wt101, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt101, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt101, "config", "commit.gpgSign", "false")
	runGitT(t, wt101, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt101, "pkg/unique1.go", "package pkg\n// fac-101 unique\n")
	writeRepoFile(t, wt101, "pkg/unique2.go", "package pkg\n// fac-101 also\n")

	// Create worktree branch: fac-102 (ahead by 1, touches shared.go)
	runGitT(t, principal, "branch", "herd/fac-102", "main")
	wt102 := filepath.Join(dir, "wt102")
	runGitT(t, principal, "worktree", "add", wt102, "herd/fac-102")
	runGitT(t, wt102, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt102, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt102, "config", "commit.gpgSign", "false")
	runGitT(t, wt102, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt102, "pkg/shared.go", "package pkg\n// fac-102\n")

	// Create worktree branch: fac-103 (dirty, touches shared.go differently)
	runGitT(t, principal, "branch", "herd/fac-103", "main")
	wt103 := filepath.Join(dir, "wt103")
	runGitT(t, principal, "worktree", "add", wt103, "herd/fac-103")
	runGitT(t, wt103, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt103, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt103, "config", "commit.gpgSign", "false")
	runGitT(t, wt103, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt103, "pkg/shared.go", "package pkg\n// fac-103\n")
	// Dirty modification on top
	if err := os.WriteFile(filepath.Join(wt103, "pkg/dirty.go"), []byte("package pkg\n// dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a locked worktree
	runGitT(t, principal, "branch", "herd/locked", "main")
	wtLocked := filepath.Join(dir, "wtLocked")
	runGitT(t, principal, "worktree", "add", wtLocked, "herd/locked")
	runGitT(t, principal, "worktree", "lock", wtLocked)

	return principal
}

func TestHerdWorktreesCLIBasic(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 3 for collisions, got success")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("expected exit code 3, got %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "ahead=") {
		t.Errorf("expected ahead= column in output, got:\n%s", s)
	}
	if !strings.Contains(s, "dirty=") {
		t.Errorf("expected dirty= column in output, got:\n%s", s)
	}
	if !strings.Contains(s, "COLLISIONS:") {
		t.Errorf("expected COLLISIONS section, got:\n%s", s)
	}
	if !strings.Contains(s, "pkg/shared.go") {
		t.Errorf("expected pkg/shared.go in collisions, got:\n%s", s)
	}
}

func TestHerdWorktreesCLIJSON(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for --json, got %v\n%s", err, out)
	}
	var rows []Row
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("expected valid JSON: %v\n%s", err, out)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one row")
	}
	foundShared := false
	for _, r := range rows {
		if r.Branch == "" {
			t.Error("every row must have a branch")
		}
		if r.Worktree == "" {
			t.Error("every row must have a worktree path")
		}
		if r.Head == "" || r.Head == "?" {
			t.Error("every row must have a non-empty HEAD")
		}
		for _, f := range r.Files {
			if f == "pkg/shared.go" {
				foundShared = true
			}
		}
	}
	if !foundShared {
		t.Errorf("expected shared.go in files, got:\n%s", out)
	}
}

func TestHerdWorktreesCLIFiles(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--files")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 3 for collisions")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("expected exit code 3, got %v\n%s", err, out)
	}
	s := string(out)
	// Each branch with files should have a -- heading
	if !strings.Contains(s, "-- herd/fac-101") {
		t.Errorf("expected -- herd/fac-101 files section, got:\n%s", s)
	}
	if !strings.Contains(s, "pkg/unique1.go") {
		t.Errorf("expected unique1.go in files, got:\n%s", s)
	}
}

func TestHerdWorktreesCLIJSONFiles(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--json", "--files")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for --json --files, got %v\n%s", err, out)
	}
	var rows []Row
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("expected valid JSON: %v\n%s", err, out)
	}
	// --files flag should be ignored when --json is set; files are always included in JSON
	if len(rows) == 0 {
		t.Fatal("expected rows")
	}
}

func TestHerdWorktreesCLIHelp(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--help")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for --help, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "herd-worktrees") {
		t.Errorf("expected herd-worktrees header, got:\n%s", out)
	}
}

func TestHerdWorktreesCLIUnknownArg(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--bogus")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 2 for unknown arg")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
}

func TestHerdWorktreesCLINoOriginMain(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@kampe.kluster")
	runGitT(t, dir, "config", "user.name", "FAC-104 Test")
	runGitT(t, dir, "config", "commit.gpgSign", "false")
	runGitT(t, dir, "config", "tag.gpgSign", "false")
	writeRepoFile(t, dir, "README.md", "root\n")

	cmd := exec.Command(binary, "worktrees")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 1 without origin/main")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "origin/main not found") {
		t.Errorf("expected origin/main not found, got:\n%s", out)
	}
}

func TestHerdWorktreesCLIVanishedWorktree(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	// Find a worktree path and delete its directory to simulate vanished
	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("initial worktrees failed: %v\n%s", err, out)
	}
	var before []Row
	if err := json.Unmarshal(out, &before); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}

	// Delete the first non-principal worktree directory
	for _, r := range before {
		if r.Branch != "main" && r.Worktree != dir {
			if err := os.RemoveAll(r.Worktree); err != nil {
				t.Fatal(err)
			}
			break
		}
	}

	// Re-run should not crash, should have fewer rows
	cmd2 := exec.Command(binary, "worktrees", "--json")
	cmd2.Dir = dir
	out2, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("second worktrees failed: %v\n%s", err, out2)
	}
	var after []Row
	if err := json.Unmarshal(out2, &after); err != nil {
		t.Fatalf("invalid JSON after vanished: %v\n%s", err, out2)
	}
	if len(after) >= len(before) {
		t.Errorf("expected fewer rows after deleting a worktree dir: before=%d after=%d", len(before), len(after))
	}
}

func TestHerdWorktreesCLILocked(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worktrees failed: %v\n%s", err, out)
	}
	var rows []Row
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	foundLocked := false
	for _, r := range rows {
		if r.Locked {
			foundLocked = true
			break
		}
	}
	if !foundLocked {
		t.Error("expected a locked worktree in output")
	}
}

func TestHerdWorktreesCLINoCollisions(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()

	// Build a repo with one worktree ahead but no shared files
	runGitT(t, dir, "init", "--bare", "bare.git")

	principal := filepath.Join(dir, "repo")
	runGitT(t, dir, "clone", "bare.git", "repo")
	runGitT(t, principal, "config", "user.email", "t@kampe.kluster")
	runGitT(t, principal, "config", "user.name", "FAC-104 Test")
	runGitT(t, principal, "config", "commit.gpgSign", "false")
	runGitT(t, principal, "config", "tag.gpgSign", "false")
	writeRepoFile(t, principal, "README.md", "root\n")
	runGitT(t, principal, "push", "origin", "main")

	runGitT(t, principal, "branch", "herd/fac-x", "main")
	wt := filepath.Join(dir, "wtx")
	runGitT(t, principal, "worktree", "add", wt, "herd/fac-x")
	runGitT(t, wt, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt, "config", "commit.gpgSign", "false")
	runGitT(t, wt, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt, "pkg/only_me.go", "package pkg\n// only on this branch\n")

	cmd := exec.Command(binary, "worktrees")
	cmd.Dir = principal
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for no collisions, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "COLLISIONS: none") {
		t.Errorf("expected COLLISIONS: none, got:\n%s", out)
	}
}

func TestHerdWorktreesCLIUnknownPositionalArg(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "extra-arg")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 2 for unknown positional arg")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "unknown arg") {
		t.Errorf("expected 'unknown arg', got:\n%s", out)
	}
}
