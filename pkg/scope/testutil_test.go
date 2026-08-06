package scope

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo is a real, throwaway git working copy used to exercise Resolver
// against actual git subprocess output rather than fabricated data.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir := t.TempDir()
	r := &testRepo{t: t, dir: dir}
	r.run("init", "--quiet", "--initial-branch=main")
	r.run("config", "user.email", "scope-test@example.com")
	r.run("config", "user.name", "Scope Test")
	// A worktree-local override so this fixture never depends on (or is
	// broken by) the operator's global signing configuration.
	r.run("config", "commit.gpgsign", "false")
	r.run("config", "tag.gpgsign", "false")
	return r
}

func (r *testRepo) run(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes path with content and commits it, returning the new HEAD sha.
func (r *testRepo) commit(path, content, msg string) string {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.run("add", path)
	r.run("commit", "--quiet", "-m", msg)
	return r.run("rev-parse", "HEAD")
}

// setOriginMain simulates having already fetched origin/main at sha, without
// requiring a real remote — Resolver tolerates fetch failing offline as long
// as origin/<branch> already exists locally.
func (r *testRepo) setOriginMain(sha string) {
	r.t.Helper()
	r.run("update-ref", "refs/remotes/origin/main", sha)
}

func (r *testRepo) checkoutNewBranch(name, at string) {
	r.t.Helper()
	r.run("checkout", "--quiet", "-b", name, at)
}

func (r *testRepo) resolver() *Resolver {
	r.t.Helper()
	res, err := NewResolver(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}
