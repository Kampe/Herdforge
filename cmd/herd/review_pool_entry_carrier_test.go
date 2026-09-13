package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-832, THROUGH THE ACTUAL ENTRY.
//
// runPoolReview is the production `herd review <ref> --pool` route. It reads
// os.Args, so this saves and restores them and drives the real function: the
// real config load, the real capacity gate, the real candidate resolution, the
// real pool Ensure/Lease, the real slot pin, the real pane census, the real
// surface symlink and the real packet. --no-launch returns before a reviewer is
// LAUNCHED, but Herdr IS consulted before that: the pane census runs after the
// lease and refuses on error. That boundary is an owned fixture CLI, never the
// operator's.
//
// The ONLY seam is loadReviewTaskProvider, because the "memory" provider type
// constructs an EMPTY provider and a fixture otherwise cannot make the entry's
// own task lookup resolve. Everything the card is about — candidate identity,
// pool readiness, carrier allocation — runs for real.

const entryRef = "FAC-9320"

// entryFixture builds a real repository whose candidate commit carries the
// reviewer contract the entry requires, and returns root and the candidate sha.
func entryFixture(t *testing.T) (root, sha string, herdrCalls func() []string) {
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
	// HERMETICITY. cmd/herd's TestMain already calls laneenv.Strip and
	// laneenv.Isolate, which cover HERD_ROLE/HERD_ROOT/HERD_WORKSPACE, the
	// HERDR_ pane prefix and HERD_STATE_DIR. They do NOT cover HERD_HERDR_BIN,
	// the admission lease path, or HOME.
	//
	// The entry DOES consult Herdr before its --no-launch return:
	// evictPoolSlotOccupants calls herdr.PaneList after Pool.Lease and REFUSES
	// on error, and herdr.RequireWorkspace and liveReviewerFor's
	// herdr.AgentList are reached too. So the boundary must be a real owned
	// CLI, not an absent one.
	//
	// installProtocolFakeHerdr is this package's existing protocol-faithful
	// fixture (FAC-145). It writes an owned executable, points HERD_HERDR_BIN
	// at it — which REPLACES any inherited override, the thing NoLiveEnv alone
	// does not do, because binaryPath honours that variable BEFORE the no-live
	// guard — and also sets NoLiveEnv so a lost override refuses instead of
	// reaching the operator's CLI. With no pane state seeded it answers
	// `pane list` and `agent list` with correctly shaped EMPTY inventories and
	// `workspace list` with its own wFAKE, so no developer binary, fleet, pane
	// or workspace is reachable.
	_, herdrCalls = installProtocolFakeHerdr(t)
	// Every test built on this fixture inherits the guard: reaching Herdr for
	// anything the fixture does not model fails the test rather than escaping.
	t.Cleanup(func() { assertOnlyCensusCommands(t, herdrCalls()) })
	home := filepath.Join(root, "fixture-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(root, ".herd", "state", "admission.lease"))
	// The protocol fake reports exactly this workspace id (fakeherdr_test.go),
	// so RequireWorkspace resolves against the fixture rather than any host.
	t.Setenv("HERD_WORKSPACE", "wFAKE")
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	// Keep the real capacity gate in play but guarantee it admits on any host.
	t.Setenv("HERD_MEM_FLOOR_MIB", "1")
	t.Setenv("HERD_REVIEWER_RSS_MIB", "1")
	return root, sha, herdrCalls
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
	root, sha, herdrCalls := entryFixture(t)
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

	// The census this fixture isolates must actually have run, or the
	// owned-boundary claim above is vacuous. Only pane list is required: it is
	// the census that refuses on error, so the entry could not have reached its
	// return without it.
	assertPaneCensusRan(t, herdrCalls())
}

// RETRY must not accumulate a carrier either.
func TestPoolNoLaunchEntryRetryLeavesNoCarrier(t *testing.T) {
	root, sha, _ := entryFixture(t)
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
	root, sha, _ := entryFixture(t)
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

// censusCommands are the EXACT argument vectors the no-launch entry is known
// to make: agent list through liveReviewerFor, pane list through
// evictPoolSlotOccupants, and workspace list through RequireWorkspace.
var censusCommands = map[string]bool{
	"pane list":      true,
	"agent list":     true,
	"workspace list": true,
}

// assertOnlyCensusCommands fails when the entry reached Herdr for anything
// other than those exact vectors.
//
// The comparison is on the WHOLE logged line. The previous version split the
// line with strings.Fields and compared only the first two tokens, which
// accepted any suffix: `pane list --unexpected` matched the allowlist AND the
// shared fake's own two-token dispatch, so it received a valid inventory and
// passed. An exact compare rejects it. It also rejects an EMPTY extra
// argument, which the fake's "$*" log represents as a trailing space and a
// Fields() split would silently erase.
//
// Deliberately NOT asserted here: order, counts, or absence of duplicates.
// Those are not a production contract. A failure path legitimately stops
// before later reads, a retry legitimately repeats them, and harmless
// reordering of independent reads would turn this into a test that mirrors
// the implementation rather than its behaviour.
func assertOnlyCensusCommands(t *testing.T, calls []string) {
	t.Helper()
	for _, call := range calls {
		if !censusCommands[call] {
			t.Errorf("the no-launch entry invoked an unmodelled herdr command: %q", call)
		}
	}
}

// assertPaneCensusRan proves the pane census actually happened.
//
// This one presence assertion is necessary rather than optional: the positive
// oracle's claim is that the entry ran its census against an OWNED boundary
// and still left no carrier. If the census never ran, the test would pass
// while proving nothing about that boundary, and the isolation this fixture
// exists for would be untested. It is asserted only on the successful entry,
// and only for `pane list` — the one census that refuses on error and so must
// have succeeded for the entry to have reached its return at all. agent list
// and workspace list tolerate failure by design, so their absence would not
// make any claim here vacuous and requiring them would over-constrain.
func assertPaneCensusRan(t *testing.T, calls []string) {
	t.Helper()
	for _, call := range calls {
		if call == "pane list" {
			return
		}
	}
	t.Fatalf("the entry never ran the pane census, so its owned-boundary and no-carrier claims are both vacuous; calls: %v", calls)
}
