package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-832, THROUGH THE ACTUAL ENTRY.
//
// runPoolReview is the production `herd review <ref> --pool` route. It reads
// os.Args, so this saves and restores them and drives the real function: the
// real config load, the real capacity gate, the real candidate resolution, the
// real pool Ensure/Lease, the real slot pin, the real surface symlink and the
// real packet. --no-launch returns before Herdr is consulted, so no external
// host or provider call happens on this path.
//
// The ONLY seam is loadReviewTaskProvider, because the "memory" provider type
// constructs an EMPTY provider and a fixture otherwise cannot make the entry's
// own task lookup resolve. Everything the card is about — candidate identity,
// pool readiness, carrier allocation — runs for real.

const entryRef = "FAC-9320"

// entryFixture builds a real repository whose candidate commit carries the
// reviewer contract the entry requires, and returns root and the candidate sha.
func entryFixture(t *testing.T) (root, sha string) {
	t.Helper()
	root = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main", ".")
	git("config", "commit.gpgsign", "false")
	git("config", "gc.auto", "0")
	// The entry refuses a candidate whose own tree does not track the reviewer
	// contract and verdict template, so the fixture commits both.
	write(reviewerContractPath, "# reviewer contract (fixture)\n")
	write(verdictTemplatePath, "# verdict template (fixture)\n")
	write("README.md", "fixture\n")
	git("add", ".")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	write("candidate.txt", "candidate work\n")
	git("add", "candidate.txt")
	git("commit", "-qm", "candidate")
	sha = git("rev-parse", "HEAD")
	if sha == base {
		t.Fatal("fixture produced no distinct candidate commit")
	}
	// The entry refuses to prepare from a dirty shared checkout.
	if dirty := git("status", "--porcelain"); dirty != "" {
		t.Fatalf("fixture checkout is dirty: %s", dirty)
	}
	// HERMETICITY, using this repository's own production-supported overrides.
	// The REAL admission gate, the REAL absence handling and the REAL entry all
	// still run; only the state they touch is test-owned.
	//
	// cmd/herd's TestMain already calls laneenv.Strip and laneenv.Isolate, so
	// HERD_ROLE/HERD_ROOT/HERD_WORKSPACE/HERDR_* pane vars and HERD_STATE_DIR
	// are handled package-wide. Three things it does not cover:
	//
	//  1. the admission lease. admissionLeasePath honours
	//     HERD_ADMISSION_LEASE_PATH and otherwise falls back to
	//     $HOME/.herd/state/admission.lease — a HOST-WIDE file. Left alone, this
	//     test would contend with, or take, the developer's or another runner's
	//     real lease. Pointing it into the fixture keeps the exclusive-lease
	//     logic exactly as production runs it, against state this test owns.
	//  2. HOME itself, so nothing that falls back to it can reach the
	//     developer's state.
	//  3. the herdr CLI. liveReviewerFor calls herdr.AgentList, which on a host
	//     with herdr installed would list the OPERATOR'S LIVE FLEET.
	//     herdr.NoLiveEnv is the FAC-145 guard for exactly this: with it set and
	//     no binary override, binaryPath refuses rather than reaching the live
	//     fleet, so the entry takes its genuine missing-Herdr branch. That is the
	//     real code path, not a bypass: --no-launch returns before Herdr is
	//     required, and liveReviewerFor already treats an unavailable roster as
	//     "not known", which is what a host without herdr does.
	home := filepath.Join(root, "fixture-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(root, ".herd", "state", "admission.lease"))
	t.Setenv(herdr.NoLiveEnv, "1")
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	// Keep the real capacity gate in play but guarantee it admits on any host.
	t.Setenv("HERD_MEM_FLOOR_MIB", "1")
	t.Setenv("HERD_REVIEWER_RSS_MIB", "1")
	return root, sha
}

// entryConfig writes the fixture's herd.yaml and installs a seeded provider for
// the entry's own lookup.
func entryConfig(t *testing.T, root string) {
	t.Helper()
	const projectID = "fixture-project"
	body := "version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n  project_id: " + projectID + "\n"
	p := filepath.Join(root, ".herd", "herd.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	seeded := provider.NewMemoryProvider()
	seeded.AddTask(&provider.Task{
		ID: "fixture-1", Ref: entryRef, Title: "carrier lifecycle fixture",
		Status: provider.StatusInProgress, ProjectID: projectID,
	})
	previous := loadReviewTaskProvider
	loadReviewTaskProvider = func(*config.Config) (provider.TaskProvider, error) { return seeded, nil }
	t.Cleanup(func() { loadReviewTaskProvider = previous })
}

// runEntry drives the real entry with real argv.
func runEntry(t *testing.T, sha, base string) error {
	t.Helper()
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = []string{"herd", "review", entryRef,
		"--pool", "--no-launch",
		"--sha", sha,
		"--base", base,
		"--builder-family", "anthropic",
	}
	return runPoolReview(entryRef)
}

func entryCarriers(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".herd", "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// THE ACCEPTANCE: fully pinned no-launch preparation is ready for the exact
// candidate in the LEASED POOL SLOT, and leaves no redundant carrier.
func TestPoolNoLaunchEntryPreparesTheLeasedSlotWithoutACarrier(t *testing.T) {
	root, sha := entryFixture(t)
	entryConfig(t, root)
	base := strings.TrimSpace(entryGitOutput(t, root, "rev-parse", sha+"^"))

	if err := runEntry(t, sha, base); err != nil {
		t.Fatalf("fully pinned no-launch preparation failed: %v", err)
	}

	// THE REGRESSION: no redundant carrier anywhere under .herd/worktrees.
	if got := entryCarriers(t, root); len(got) != 0 {
		t.Fatalf("no-launch preparation left an unowned carrier: %v", got)
	}

	// READINESS: the leased pool slot is pinned to the exact candidate.
	slot := filepath.Join(root, ".herd", "pool", "pool-01")
	if head := strings.TrimSpace(entryGitOutput(t, slot, "rev-parse", "HEAD")); head != sha {
		t.Fatalf("leased slot HEAD = %s, want the exact candidate %s", head, sha)
	}

	// The lease is HELD, which is what --no-launch promises the later dispatch.
	state, err := os.ReadFile(filepath.Join(root, ".herd", "pool", "pool.json"))
	if err != nil {
		t.Fatalf("pool state unreadable: %v", err)
	}
	if !strings.Contains(string(state), "\"lease_id\"") {
		t.Fatalf("no-launch preparation released the lease it must hold: %s", state)
	}
}

// RETRY must not accumulate a carrier either.
func TestPoolNoLaunchEntryRetryLeavesNoCarrier(t *testing.T) {
	root, sha := entryFixture(t)
	entryConfig(t, root)
	base := strings.TrimSpace(entryGitOutput(t, root, "rev-parse", sha+"^"))

	if err := runEntry(t, sha, base); err != nil {
		t.Fatalf("first preparation failed: %v", err)
	}
	// A second attempt either succeeds or refuses; neither may allocate.
	_ = runEntry(t, sha, base)

	if got := entryCarriers(t, root); len(got) != 0 {
		t.Fatalf("retry accumulated a carrier: %v", got)
	}
}

// A FAILING preparation must not leave one behind either: an unresolvable
// candidate is refused after the same candidate-resolution step.
func TestPoolNoLaunchEntryFailureLeavesNoCarrier(t *testing.T) {
	root, sha := entryFixture(t)
	entryConfig(t, root)
	base := strings.TrimSpace(entryGitOutput(t, root, "rev-parse", sha+"^"))
	missing := strings.Repeat("c", 40)

	if err := runEntry(t, missing, base); err == nil {
		t.Fatal("an unresolvable candidate must refuse")
	}
	if got := entryCarriers(t, root); len(got) != 0 {
		t.Fatalf("a refused preparation left a carrier: %v", got)
	}
}

func entryGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return string(out)
}
