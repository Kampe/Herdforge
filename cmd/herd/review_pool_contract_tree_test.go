package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// FAC-668 correction findings 1 and 2: runPoolReview used to lease a pool
// slot, evict stale occupants, reset the slot --hard and create the surface
// symlink BEFORE validating the candidate's repository-owned review contract,
// and that validation could not prove candidate provenance anyway (os.Stat
// accepts untracked leftovers `git reset --hard` never removes, and follows
// symlinks to content the reviewed commit does not own).
//
// These tests drive the REAL public entry (the compiled `herd review --pool`
// CLI) against a protocol-faithful fake herdr and a hermetic board, so the
// failure paths are proven by OBSERVABLE STATE — no pool root, no surface
// symlink, no packet, and a fake call log with zero mutation verbs — never by
// source-order scans, and never by touching live pool or provider state.

// poolContractStatusStub answers the raw-PATH `herdr status server` probe the
// capacity census makes (herdrServerRunning execs "herdr" from PATH directly,
// not the BinaryEnv override) and delegates every other call to the protocol
// fake.
const poolContractStatusStub = `#!/bin/sh
if [ "$1" = "status" ] && [ "$2" = "server" ]; then
  echo "status: running"
  exit 0
fi
exec "$HERD_HERDR_BIN" "$@"
`

// poolContractFixture builds the hermetic review --pool fixture: a temp git
// repo (the CLI's cwd, so HERD_ROOT resolves to it) with an ignored .herd/,
// a kaneo board fake, candidate commit variants for every contract
// provenance case, and an origin for the pool's slot worktrees.
func poolContractFixture(t *testing.T, binary string) (dir, keyDir string, shas map[string]string, calls func() []string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	// .herd/, the provisioned fence seal and the claim volume are ignored in
	// the fixture exactly as fleet checkouts ignore their local state, so
	// prepared candidate surfaces, pool slots and fixture plumbing never read
	// as shared-checkout dirt.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".herd/\n.fence-seal\nclaims/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".gitignore")
	gitIn(t, dir, "commit", "-q", "-m", "chore: fixture gitignore")

	// Board + winddown + config: the identity path the public entry walks
	// before anything pool-related. All hermetic.
	fk, server := newFakeKaneo()
	t.Cleanup(server.Close)
	fk.mu.Lock()
	fk.status = "in-progress"
	fk.mu.Unlock()
	writeReviewConfig(t, dir, server.URL, "proj-x")
	seedDisabledWinddown(t, dir)

	// The pool's Ensure creates slot worktrees from origin/main.
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, dir, "clone", "-q", "--bare", ".", bare)
	gitIn(t, dir, "remote", "add", "origin", bare)
	gitIn(t, dir, "fetch", "-q", "origin")

	// The board provider refuses to load without a sealed claim volume; seal
	// one inside the fixture (never a host path).
	keyDir = t.TempDir()
	attestKeyDir(t, keyDir)
	provisionFence(t, binary, dir, keyDir)

	// Every public pool candidate carries the same authenticated launch context
	// as a real task worktree. The context is tracked in this fixture so a
	// detached candidate surface receives it without ambient fallback; later
	// contract variants inherit it and therefore reach the contract gate before
	// any base-resolution refusal.
	baseSHA := gitIn(t, dir, "rev-parse", "HEAD")
	signer := fixtureSigner(t, keyDir, dir)
	contextReceipt, err := signer.Issue(dispatch.TaskContext{
		ProviderType:    "kaneo",
		ProjectID:       "proj-x",
		Repository:      dispatch.RepositoryIdentityOrName(dir, "herdforge-test"),
		Role:            dispatch.RoleWorker,
		TaskRef:         "FAC-1",
		TaskID:          "fixture-task",
		Branch:          "herd/fac-1",
		BaseSHA:         baseSHA,
		LeaseID:         "fixture-review-lease",
		LeaseGeneration: 1,
		LeaseTaskRef:    "FAC-1",
		SessionID:       "fixture-review-session",
		AllowedOps:      dispatch.WorkerOps,
		ExpiresAt:       time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(dir, contextReceipt); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "TASK-CONTEXT.json")
	gitIn(t, dir, "commit", "-q", "-m", "chore: authenticate pool fixture context")

	shas = map[string]string{}
	commit := func(name string) string {
		gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "candidate: "+name)
		return gitIn(t, dir, "rev-parse", "HEAD")
	}

	// valid: the candidate commit tracks BOTH contract paths as regular blobs.
	prompts := filepath.Join(dir, ".herd", "prompts")
	if err := os.MkdirAll(prompts, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{reviewerContractPath, verdictTemplatePath} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("candidate contract\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", "-f", rel)
	}
	shas["valid"] = commit("valid contract")

	// missing: the candidate tree does NOT track the paths — the removal is a
	// real tree change, since an empty commit would inherit the parent's tree.
	gitIn(t, dir, "rm", "-q", "--cached", reviewerContractPath, verdictTemplatePath)
	shas["missing"] = commit("missing contract")

	// untracked: same tree as missing, but the candidate SURFACE worktree a
	// prior occupant held has the paths as untracked leftovers — the content
	// a Stat-based gate wrongly accepts. Pre-create the surface so resolution
	// reuses it instead of preparing a fresh one.
	shas["untracked"] = commit("untracked contract")
	untrackedSurface := filepath.Join(dir, ".herd", "worktrees", "fac-1")
	gitIn(t, dir, "worktree", "add", "-q", "--detach", untrackedSurface, shas["untracked"])
	for _, rel := range []string{reviewerContractPath, verdictTemplatePath} {
		full := filepath.Join(untrackedSurface, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("stale occupant contract\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// symlink: the candidate tracks the reviewer contract as a mode-120000
	// symlink to a real regular file OUTSIDE the tree, so a Stat-following
	// gate would see a regular file and pass; the template is absent.
	if err := os.MkdirAll(prompts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(prompts, "reviewer.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(prompts, "reviewer.md")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-f", reviewerContractPath)
	shas["symlink"] = commit("symlinked contract")

	// PATH stub so the subprocess capacity census sees herdr "running" without
	// ever reaching the operator's fleet, plus the protocol fake.
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "herdr")
	if err := os.WriteFile(stub, []byte(poolContractStatusStub), 0o755); err != nil {
		t.Fatal(err)
	}
	_, calls = installProtocolFakeHerdr(t)
	fixtureStubDir = stubDir
	return dir, keyDir, shas, calls
}

// fixtureStubDir is set by poolContractFixture for poolReviewCmd.
var fixtureStubDir string

// poolReviewCmd runs the pool path against a KNOWN healthy host. These fixtures
// assert contract ownership and pool mutation, not resource admission, and the
// runner's own load is not their subject.
func poolReviewCmd(t *testing.T, binary, dir, keyDir string, args ...string) ([]byte, error) {
	t.Helper()
	return poolReviewCmdOnHost(t, "healthy", binary, dir, keyDir, args...)
}

// poolReviewCmdOnHost runs the pool path against the named fixture host. The
// readings still go through the real admission policy in the herdfixture build,
// so "cpu-saturated" and "memory-pressure" produce production refusals.
func poolReviewCmdOnHost(t *testing.T, host, binary, dir, keyDir string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = append(reviewTestEnv(),
		dispatch.KeyDirEnv+"="+keyDir,
		// Fence the provider at a claim volume inside the fixture, matching
		// herdCmd semantics; the seal was minted by provisionFence above.
		"HERD_CLAIM_DIR="+filepath.Join(dir, "claims"),
		"HERD_FENCE_ATOMIC_SERVER=1",
		"HERD_ADMISSION_LEASE_PATH="+filepath.Join(t.TempDir(), "admission.lease"),
		// Keep the census admissible on any CI host without touching the
		// operator's fleet: the reviewer budget is a fixture fact here.
		"HERD_REVIEWER_RSS_MIB=64",
		"HERD_MEM_FLOOR_MIB=64",
		// The host readings the capacity census decides against. Honoured only
		// by the herdfixture build; see capacity_shared_admission_fixture.go.
		"HERD_FIXTURE_ADMISSION="+host,
	)
	if seal := readMintedSeal(dir); seal != "" {
		cmd.Env = append(cmd.Env, "HERD_FENCE_VOLUME_ID="+seal)
	}
	cmd.Env = append(cmd.Env, hermeticHerdrEnv(os.Getenv(herdr.BinaryEnv), os.Getenv("HERD_FAKE_LOG"))...)
	prependToPath(cmd, fixtureStubDir)
	return cmd.CombinedOutput()
}

// assertZeroPoolMutation is the behavioral proof of finding 1: a refusal that
// happens before the pool must leave NO trace — no pool root (no Ensure, no
// Lease, no Reset), no surface root (no Symlink), no packet, and a fake
// herdr call log with zero lease/eviction/tab/spawn verbs.
func assertZeroPoolMutation(t *testing.T, out []byte, poolRoot, surfaceRoot, packetRoot string, calls func() []string) {
	t.Helper()
	for _, root := range []string{poolRoot, surfaceRoot, packetRoot} {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("refusal mutated %s; candidate contract must be validated BEFORE any pool state change. Output:\n%s", root, out)
		}
	}
	for _, c := range calls() {
		for _, verb := range []string{"tab create", "tab close", "pane list", "pane process-info", "agent start", "agent prompt"} {
			if strings.Contains(c, verb) {
				t.Errorf("refusal touched the fleet: fake herdr saw %q. Output:\n%s", c, out)
			}
		}
	}
}

// A candidate whose tree does not own the reviewer contract must be refused
// BEFORE the operator-asserted builder provenance write too: --builder-family
// records an operator-attributed launch row through the review ledger, and a
// row left behind by a REFUSED candidate makes the failure look
// provenance-admitted on a retry. Same public entry, ledger absence as the
// observable.
func TestPoolReviewRefusesMalformedContractBeforeAssertedProvenance(t *testing.T) {
	binary := buildHerdFixtureAdmission(t)
	dir, keyDir, shas, calls := poolContractFixture(t, binary)
	ledgerPath := reviewledger.DefaultPath(dir)

	for name, sha := range map[string]string{"missing": shas["missing"], "symlink": shas["symlink"]} {
		t.Run(name, func(t *testing.T) {
			poolRoot := filepath.Join(t.TempDir(), "pool")
			surfaceRoot := filepath.Join(t.TempDir(), "review-surfaces")
			packetRoot := filepath.Join(t.TempDir(), "review-packets")

			out, err := poolReviewCmd(t, binary, dir, keyDir, "review", "FAC-1", "--pool", "--no-launch",
				"--builder-family", "anthropic",
				"--sha", sha, "--pool-root", poolRoot, "--surface-root", surfaceRoot, "--packet-root", packetRoot)
			if err == nil {
				t.Fatalf("a candidate whose tree does not own the reviewer contract must refuse even with asserted provenance; output:\n%s", out)
			}
			if _, statErr := os.Stat(ledgerPath); !os.IsNotExist(statErr) {
				t.Errorf("a refused candidate must leave NO operator-asserted provenance row behind (%s must not exist); output:\n%s", ledgerPath, out)
			}
			assertZeroPoolMutation(t, out, poolRoot, surfaceRoot, packetRoot, calls)
		})
	}
}

func TestPoolReviewRefusesUnownedContractBeforePoolMutation(t *testing.T) {
	binary := buildHerdFixtureAdmission(t)
	dir, keyDir, shas, calls := poolContractFixture(t, binary)

	for name, sha := range shas {
		if name == "valid" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			poolRoot := filepath.Join(t.TempDir(), "pool")
			surfaceRoot := filepath.Join(t.TempDir(), "review-surfaces")
			packetRoot := filepath.Join(t.TempDir(), "review-packets")

			out, err := poolReviewCmd(t, binary, dir, keyDir, "review", "FAC-1", "--pool", "--no-launch",
				"--sha", sha, "--pool-root", poolRoot, "--surface-root", surfaceRoot, "--packet-root", packetRoot)
			if err == nil {
				t.Fatalf("candidate whose git tree does not own the reviewer contract must refuse; output:\n%s", out)
			}
			for _, want := range []string{reviewerContractPath, sha[:12]} {
				if !strings.Contains(string(out), want) {
					t.Errorf("refusal must name %q; a bare exit hides which contract path was unowned. Output:\n%s", want, out)
				}
			}
			assertZeroPoolMutation(t, out, poolRoot, surfaceRoot, packetRoot, calls)
		})
	}
}

// The positive control: a candidate whose tree owns both contract paths as
// regular blobs passes the pre-pool gate, prepares the surface, and — under
// --no-launch — keeps BOTH the surface and the lease (FAC-626).
func TestPoolReviewValidCandidatePreparesSurfaceAndHoldsLease(t *testing.T) {
	binary := buildHerdFixtureAdmission(t)
	dir, keyDir, shas, calls := poolContractFixture(t, binary)
	sha := shas["valid"]
	poolRoot := filepath.Join(t.TempDir(), "pool")
	surfaceRoot := filepath.Join(t.TempDir(), "review-surfaces")
	packetRoot := filepath.Join(t.TempDir(), "review-packets")

	out, err := poolReviewCmd(t, binary, dir, keyDir, "review", "FAC-1", "--pool", "--no-launch",
		"--sha", sha, "--pool-root", poolRoot, "--surface-root", surfaceRoot, "--packet-root", packetRoot)
	if err != nil {
		t.Fatalf("a candidate owning its contract as tracked regular blobs must prepare a surface; output:\n%s", out)
	}
	if !strings.Contains(string(out), "review surface ready") {
		t.Fatalf("expected the --no-launch ready report; output:\n%s", out)
	}
	// The census the pool gate decided against must be the FIXTURE's, not the
	// runner's. The gate prints mem_available from the same observation the
	// post-admission arms read, so this one assertion proves the seam reached
	// every one of them rather than only the admission.
	if !strings.Contains(string(out), "mem_available=49152MiB") {
		t.Fatalf("the capacity gate did not decide against the pinned fixture census; output:\n%s", out)
	}

	// The surface exists, is a symlink, and resolves to a pool slot pinned at
	// the exact candidate.
	surface := filepath.Join(surfaceRoot, "review-fac-1-"+sha[:12])
	target, err := os.Readlink(surface)
	if err != nil {
		t.Fatalf("prepared surface missing: %v\noutput:\n%s", err, out)
	}
	slotPath := target
	if !filepath.IsAbs(slotPath) {
		slotPath = filepath.Join(surfaceRoot, target)
	}
	if head := strings.TrimSpace(runGitOut(t, slotPath, "rev-parse", "HEAD")); head != sha {
		t.Fatalf("pooled slot is at %s, want exact candidate %s", head, sha)
	}

	// The lease is HELD for the supervisor (FAC-626), not released.
	slots, err := worktree.NewPool(dir, poolRoot, 2).Slots()
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, s := range slots {
		if s.LeaseID != "" {
			held++
			if !strings.HasPrefix(s.Purpose, "review-fac-1-") {
				t.Errorf("lease purpose %q must name the candidate review", s.Purpose)
			}
		}
	}
	if held != 1 {
		t.Fatalf("exactly one lease must be held after --no-launch, saw %d (slots: %+v)", held, slots)
	}

	// The packet was written for the pinned candidate.
	if _, err := os.Stat(filepath.Join(packetRoot, "review-fac-1-"+sha[:12]+".md")); err != nil {
		t.Fatalf("review packet missing: %v", err)
	}

	for _, c := range calls() {
		if strings.Contains(c, "tab create") {
			t.Errorf("--no-launch must never create a reviewer tab; fake herdr saw %q", c)
		}
	}
}

// A failure AFTER the surface exists is the launch's own to clean up: the
// provisional surface must be removed and the lease released, leaving no
// ownerless surface behind (finding 1, explicit provisional cleanup).
func TestPoolReviewPostPreparationFailureCleansProvisionalSurface(t *testing.T) {
	binary := buildHerdFixtureAdmission(t)
	dir, keyDir, shas, calls := poolContractFixture(t, binary)
	sha := shas["valid"]
	poolRoot := filepath.Join(t.TempDir(), "pool")
	surfaceRoot := filepath.Join(t.TempDir(), "review-surfaces")

	// Force a POST-preparation failure: the packet phase runs only after the
	// pool lease, reset, surface symlink and both surface verifications have
	// succeeded. A FILE at the packet root makes that phase fail.
	packetBlocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(packetBlocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	packetRoot := filepath.Join(packetBlocker, "packets")

	out, err := poolReviewCmd(t, binary, dir, keyDir, "review", "FAC-1", "--pool", "--no-launch",
		"--sha", sha, "--pool-root", poolRoot, "--surface-root", surfaceRoot, "--packet-root", packetRoot)
	if err == nil {
		t.Fatalf("a blocked packet root must fail the launch; output:\n%s", out)
	}

	// The failure happened after the surface was created, so the failure OWNS
	// the provisional surface: it must be gone, and the lease released.
	surface := filepath.Join(surfaceRoot, "review-fac-1-"+sha[:12])
	if _, statErr := os.Lstat(surface); !os.IsNotExist(statErr) {
		t.Errorf("a post-preparation failure must remove the provisional surface symlink it created; output:\n%s", out)
	}
	slots, err := worktree.NewPool(dir, poolRoot, 2).Slots()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range slots {
		if s.LeaseID != "" {
			t.Errorf("a post-preparation failure must release its lease; slot %s still holds %q", s.Name, s.LeaseID)
		}
	}
	for _, c := range calls() {
		if strings.Contains(c, "tab create") {
			t.Errorf("a post-preparation failure must not reach the tab phase; fake herdr saw %q", c)
		}
	}
}

// --- In-process units of the tree-provenance validator itself (race-covered).

func TestVerifyCandidateTreeContract(t *testing.T) {
	binary := buildHerd(t)
	dir, _, shas, _ := poolContractFixture(t, binary)

	t.Run("valid regular blobs pass", func(t *testing.T) {
		if err := verifyCandidateTreeContract(dir, shas["valid"]); err != nil {
			t.Fatalf("a tree owning both contract paths as regular blobs must pass: %v", err)
		}
	})

	t.Run("missing path refuses", func(t *testing.T) {
		err := verifyCandidateTreeContract(dir, shas["missing"])
		if err == nil || !strings.Contains(err.Error(), reviewerContractPath) {
			t.Fatalf("a tree without the contract must refuse naming %q, got %v", reviewerContractPath, err)
		}
	})

	t.Run("untracked worktree leftovers refuse", func(t *testing.T) {
		// The untracked fixture surface HOLDS both files on disk right now —
		// exactly what a Stat-based gate accepts and what reset --hard keeps.
		for _, rel := range []string{reviewerContractPath, verdictTemplatePath} {
			if _, err := os.Stat(filepath.Join(dir, ".herd", "worktrees", "fac-1", rel)); err != nil {
				t.Fatalf("fixture precondition: untracked leftover should exist: %v", err)
			}
		}
		if err := verifyCandidateTreeContract(dir, shas["untracked"]); err == nil {
			t.Fatal("untracked contract leftovers must refuse: the reviewed commit does not own them")
		}
	})

	t.Run("symlinked contract refuses and stat would be fooled", func(t *testing.T) {
		symlinkSHA := shas["symlink"]
		wt := filepath.Join(dir, ".herd", "worktrees", "symlink-variant")
		gitIn(t, dir, "worktree", "add", "-q", "--detach", wt, symlinkSHA)
		linkPath := filepath.Join(wt, reviewerContractPath)
		info, err := os.Stat(linkPath)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("fixture precondition: os.Stat must see a regular file through the contract symlink (that is the finding), got %v %v", info, err)
		}
		err = verifyCandidateTreeContract(dir, symlinkSHA)
		if err == nil {
			t.Fatal("a mode-120000 contract entry must refuse even though Stat resolves it to a regular file")
		}
		if !strings.Contains(err.Error(), "120000") && !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("refusal must name the symlink mode, got %v", err)
		}
	})

	t.Run("non-commit refuses", func(t *testing.T) {
		if err := verifyCandidateTreeContract(dir, "0123456789abcdef0123456789abcdef01234567"); err == nil {
			t.Fatal("a sha that is not a commit here must refuse, not pass vacuously")
		}
	})
}

// TestPoolReviewRefusesUnsafeHostBeforeCandidatePreparation is the counterpart
// to the healthy path above, through the SAME subprocess seam.
//
// It matters that this goes through the subprocess: capacity_pool_gate_test.go
// substitutes poolCapacityObserve in-process and therefore never reaches
// withSharedAdmission at all. These cases do, so the attachment, the real
// resources.Decide and the pool gate's refusal are exercised together.
//
// A refusal must land BEFORE any candidate or pool mutation, and it must name
// the resource that refused, so an operator is not left guessing which ceiling
// stopped the launch.
func TestPoolReviewRefusesUnsafeHostBeforeCandidatePreparation(t *testing.T) {
	binary := buildHerdFixtureAdmission(t)
	dir, keyDir, shas, calls := poolContractFixture(t, binary)
	sha := shas["valid"]

	for _, tc := range []struct {
		host  string
		wants []string
	}{
		{
			host: "cpu-saturated",
			// The production sentence, not a fixture paraphrase.
			wants: []string{"REFUSING before candidate preparation", "CPU is saturated", "normalized load 2.00"},
		},
		{
			host:  "memory-pressure",
			wants: []string{"REFUSING before candidate preparation", "kernel reports memory pressure critical"},
		},
		{
			// An unset or misspelled request must refuse deterministically
			// rather than inherit whatever the runner was doing.
			host:  "not-a-known-host",
			wants: []string{"REFUSING before candidate preparation", "not known"},
		},
	} {
		t.Run(tc.host, func(t *testing.T) {
			poolRoot := filepath.Join(t.TempDir(), "pool")
			surfaceRoot := filepath.Join(t.TempDir(), "review-surfaces")
			packetRoot := filepath.Join(t.TempDir(), "review-packets")

			out, err := poolReviewCmdOnHost(t, tc.host, binary, dir, keyDir, "review", "FAC-1", "--pool", "--no-launch",
				"--sha", sha, "--pool-root", poolRoot, "--surface-root", surfaceRoot, "--packet-root", packetRoot)
			if err == nil {
				t.Fatalf("an unsafe host must refuse the launch; output:\n%s", out)
			}
			for _, want := range tc.wants {
				if !strings.Contains(string(out), want) {
					t.Errorf("refusal must name %q so the operator knows which ceiling stopped it. Output:\n%s", want, out)
				}
			}
			// The candidate here OWNS its contract, so nothing but the resource
			// gate can be refusing: the same sha admits on a healthy host in
			// TestPoolReviewValidCandidatePreparesSurfaceAndHoldsLease.
			assertZeroPoolMutation(t, out, poolRoot, surfaceRoot, packetRoot, calls)
		})
	}
}
