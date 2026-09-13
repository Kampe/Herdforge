package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

// FAC-764, through the PUBLIC CLI.
//
// `herd pool release <lease>` run from the orchestrator's working directory
// failed with "cannot change to '.herd/pool/pool-01'", while the identical
// release from the canonical checkout succeeded. The stored slot path is
// deliberately repository-relative (pool.go: "the state file is deliberately
// repo-relative so it can be moved with a worktree"), and ordinary Release
// handed it straight to `git -C`, which resolves against the CALLER'S working
// directory.
//
// This drives the real binary from a foreign caller worktree, with a real
// decoy at the caller-relative path, so the silent-wrong-tree direction is
// reachable and not merely argued.

type poolCLIFixture struct {
	bin      string
	root     string
	poolDir  string
	slotPath string
	foreign  string
	decoy    string
	decoyRef string
	mainRef  string
}

func poolCLIGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// herdPool runs the real CLI with the given working directory and returns
// stdout, stderr and the exit code, so a refusal is evidence rather than a
// fatal error.
func (f *poolCLIFixture) herdPool(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(f.bin, append([]string{"pool"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HERD_ROOT="+f.root, "HERD_REPO_ROOT="+f.root)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("herd pool %v: %v (%s)", args, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func newPoolCLIFixture(t *testing.T) *poolCLIFixture {
	t.Helper()
	f := &poolCLIFixture{bin: buildHerd(t)}
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	f.root = filepath.Join(base, "repo")
	poolCLIGit(t, base, "init", "--bare", "-q", "-b", "main", origin)
	poolCLIGit(t, base, "clone", "-q", origin, f.root)
	for _, kv := range [][2]string{
		{"user.email", "t@example.invalid"}, {"user.name", "t"},
		{"commit.gpgsign", "false"}, {"gc.auto", "0"},
	} {
		poolCLIGit(t, f.root, "config", kv[0], kv[1])
	}
	if err := os.MkdirAll(filepath.Join(f.root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".herd", "herd.yaml"),
		[]byte("version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// native.db is ignored evidence: `clean -fd` must not take it.
	if err := os.WriteFile(filepath.Join(f.root, ".gitignore"), []byte("native.db\n.herd/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	poolCLIGit(t, f.root, "add", ".gitignore")
	poolCLIGit(t, f.root, "commit", "-q", "-m", "base")
	poolCLIGit(t, f.root, "push", "-q", "origin", "main")
	f.mainRef = poolCLIGit(t, f.root, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(f.root, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	poolCLIGit(t, f.root, "add", "other.txt")
	poolCLIGit(t, f.root, "commit", "-q", "-m", "second")
	f.decoyRef = poolCLIGit(t, f.root, "rev-parse", "HEAD")
	poolCLIGit(t, f.root, "reset", "-q", "--hard", f.mainRef)

	f.poolDir = filepath.Join(f.root, ".herd", "pool")
	f.slotPath = filepath.Join(f.poolDir, "pool-01")

	// Pool creation through the REAL CLI with a RELATIVE pool root. This is
	// what persists a relative slot path; the test writes no pool state.
	if _, stderr, code := f.herdPool(t, f.root, "--pool-root", filepath.Join(".herd", "pool"), "--size", "1", "ensure"); code != 0 {
		t.Fatalf("herd pool ensure: exit %d (%s)", code, stderr)
	}
	if got := f.storedPath(t); got != filepath.Join(".herd", "pool", "pool-01") {
		t.Fatalf("pool creation did not persist a relative slot path, got %q; the defect is unreachable", got)
	}

	f.foreign = filepath.Join(base, "caller")
	poolCLIGit(t, f.root, "worktree", "add", "--detach", f.foreign, f.mainRef)
	f.decoy = filepath.Join(f.foreign, ".herd", "pool", "pool-01")
	if err := os.MkdirAll(filepath.Dir(f.decoy), 0o755); err != nil {
		t.Fatal(err)
	}
	poolCLIGit(t, f.root, "worktree", "add", "--detach", f.decoy, f.decoyRef)
	if err := os.WriteFile(filepath.Join(f.decoy, "marker.txt"), []byte("someone else's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *poolCLIFixture) slots(t *testing.T) []worktree.PoolSlot {
	t.Helper()
	slots, err := worktree.NewPool(f.root, f.poolDir, 1).Slots()
	if err != nil {
		t.Fatalf("slots: %v", err)
	}
	return slots
}

func (f *poolCLIFixture) storedPath(t *testing.T) string {
	t.Helper()
	slots := f.slots(t)
	if len(slots) != 1 {
		t.Fatalf("want one slot, got %d", len(slots))
	}
	return slots[0].Path
}

func (f *poolCLIFixture) lease(t *testing.T) string {
	t.Helper()
	stdout, stderr, code := f.herdPool(t, f.root, "--pool-root", f.poolDir, "lease", "review-cli")
	if code != 0 {
		t.Fatalf("herd pool lease: exit %d (%s)", code, stderr)
	}
	fields := strings.Fields(stdout)
	if len(fields) < 1 || fields[0] == "" {
		t.Fatalf("herd pool lease printed no lease id: %q", stdout)
	}
	return fields[0]
}

func (f *poolCLIFixture) decoyIntact(t *testing.T) {
	t.Helper()
	if head := poolCLIGit(t, f.decoy, "rev-parse", "HEAD"); head != f.decoyRef {
		t.Fatalf("the caller-relative decoy was reset: HEAD %s, want %s", head, f.decoyRef)
	}
	if _, err := os.Stat(filepath.Join(f.decoy, "marker.txt")); err != nil {
		t.Fatalf("the caller-relative decoy's untracked marker was destroyed: %v", err)
	}
}

// POSITIVE: the release succeeds from a foreign caller, and it is the OWNING
// slot that is reset.
func TestPoolReleaseCLIFromAForeignCallerReleasesTheOwningSlot(t *testing.T) {
	f := newPoolCLIFixture(t)
	leaseID := f.lease(t)

	evidence := filepath.Join(f.slotPath, "native.db")
	if err := os.WriteFile(evidence, []byte("retained evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	poolCLIGit(t, f.slotPath, "checkout", "-q", "--detach", f.decoyRef)

	_, stderr, code := f.herdPool(t, f.foreign, "--pool-root", f.poolDir, "release", leaseID)
	if code != 0 {
		t.Fatalf("herd pool release from a foreign caller: exit %d (%s)", code, stderr)
	}

	if head := poolCLIGit(t, f.slotPath, "rev-parse", "HEAD"); head != f.mainRef {
		t.Fatalf("owning slot HEAD = %s, want the base %s", head, f.mainRef)
	}
	got := f.slots(t)[0]
	if got.LeaseID != "" || got.LastReleaseLeaseID != leaseID {
		t.Fatalf("owning slot was not recorded as released: %+v", got)
	}
	if _, err := os.Stat(evidence); err != nil {
		t.Fatalf("ignored evidence in the released slot was destroyed: %v", err)
	}
	f.decoyIntact(t)
}

// A release aimed at a DIFFERENT pool frees nothing and touches neither pool.
func TestPoolReleaseCLIAgainstTheWrongPoolRootFreesNothing(t *testing.T) {
	f := newPoolCLIFixture(t)
	leaseID := f.lease(t)

	other := filepath.Join(t.TempDir(), "otherpool")
	if _, stderr, code := f.herdPool(t, f.root, "--pool-root", other, "--size", "1", "ensure"); code != 0 {
		t.Fatalf("ensure other pool: exit %d (%s)", code, stderr)
	}

	_, stderr, code := f.herdPool(t, f.foreign, "--pool-root", other, "release", leaseID)
	if code == 0 {
		t.Fatal("a lease from another pool was released")
	}
	if !strings.Contains(stderr, "lease not found") {
		t.Fatalf("stderr = %q, want lease not found", stderr)
	}
	if got := f.slots(t)[0]; got.LeaseID != leaseID {
		t.Fatalf("the owning pool's lease was disturbed: %+v", got)
	}
	f.decoyIntact(t)
}

// A retired lease identity cannot release its slot's new owner.
func TestPoolReleaseCLIRefusesARetiredLeaseIdentity(t *testing.T) {
	f := newPoolCLIFixture(t)
	first := f.lease(t)
	if _, stderr, code := f.herdPool(t, f.foreign, "--pool-root", f.poolDir, "release", first); code != 0 {
		t.Fatalf("first release: exit %d (%s)", code, stderr)
	}
	second := f.lease(t)
	if second == first {
		t.Fatalf("a reassigned slot reused the retired lease identity %q", first)
	}

	_, stderr, code := f.herdPool(t, f.foreign, "--pool-root", f.poolDir, "release", first)
	if code == 0 {
		t.Fatal("the retired lease identity released its slot's new owner")
	}
	if !strings.Contains(stderr, "lease not found") {
		t.Fatalf("stderr = %q, want lease not found", stderr)
	}
	if got := f.slots(t)[0]; got.LeaseID != second {
		t.Fatalf("the new owner's lease was disturbed: %+v", got)
	}
}
