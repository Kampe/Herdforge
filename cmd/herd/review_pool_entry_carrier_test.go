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

	// The capacity census makes ONE MORE external call, and it does not use the
	// override: herdrServerRunning (capacity.go:461) execs "herdr" from PATH
	// directly. CI 34753903312 proved it — the entry refused with
	// `herdr status server: exec: "herdr": executable file not found in $PATH`
	// before candidate preparation, so the earlier fixture never reached the
	// census at all. That PATH resolution is deliberate, and this package's
	// convention for it is poolContractStatusStub: a fixture-owned `herdr` on
	// PATH that answers the health probe and delegates everything else to the
	// protocol fake, so the real probe really runs and really parses.
	//
	// The probe is therefore NOT in the call log (the stub answers it before
	// delegating), and nothing asserts it directly. It needs no assertion: a
	// failing probe refuses BEFORE candidate preparation, so the positive
	// oracle reaching its assertions at all is proof the probe was served here.
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "herdr"), []byte(poolContractStatusStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

	// Every boundary this fixture serves must actually have been reached, or
	// the protocols it answers are unproven: the two tolerant ones would fall
	// back silently on a wrong envelope. Presence only — no order, no counts.
	assertCompleteCensusObserved(t, herdrCalls())
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

// assertCompleteCensusObserved proves the successful entry actually reached
// EVERY Herdr boundary this fixture serves, each at least once.
//
// Presence is the point, and it is necessary rather than optional hardening:
// agent list and workspace list both TOLERATE failure by design — an
// unavailable roster reads as "not known", and an unresolvable workspace falls
// back to the environment — so if the fixture answered either with a wrong
// envelope the entry would take its tolerant path and this test would still
// pass, silently proving nothing about those two protocols. Requiring the
// observation is what makes the fixture a compatibility oracle instead of a
// one-command one. pane list is the third and the only one that refuses on
// error, so the entry could not have returned without it.
//
// Still NOT asserted: order, counts, or absence of duplicates. Only the
// SUCCESS path requires the complete set; the refusal path legitimately stops
// before the census and the retry path legitimately repeats reads, so both of
// those inherit only the exact-vector rejection.
func assertCompleteCensusObserved(t *testing.T, calls []string) {
	t.Helper()
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		seen[call] = true
	}
	for _, want := range []string{"agent list", "pane list", "workspace list"} {
		if !seen[want] {
			t.Fatalf("the successful entry never reached the %q boundary, so this fixture proves nothing about that protocol; calls: %v", want, calls)
		}
	}
}
