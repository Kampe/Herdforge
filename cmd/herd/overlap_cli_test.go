package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit executes git in dir and fails the test on error.
func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// writeRepoFile writes a file in dir and commits it.
func writeRepoFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "add", ".")
	runGitT(t, dir, "commit", "-m", "add "+path)
}

// ftpdHerdOverlapRepo builds a hermetic repo with two branches ahead of
// origin/main, both touching the same shared file, and returns its dir.
func ftpdHerdOverlapRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.email", "test@kampe.kluster")
	runGitT(t, dir, "config", "user.name", "FAC-71 Test")
	runGitT(t, dir, "config", "commit.gpgSign", "false")
	runGitT(t, dir, "config", "tag.gpgSign", "false")

	// Base main.
	writeRepoFile(t, dir, "README.md", "root\n")

	// Two branches ahead of main, both teaching the same file.
	runGitT(t, dir, "checkout", "-b", "feat-sharing")
	writeRepoFile(t, dir, "pkg/shared.go", "package pkg\n// branch A\n")
	runGitT(t, dir, "checkout", "main")
	runGitT(t, dir, "checkout", "-b", "feat-b")
	writeRepoFile(t, dir, "pkg/shared.go", "package pkg\n// branch B\n")

	// origin/main snapshots the original main tip.
	runGitT(t, dir, "checkout", "main")
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", "main")
	return dir
}

func TestOverlapCLIHot(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdOverlapRepo(t)

	cmd := exec.Command(binary, "overlap")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 1 for overlap, got success")
	}
	// ExitCode covers the exit-contract: 1 = overlap found.
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %v output\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "file(s) edited by 2+ unmerged branches (2 scanned)") {
		t.Errorf("missing overlap header, got:\n%s", s)
	}
	if !strings.Contains(s, "pkg/shared.go") {
		t.Errorf("expected shared file listed, got:\n%s", s)
	}
}

func TestHerdOverlapCLIMinHigher(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdOverlapRepo(t)

	cmd := exec.Command(binary, "overlap", "--min", "3")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 with --min 3 (only 2 branches overlap): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no file is being edited by 3+ unmerged branches") {
		t.Errorf("expected no-overlap message, got:\n%s", out)
	}
}

func TestHerdOverlapCLIJSON(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdOverlapRepo(t)

	cmd := exec.Command(binary, "overlap", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("json snapshot should exit 0: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, `"file":"pkg/shared.go"`) {
		t.Errorf("expected shared.go in json, got:\n%s", s)
	}
	if !strings.Contains(s, `"branches":2`) {
		t.Errorf("expected branches:2, got:\n%s", s)
	}
	if !strings.Contains(s, `"owners":[`) {
		t.Errorf("expected owners array, got:\n%s", s)
	}
}

func TestHerdOverlapCLINoSymbols(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdOverlapRepo(t)

	cmd := exec.Command(binary, "overlap", "--symbols")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 when no shared symbols: %v\n%s", err, out)
	}
	// Root has no .ts/.sh/.zsh files, so nothing is extracted. The message
	// proves the path ran rather than vacuously returned.
	if !strings.Contains(string(out), "no symbol is being added on 2+ unmerged tips") {
		t.Errorf("expected symbols no-op message, got:\n%s", out)
	}
}

func TestHerdOverlapCLISelftest(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdOverlapRepo(t)

	cmd := exec.Command(binary, "overlap", "--selftest")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected selftest pass: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "herd-overlap --selftest PASS") {
		t.Errorf("expected PASS, got:\n%s", out)
	}
}

func TestHerdOverlapCLISelftestNoRef(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@kamdg.kluster")
	runGitT(t, dir, "config", "user.name", "FAC- Test")
	runGitT(t, dir, "config", "commit.gpgSign", "false")
	writeRepoFile(t, dir, "README.md", "root\n")

	cmd := exec.Command(binary, "overlap", "--selftest")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected failure for selftest without origin/main")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "FAIL: no origin/main") {
		t.Errorf("expected FAIL: no origin/main, got:\n%s", out)
	}
}

func TestHerdOverlapCLINoOriginMain(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@kamdg.kluster")
	runGitT(t, dir, "config", "user.name", "FAC- T")
	runGitT(t, dir, "config", "commit.gpgSign", "false")
	writeRepoFile(t, dir, "README.md", "root\n")

	cmd := exec.Command(binary, "overlap")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 3 without origin/main")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("expected exit code 3, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "herd-overlap: no origin/main; run git fetch origin main") {
		t.Errorf("expected fetch hint, got:\n%s", out)
	}
}

func TestHerdOverlapCLIUnknownArg(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdOverlapRepo(t)

	cmd := exec.Command(binary, "overlap", "--bogus")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 2 for unknown arg")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
}

func TestHerdOverlapCLISymbolsHot(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@herd.kluster")
	runGitT(t, dir, "config", "user.name", "FAC- T")
	runGitT(t, dir, "config", "commit.gpgSign", "false")
	writeRepoFile(t, dir, "base.ts", "export const base = 1;\n")

	runGitT(t, dir, "checkout", "-b", "feat-sym-a")
	writeRepoFile(t, dir, "pkg/symA.ts", "export function reconcile() { return 1 }\n")
	runGitT(t, dir, "checkout", "main")
	runGitT(t, dir, "checkout", "-b", "feat-sym-b")
	writeRepoFile(t, dir, "pkg/symB.ts", "export function reconcile() { return 2 }\n")
	runGitT(t, dir, "checkout", "main")
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", "main")

	cmd := exec.Command(binary, "overlap", "--symbols")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 1 when symbols collide")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "1 symbol(s) added on 2+ unmerged tips in different files") {
		t.Errorf("expected symbols hot header, got:\n%s", s)
	}
	if !strings.Contains(s, "reconcile") {
		t.Errorf("expected reconcile symbol, got:\n%s", s)
	}

	// JSON variant must also exit 1 with refs present.
	cmd = exec.Command(binary, "overlap", "--symbols", "--json")
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 1 for symbols --json hot")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit code 1 for symbols json, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"symbol":"reconcile"`) {
		t.Errorf("expected reconcile in symbols json, got:\n%s", out)
	}
	if !strings.Contains(string(out), `"location":`) {
		t.Errorf("expected locations in symbols json, got:\n%s", out)
	}
}
