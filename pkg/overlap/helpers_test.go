package overlap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func createTestGitRepo(t *testing.T, initialFile string) string {
	t.Helper()
	dir := t.TempDir()
	for _, cmd := range []string{
		"git init -b main",
		"git config user.email test@test.com",
		"git config user.name test",
		"git config commit.gpgSign false",
		"git config tag.gpgSign false",
	} {
		c := exec.Command("bash", "-c", cmd)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
	}
	if initialFile != "" {
		commitFile(t, dir, initialFile, "base\n", "initial")
	}
	return dir
}

func commitFile(t *testing.T, dir, filename, content, msg string) {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filename, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	c := exec.Command("git", "add", filename)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", filename, err, out)
	}
	c = exec.Command("git", "commit", "-m", msg)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit %s: %v\n%s", msg, err, out)
	}
}

func createBranch(t *testing.T, dir, name string) {
	t.Helper()
	c := exec.Command("git", "checkout", "-b", name)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", name, err, out)
	}
}

func checkoutBranch(t *testing.T, dir, name string) {
	t.Helper()
	c := exec.Command("git", "checkout", name)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout %s: %v\n%s", name, err, out)
	}
}

// setOriginMain points refs/remotes/origin/main at the given rev (default main).
// The real script requires a fetched origin/main remote-tracking ref; tests
// fabricate one so --selftest and the diff base are well defined.
func setOriginMain(t *testing.T, dir, rev string) {
	t.Helper()
	if rev == "" {
		rev = "main"
	}
	c := exec.Command("git", "rev-parse", rev)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	sha := strings.TrimSpace(string(out))
	c = exec.Command("git", "update-ref", "refs/remotes/origin/main", sha)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/main: %v\n%s", err, out)
	}
}

// setHerePoints current HEAD at rev (like git reset --hard) so a branch tip
// can be left dangling for the park-triplicate tests.
func forceRev(t *testing.T, dir, rev string) {
	t.Helper()
	c := exec.Command("git", "reset", "--hard", rev)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git reset --hard %s: %v\n%s", rev, err, out)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func listBranches(t *testing.T, dir string) []string {
	t.Helper()
	c := exec.Command("git", "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}