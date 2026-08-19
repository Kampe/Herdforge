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
