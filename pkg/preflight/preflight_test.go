package preflight

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckMainOriginDivergence_FailsClosedWithExactCounts(t *testing.T) {
	root := t.TempDir()
	runGitDivergenceTest(t, root, "init", "-b", "main")
	runGitDivergenceTest(t, root, "config", "user.email", "test@example.com")
	runGitDivergenceTest(t, root, "config", "user.name", "Preflight Test")
	writeDivergenceTestFile(t, root, "base")
	runGitDivergenceTest(t, root, "add", ".")
	runGitDivergenceTest(t, root, "commit", "-m", "base")
	base := strings.TrimSpace(runGitDivergenceTest(t, root, "rev-parse", "HEAD"))

	// Build an origin/main commit from the common base, then make a different
	// local main commit from that same base. This proves both sides diverged.
	runGitDivergenceTest(t, root, "checkout", "-b", "remote-commit")
	writeDivergenceTestFile(t, root, "remote")
	runGitDivergenceTest(t, root, "add", ".")
	runGitDivergenceTest(t, root, "commit", "-m", "remote")
	runGitDivergenceTest(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGitDivergenceTest(t, root, "checkout", "-B", "main", base)
	writeDivergenceTestFile(t, root, "local")
	runGitDivergenceTest(t, root, "add", ".")
	runGitDivergenceTest(t, root, "commit", "-m", "local")

	report, err := CheckMainOriginDivergence(root)
	if err == nil {
		t.Fatal("diverged main and origin/main were accepted")
	}
	if report.LocalAhead != 1 || report.RemoteAhead != 1 {
		t.Fatalf("report = %+v, want local ahead=1 and origin ahead=1", report)
	}
	if !strings.Contains(err.Error(), "main is 1 commit(s) ahead and origin/main is 1 commit(s) ahead") {
		t.Fatalf("error = %q, want exact divergence counts", err)
	}
}

func TestCheckRootGitConfigAcceptsActualRoot(t *testing.T) {
	root := t.TempDir()
	runGitDivergenceTest(t, root, "init", "-b", "main")
	if err := CheckRootGitConfig(root); err != nil {
		t.Fatalf("actual Git root rejected: %v", err)
	}
}

func TestCheckRootGitConfigRejectsCoreWorktreeRedirect(t *testing.T) {
	root := t.TempDir()
	runGitDivergenceTest(t, root, "init", "-b", "main")
	redirect := filepath.Join(t.TempDir(), "other-worktree")
	runGitDivergenceTest(t, root, "config", "--local", "core.worktree", redirect)

	err := CheckRootGitConfig(root)
	if err == nil {
		t.Fatal("core.worktree redirect was accepted")
	}
	if !strings.Contains(err.Error(), "core.worktree") || !strings.Contains(err.Error(), redirect) {
		t.Fatalf("error = %q, want redirect diagnostic naming core.worktree and target", err)
	}
}

func TestCheckRootGitConfigRejectsCorruptedGitConfig(t *testing.T) {
	root := t.TempDir()
	runGitDivergenceTest(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := CheckRootGitConfig(root)
	if err == nil {
		t.Fatal("corrupted Git config was accepted")
	}
	if !strings.Contains(err.Error(), "inspect Git checkout") {
		t.Fatalf("error = %q, want corrupted-checkout diagnostic", err)
	}
}

func writeDivergenceTestFile(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "state.txt"), []byte(contents+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGitDivergenceTest(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

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

func TestCheckGoToolchain_ReportsExportedGOROOTMismatch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/opt/homebrew/bin:/usr/bin", "GOROOT=" + root}
	probe := func(probeEnv []string, args ...string) (string, error) {
		if _, ok := lookupEnv(probeEnv, "GOROOT"); ok {
			t.Fatal("probe environment still contains GOROOT")
		}
		if strings.Join(args, " ") == "env GOROOT" {
			return "/opt/homebrew/Cellar/go/1.26.6/libexec\n", nil
		}
		return "go version go1.26.6 darwin/arm64\n", nil
	}

	err := checkGoToolchain(env, probe)
	if err == nil {
		t.Fatal("mismatched exported GOROOT was accepted")
	}
	message := err.Error()
	for _, want := range []string{"GOROOT", "go1.26.2", "go1.26.6", "env -u GOROOT make build"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to contain %q", message, want)
		}
	}
}

func TestCheckGoToolchain_AllowsUnsetAndMatchingGOROOT(t *testing.T) {
	probe := func(_ []string, args ...string) (string, error) {
		if strings.Join(args, " ") == "env GOROOT" {
			return "/opt/homebrew/Cellar/go/1.26.6/libexec\n", nil
		}
		return "go version go1.26.6 darwin/arm64\n", nil
	}

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "unset", env: []string{"PATH=/usr/bin"}},
		{name: "matching", env: []string{"PATH=/usr/bin", "GOROOT=/opt/homebrew/Cellar/go/1.26.6/libexec"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkGoToolchain(tc.env, probe); err != nil {
				t.Fatalf("checkGoToolchain() error = %v", err)
			}
		})
	}
}

// FAC-700: a file git is configured to IGNORE cannot reach the repository, so
// it cannot leak a host path into it. The scout-planner worktree preserved 72
// untracked guard/receipt artifacts whose contents legitimately record where a
// snapshot lives, and preflight failed the whole worktree on them -- blocking
// CHA-3176/CHA-3180 while the orchestrator checkout, holding no such artifacts,
// passed.
func TestIgnoredRuntimeArtifactDoesNotFailTheBoundaryCheck(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	// A receipt doing its job: it records an absolute path on purpose.
	mustWrite(t, filepath.Join(dir, "receipts"), "CHA-15.identity.json",
		`{"ref":"CHA-15","prewrite_snapshot":"/Users/kampe/Personal/scout-planner/receipts/CHA-15.snapshot.json"}`)
	mustWrite(t, dir, ".gitignore", "receipts/\n")

	if err := CheckWorktreeBoundary(dir); err != nil {
		t.Fatalf("an ignored runtime artifact failed the boundary check: %v", err)
	}
}

func TestTrackedFileWithAbsolutePathStillFails(t *testing.T) {
	// The gate must keep doing its job. A file that CAN be committed is
	// exactly what the boundary check exists for.
	dir := t.TempDir()
	gitInit(t, dir)
	mustWrite(t, dir, "config.json", `{"path":"/Users/kampe/Personal/thing"}`)

	if err := CheckWorktreeBoundary(dir); err == nil {
		t.Fatal("a committable file with an absolute path passed the boundary check")
	}
}

func TestUnignoredUntrackedFileStillFails(t *testing.T) {
	// Untracked but NOT ignored means it can still be added, so it stays in
	// scope. Only an explicit ignore rule exempts a file.
	dir := t.TempDir()
	gitInit(t, dir)
	mustWrite(t, dir, "stray.json", `{"path":"/Users/kampe/Personal/thing"}`)

	if err := CheckWorktreeBoundary(dir); err == nil {
		t.Fatal("an untracked, unignored file with an absolute path passed the boundary check")
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func mustWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
