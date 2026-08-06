package overlap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

func createTestGitRepo(t *testing.T, initialFile string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgSign", "false"},
		{"config", "tag.gpgSign", "false"},
	} {
		c := testgit.Command(dir, args...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
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
	c := testgit.Command(dir, "add", filename)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", filename, err, out)
	}
	c = testgit.Command(dir, "commit", "-m", msg)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit %s: %v\n%s", msg, err, out)
	}
}

func createBranch(t *testing.T, dir, name string) {
	t.Helper()
	c := testgit.Command(dir, "checkout", "-b", name)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", name, err, out)
	}
}

func checkoutBranch(t *testing.T, dir, name string) {
	t.Helper()
	c := testgit.Command(dir, "checkout", name)
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
	c := testgit.Command(dir, "rev-parse", rev)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	sha := strings.TrimSpace(string(out))
	c = testgit.Command(dir, "update-ref", "refs/remotes/origin/main", sha)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/main: %v\n%s", err, out)
	}
}

// setHerePoints current HEAD at rev (like git reset --hard) so a branch tip
// can be left dangling for the park-triplicate tests.
func forceRev(t *testing.T, dir, rev string) {
	t.Helper()
	c := testgit.Command(dir, "reset", "--hard", rev)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git reset --hard %s: %v\n%s", rev, err, out)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := testgit.Command(dir, args...)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func listBranches(t *testing.T, dir string) []string {
	t.Helper()
	c := testgit.Command(dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
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
