package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/freshness"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/resources"
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

	// RUNTIME IGNORES, COMMITTED FIRST. The entry writes its own operating
	// state into .herd/ AFTER the fixture's clean check, and the production
	// dirty guard then refuses — correctly. CI 34755537851 listed exactly
	// .herd/harvest-queue.jsonl, .herd/herd.yaml, .herd/review-ledger.jsonl
	// and .herd/state/.
	//
	// These rules name only artifacts the RUNTIME creates, one per line, so
	// genuine source dirt in the fixture still trips the real guard. Nothing
	// broad is ignored: .herd/prompts/ stays tracked, because the reviewer
	// contract IS source and the entry verifies it in the candidate's tree.
	write(".gitignore", strings.Join([]string{
		"/.herd/review-ledger.jsonl",
		"/.herd/harvest-queue.jsonl",
		"/.herd/pool/",
		"/.herd/review-surfaces/",
		"/.herd/review-packets/",
		"/.herd/worktrees/",
		"/.herd/review/",
		"",
	}, "\n"))

	// CONFIGURATION IS TRACKED, not ignored: this repository tracks
	// .herd/herd.yaml and ignores only .herd/herd.yaml.local, so a fixture
	// that ignored its own config would be unfaithful to the native contract.
	// It is committed before the candidate, like any other checked-in config.
	write(".herd/herd.yaml", "version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n  project_id: "+entryProjectID+"\n")

	// The entry refuses a candidate whose own tree does not track the reviewer
	// contract and verdict template, so the fixture commits both.
	write(reviewerContractPath, "# reviewer contract (fixture)\n")
	write(verdictTemplatePath, "# verdict template (fixture)\n")
	write("README.md", "fixture\n")
	git("add", ".")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	// A REAL origin, because the warm pool creates its slots at origin/main:
	// worktree.NewPool defaults DefaultBase to "origin/main" and runPoolReview
	// does not override it, so a fixture without a remote would fail at
	// Pool.Ensure — the next gate after the dirty check, not a hypothetical.
	// origin/main stays at the BASE, as it would for unmerged candidate work;
	// the slot is reset to the exact candidate afterwards by the entry itself.
	originDir := filepath.Join(filepath.Dir(root), "origin-"+filepath.Base(root)+".git")
	if out, err := exec.Command("git", "init", "--bare", "-q", "-b", "main", originDir).CombinedOutput(); err != nil {
		t.Fatalf("fixture origin: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = os.RemoveAll(originDir) })
	git("remote", "add", "origin", originDir)
	git("push", "-q", "origin", "main")

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
	bindOwnedHostObservation(t)
	_, herdrCalls = installProtocolFakeHerdr(t)
	// Every test built on this fixture inherits the guard: reaching Herdr for
	// anything the fixture does not model fails the test rather than escaping.
	t.Cleanup(func() { assertOnlyCensusCommands(t, herdrCalls()) })

	// The capacity census makes ONE MORE external call, on a DIFFERENT
	// transport: herdrServerRunning (capacity.go:461) execs "herdr" from PATH
	// directly. pkg/herdr does honour HERD_HERDR_BIN — this probe simply does
	// not go through it. CI 34753903312 proved the gap: the entry refused with
	// `herdr status server: exec: "herdr": executable file not found in $PATH`
	// before candidate preparation, so the earlier fixture never reached the
	// census it claimed to isolate.
	//
	// entryHealthStub is poolContractStatusStub's shape — answer the health
	// probe, exec $HERD_HERDR_BIN for everything else — plus one line: it LOGS
	// the probe. The shared stub answers silently, and a boundary that is not
	// observed cannot be asserted. Logging it makes `status server` a
	// first-class member of the same exact-argv allowlist and of the
	// success-path presence set, so this pre-candidate health read is neither
	// concealed nor bypassed, and the real capacity gate still decides.
	//
	// The fake's own default envelope would NOT work here: herdrServerRunning
	// parses plain `key: value` text and wants `status: running`, while the
	// fake's default is JSON {"result":{}} with no status line.
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "herdr"), []byte(entryHealthStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// HOME sits OUTSIDE the repository. A home inside the checkout is not the
	// native shape, and the moment anything wrote into it the directory would
	// become untracked dirt and the production guard would refuse — the same
	// class of failure as .herd/state/, one level up.
	home := filepath.Join(filepath.Dir(root), "home-"+filepath.Base(root))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	// The admission lease lives under HOME in production: admissionLeasePath
	// falls back to $HOME/.herd/state/admission.lease. Pointing it inside the
	// repository is what put .herd/state/ in the dirty list.
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(home, ".herd", "state", "admission.lease"))
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

// entryProjectID is the project the committed fixture config declares and the
// seeded provider answers for.
const entryProjectID = "fixture-project"

// entryConfig installs the seeded provider for the entry's own task lookup.
// The CONFIG itself is committed by entryFixture, because this repository
// tracks .herd/herd.yaml.
func entryConfig(t *testing.T, root string) {
	t.Helper()
	seeded := provider.NewMemoryProvider()
	seeded.AddTask(&provider.Task{
		ID: "fixture-1", Ref: entryRef, Title: "carrier lifecycle fixture",
		Status: provider.StatusInProgress, ProjectID: entryProjectID,
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

	// The fixture's runtime ignores must COVER what the entry actually wrote.
	// `git status --porcelain` omits ignored paths, so a clean result means
	// every artifact the run produced was declared, while genuine source dirt —
	// a tracked file the entry modified, or an undeclared new runtime file —
	// still shows and fails here. That is the distinction the production guard
	// makes, checked against real output rather than restated from the ignore
	// list, so a future runtime artifact fails loudly here instead of silently
	// refusing on the next hosted run.
	if dirt := strings.TrimSpace(entryGitOutput(t, root, "status", "--porcelain")); dirt != "" {
		t.Fatalf("the entry left undeclared dirt in the shared checkout:\n%s", dirt)
	}
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
// to make: status server through herdrServerRunning on the raw-PATH transport,
// then agent list through liveReviewerFor, pane list through
// evictPoolSlotOccupants and workspace list through RequireWorkspace, all
// three through the HERD_HERDR_BIN transport.
var censusCommands = map[string]bool{
	"status server":  true,
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
	for _, want := range []string{"status server", "agent list", "pane list", "workspace list"} {
		if !seen[want] {
			t.Fatalf("the successful entry never reached the %q boundary, so this fixture proves nothing about that protocol; calls: %v", want, calls)
		}
	}
}

// entryHealthStub is poolContractStatusStub plus a log line. The raw-PATH
// health probe is a real boundary of this entry, so this fixture records it
// like every other call instead of answering it invisibly.
const entryHealthStub = `#!/bin/sh
if [ "$1" = "status" ] && [ "$2" = "server" ]; then
  printf '%s\n' "$*" >> "$HERD_FAKE_LOG"
  echo "status: running"
  exit 0
fi
exec "$HERD_HERDR_BIN" "$@"
`

// bindOwnedHostObservation makes the host readings this entry's admission
// judges DETERMINISTIC, without touching the policy that judges them.
//
// CI 34754690586 refused both entry oracles with "resource admission refuses
// this host: ... normalized load 1.43 (load1 5.72 over 4 cpus) is at or above
// the 0.75 limit". The earlier fixture isolated the capacity gate's ENV knobs
// and both Herdr transports, but the shared resource admission is neither: it
// is read from the live runner inside observeCapacity, so a busy GitHub runner
// decided the outcome. It never fired locally because this Mac is not loaded.
//
// poolCapacityObserve is a package var, so this saves and restores it, and it
// DELEGATES to the live observer first — the real herdr status probe, the real
// process census and the real lease all still run. Only the host readings are
// replaced, and they are replaced exactly the way the herdfixture twin does it
// (capacity_shared_admission_fixture.go): the numbers go through the real
// resources.Decide with the real resources.DefaultLimits, so the policy is
// untouched and a saturated reading would still refuse with the production
// text. The values mirror that twin's "healthy" arm and its pinned census,
// which cannot be referenced directly here because that file is behind the
// herdfixture build tag and these tests run untagged.
//
// Herdr liveness, the agent census and reviewer counts are deliberately left
// live, as pinFixtureCensus documents: this fixture's own stubs control those,
// and overriding them would hide a real failure in that plumbing.
func bindOwnedHostObservation(t *testing.T) {
	t.Helper()
	live := poolCapacityObserve
	t.Cleanup(func() { poolCapacityObserve = live })
	poolCapacityObserve = func() CapacityObservation {
		o := live()
		now := time.Now()
		const source = "fac832-entry-fixture"
		decision := resources.Decide(now,
			freshness.Fresh(source, now, resources.CPULoad{Load1: 1, CPUs: 8, Normalized: 0.125}),
			freshness.Fresh(source, now, resources.MemHeadroom{
				Pressure: resources.PressureNormal, FreePct: 80, FreePctGates: true,
			}),
			resources.DefaultLimits())
		o.admission = &decision
		// The arms decideCapacity evaluates AFTER the admission read the
		// runner's census too, so pinning only the admission would leave the
		// oracle half controlled. Same values as the tagged twin's.
		o.PressurePct = 0
		o.SwapTotalMiB = 8192
		o.SwapUsedMiB = 0
		o.MemTotalMiB = 65536
		o.MemAvailMiB = 49152
		return o
	}
}
