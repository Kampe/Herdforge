package mergeadmit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Hermetic git fixtures. No network, no reliance on the developer's global git
// config, and no assumption about the ambient default branch name — CI runs on
// a clean machine where none of those are set the way a laptop has them.

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	// Identity and signing are configured LOCALLY so the test passes on a
	// machine with no global git identity and on one with commit signing
	// turned on globally (which would otherwise prompt or fail).
	for _, kv := range [][2]string{
		{"user.name", "herd test"},
		{"user.email", "herd@example.invalid"},
		{"commit.gpgsign", "false"},
		{"tag.gpgsign", "false"},
		{"gc.auto", "0"},
		// toolchild.RepositoryIdentity reads remote.origin.url to bind a
		// receipt to its repository. This is a git CONFIG read only — nothing
		// in this package fetches, so the fixture stays offline and CI never
		// touches the network.
		{"remote.origin.url", "git@github.com:Kampe/Herdforge-fixture.git"},
	} {
		run(t, dir, "git", "config", kv[0], kv[1])
	}
	return dir
}

// commit writes path=body and commits it, returning the new full sha.
func commit(t *testing.T, dir, path, body, msg string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	run(t, dir, "git", "add", path)
	run(t, dir, "git", "commit", "-q", "-m", msg)
	return revParse(t, dir, "HEAD")
}

// rewriteOnto replays commits (oldest first) onto base in a fresh branch,
// producing NEW object ids for identical content. This is the rebase/rewrite
// shape GitHub produced in the FAC-178 incident: the reviewed candidate sha no
// longer exists anywhere in the landed history.
func rewriteOnto(t *testing.T, dir, branch, base string, commits []string) string {
	t.Helper()
	run(t, dir, "git", "checkout", "-q", "-B", branch, base)
	for i, c := range commits {
		cmd := exec.Command("git", "cherry-pick", c)
		cmd.Dir = dir
		// A different committer date guarantees a different object id even
		// though the tree and patch are identical — exactly what a rebase does.
		cmd.Env = append(os.Environ(),
			"GIT_COMMITTER_DATE=2026-08-07T0"+string(rune('1'+i))+":00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cherry-pick %s: %v\n%s", c, err, out)
		}
	}
	return revParse(t, dir, "HEAD")
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(runOut(t, dir, "git", "rev-parse", rev))
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func runOut(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return string(out)
}
