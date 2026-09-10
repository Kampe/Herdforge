package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// fakeKnownEmptyInspector is the explicit, known-empty process-ownership
// snapshot fixture tests inject. Production code never constructs this --
// a nil IdlePoolDiscoveryConfig.ProcessInspector leaves each Pool's own
// NewPool default (the real native census) in force.
type fakeKnownEmptyInspector struct{ busy map[string]bool }

func (f fakeKnownEmptyInspector) InUse(_ context.Context, path string) (resources.ProcessUsage, error) {
	if f.busy[path] {
		return resources.ProcessUsage{CWD: true}, nil
	}
	return resources.ProcessUsage{}, nil
}

// makeIdlePoolRoot creates a real, registered 2-slot pool at
// <repoRoot>/.herd/<name>, clean and reachable from base, via the real
// Pool.Ensure path -- never a hand-built fixture. Its ProcessInspector is a
// known-empty fake so tests do not depend on real lsof/process state.
func makeIdlePoolRoot(t *testing.T, repoRoot, name string, base string) *Pool {
	t.Helper()
	root := filepath.Join(repoRoot, ".herd", name)
	pool := NewPool(repoRoot, root, 1)
	pool.DefaultBase = base
	pool.ProcessInspector = fakeKnownEmptyInspector{}
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure %s: %v", name, err)
	}
	// A fixture pool root carries positive retirement evidence by default:
	// under the native contract a pool absent from the manifest registry is
	// not automatically disposable, so every fixture pool records one
	// completed generation. Guards under test (dirty/leased/unmerged) keep
	// their own refusals on top of this.
	makeRetirementEvidence(t, repoRoot, name)
	return pool
}

// makeRetirementEvidence appends the positive retirement evidence the
// native contract requires before a pool root is disposable: a manifest
// registry row naming the pool and a phase journal record confirming that
// exact generation complete. Without it a pool root retains, whatever its
// cleanliness. Appending keeps evidence for every fixture pool cumulative.
func makeRetirementEvidence(t *testing.T, repoRoot, poolName string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(repoRoot, "retirement-manifests.jsonl")
	journalPath := filepath.Join(repoRoot, "retirement-phases.jsonl")
	row := fmt.Sprintf(`{"pool":".herd/%s","generation":"g1","candidate_sha":"c1","reviewer":"r1","binding_digest":"b1"}`+"\n", poolName)
	if err := appendEvidenceLine(manifestPath, row); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"pool":".herd/%s","generation":"g1","candidate_sha":"c1","reviewer":"r1","binding_digest":"b1","phase":"complete"}`+"\n", poolName)
	if err := appendEvidenceLine(journalPath, rec); err != nil {
		t.Fatal(err)
	}
	return manifestPath, journalPath
}

func appendEvidenceLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

func testCfg(root string) IdlePoolDiscoveryConfig {
	return IdlePoolDiscoveryConfig{
		RepoRoot:         root,
		DefaultBase:      "main",
		ProcessInspector: fakeKnownEmptyInspector{},
		ManifestPath:     filepath.Join(root, "retirement-manifests.jsonl"),
		PhaseJournalPath: filepath.Join(root, "retirement-phases.jsonl"),
	}
}

func TestDiscoverIdlePools_FindsCleanNonCurrentPoolsEligible(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-a", "main")
	makeIdlePoolRoot(t, root, "pool-fac-b", "main")
	// The bare ".herd/pool" is the live, in-rotation pool -- never a
	// discovery candidate even when idle and clean.
	current := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	current.DefaultBase = "main"
	current.ProcessInspector = fakeKnownEmptyInspector{}
	if err := current.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	result, err := DiscoverIdlePools(context.Background(), testCfg(root))
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Eligible != 2 || result.Retained != 0 || result.Failed != 0 {
		t.Fatalf("result=%+v", result)
	}
	for _, d := range result.Dispositions {
		if d.Root == "pool" {
			t.Fatalf("the current pool must never appear as a candidate: %+v", result.Dispositions)
		}
	}
}

func TestDiscoverIdlePools_DryRunWritesNoFiles(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-a", "main")

	before, err := os.ReadDir(filepath.Join(root, ".herd"))
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := map[string]bool{}
	for _, e := range before {
		beforeNames[e.Name()] = true
	}

	result, err := DiscoverIdlePools(context.Background(), testCfg(root))
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Eligible != 1 {
		t.Fatalf("result=%+v", result)
	}

	after, err := os.ReadDir(filepath.Join(root, ".herd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after {
		if beforeNames[e.Name()] {
			continue
		}
		// The tick lock file is the mutex artifact (kernel flock carrier),
		// not discovery state: it may appear during a dry run exactly as
		// during an acting pass, and it is never unlinked afterwards. Every
		// other new entry is a purity violation.
		if e.Name() == filepath.Base(idlePoolLockPath(root)) {
			continue
		}
		t.Fatalf("dry run created a new .herd entry it must not have: %s", e.Name())
	}
	if _, err := os.Stat(idlePoolCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("dry run must never write the cursor file, stat err=%v", err)
	}
	slots, err := NewPool(root, filepath.Join(root, ".herd", "pool-fac-a"), 0).Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("dry run mutated the pool: slots=%d err=%v", len(slots), err)
	}
}

func TestReclaimIdlePools_WritesCursorOnlyOnAct(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-a", "main")

	if _, err := ReclaimIdlePools(context.Background(), testCfg(root)); err != nil {
		t.Fatalf("ReclaimIdlePools: %v", err)
	}
	if _, err := os.Stat(idlePoolCursorPath(root)); err != nil {
		t.Fatalf("acting reclaim must persist the cursor: %v", err)
	}
}

func TestDiscoverIdlePools_RetainsLeasedDirtyAndUnmergedSlots(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)

	leased := makeIdlePoolRoot(t, root, "pool-fac-leased", "main")
	if _, err := leased.Lease(context.Background(), "reviewer"); err != nil {
		t.Fatal(err)
	}

	dirty := makeIdlePoolRoot(t, root, "pool-fac-dirty", "main")
	if err := os.WriteFile(filepath.Join(dirty.Root, "pool-01", "untracked.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	unmerged := makeIdlePoolRoot(t, root, "pool-fac-unmerged", "main")
	slotPath := filepath.Join(unmerged.Root, "pool-01")
	if err := os.WriteFile(filepath.Join(slotPath, "unique.txt"), []byte("only here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(slotPath, "git", "add", "unique.txt"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(slotPath, "git", "commit", "-m", "unmerged work"); err != nil {
		t.Fatal(err)
	}

	result, err := DiscoverIdlePools(context.Background(), testCfg(root))
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Eligible != 0 || result.Retained != 3 {
		t.Fatalf("result=%+v", result)
	}
	for _, name := range []string{"pool-fac-leased", "pool-fac-dirty", "pool-fac-unmerged"} {
		slots, err := NewPool(root, filepath.Join(root, ".herd", name), 0).Slots()
		if err != nil || len(slots) != 1 {
			t.Fatalf("retained pool %s must survive untouched: slots=%d err=%v", name, len(slots), err)
		}
	}
}

func TestDiscoverIdlePools_UnknownProcessOwnershipRetains(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-owned", "main")

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg(root)
	cfg.ProcessInspector = fakeKnownEmptyInspector{busy: map[string]bool{
		filepath.Join(realRoot, ".herd", "pool-fac-owned", "pool-01"): true,
	}}
	result, err := DiscoverIdlePools(context.Background(), cfg)
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Eligible != 0 || result.Retained != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func writeJSONL(t *testing.T, path string, rows ...map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverIdlePools_ActiveUncompletedManifestRetainsHeldTerminalReclaims(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-active", "main")
	makeIdlePoolRoot(t, root, "pool-fac-terminal", "main")

	manifestPath := filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")
	phasePath := filepath.Join(root, ".herd", "review", "retirement-phases.jsonl")
	writeJSONL(t, manifestPath,
		map[string]string{"pool": ".herd/pool-fac-active", "generation": "g-active", "candidate_sha": "a", "reviewer": "r", "binding_digest": "d"},
		map[string]string{"pool": ".herd/pool-fac-terminal", "generation": "g-done", "candidate_sha": "a", "reviewer": "r", "binding_digest": "d"},
	)
	// Only the terminal pool's exact generation has a matching "complete"
	// phase record -- the active pool's generation never completed.
	writeJSONL(t, phasePath,
		map[string]string{"pool": ".herd/pool-fac-terminal", "generation": "g-done", "candidate_sha": "a", "reviewer": "r", "binding_digest": "d", "phase": "worktree-intent"},
		map[string]string{"pool": ".herd/pool-fac-terminal", "generation": "g-done", "candidate_sha": "a", "reviewer": "r", "binding_digest": "d", "phase": "complete"},
	)

	cfg := testCfg(root)
	cfg.ManifestPath, cfg.PhaseJournalPath = manifestPath, phasePath
	result, err := DiscoverIdlePools(context.Background(), cfg)
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	byRoot := map[string]IdlePoolStatus{}
	for _, d := range result.Dispositions {
		byRoot[d.Root] = d.Status
	}
	if byRoot["pool-fac-active"] != IdlePoolRetained {
		t.Fatalf("active/uncompleted manifest pool must be retained: %+v", result)
	}
	if byRoot["pool-fac-terminal"] != IdlePoolEligible {
		t.Fatalf("fully-completed terminal manifest pool must be eligible, not held forever: %+v", result)
	}
}

func TestDiscoverIdlePools_UnparseablePhaseJournalFailsClosed(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-x", "main")

	manifestPath := filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")
	phasePath := filepath.Join(root, ".herd", "review", "retirement-phases.jsonl")
	writeJSONL(t, manifestPath, map[string]string{"pool": ".herd/pool-fac-x", "generation": "g1", "candidate_sha": "a", "reviewer": "r", "binding_digest": "d"})
	if err := os.MkdirAll(filepath.Dir(phasePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(phasePath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := testCfg(root)
	cfg.ManifestPath, cfg.PhaseJournalPath = manifestPath, phasePath
	if _, err := DiscoverIdlePools(context.Background(), cfg); err == nil {
		t.Fatal("expected the tick to fail closed on an unparseable phase journal")
	}
	slots, err := NewPool(root, filepath.Join(root, ".herd", "pool-fac-x"), 0).Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("fail-closed tick must not have mutated anything: slots=%d err=%v", len(slots), err)
	}
}

func TestDiscoverIdlePools_ReportsSchemaFailureForCorruptPoolJSON(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	badRoot := filepath.Join(root, ".herd", "pool-fac-corrupt")
	if err := os.MkdirAll(badRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, "pool.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := DiscoverIdlePools(context.Background(), testCfg(root))
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Failed != 1 || result.Eligible != 0 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(badRoot); err != nil {
		t.Fatalf("corrupt schema root must survive untouched: %v", err)
	}
}

func TestReclaimIdlePools_DeletesOnlyEligibleAndLeavesOthersIntact(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	clean := makeIdlePoolRoot(t, root, "pool-fac-clean", "main")
	dirty := makeIdlePoolRoot(t, root, "pool-fac-dirty2", "main")
	if err := os.WriteFile(filepath.Join(dirty.Root, "pool-01", "mine.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ReclaimIdlePools(context.Background(), testCfg(root))
	if err != nil {
		t.Fatalf("ReclaimIdlePools: %v", err)
	}
	if result.Eligible != 1 || result.Retained != 1 {
		t.Fatalf("result=%+v", result)
	}

	cleanSlots, err := clean.Slots()
	if err != nil || len(cleanSlots) != 0 {
		t.Fatalf("clean pool should be fully reclaimed, slots=%d err=%v", len(cleanSlots), err)
	}
	dirtySlots, err := dirty.Slots()
	if err != nil || len(dirtySlots) != 1 {
		t.Fatalf("dirty pool must survive with its slot intact, slots=%d err=%v", len(dirtySlots), err)
	}
	if _, err := os.Stat(filepath.Join(dirty.Root, "pool-01")); err != nil {
		t.Fatalf("dirty slot directory must still exist: %v", err)
	}

	// A reclaimed pool root must still allow a fresh Ensure to rebuild it.
	if err := clean.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after reclaim: %v", err)
	}
	rebuilt, err := clean.Slots()
	if err != nil || len(rebuilt) != 1 {
		t.Fatalf("Ensure after reclaim should recreate 1 slot, got %d err=%v", len(rebuilt), err)
	}
}

func TestIdlePoolDiscovery_BoundedRootsAppliesDuringInspectionNotAfter(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	// One valid pool plus several corrupt-schema roots -- if bounding only
	// applied after a full parse pass, all of these would already have been
	// opened and parsed regardless of MaxRoots. Instead only MaxRoots
	// candidates in fair order are inspected at all.
	makeIdlePoolRoot(t, root, "pool-fac-1", "main")
	for _, name := range []string{"pool-fac-2", "pool-fac-3", "pool-fac-4"} {
		bad := filepath.Join(root, ".herd", name)
		if err := os.MkdirAll(bad, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bad, "pool.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result, err := DiscoverIdlePools(context.Background(), func() IdlePoolDiscoveryConfig {
		cfg := testCfg(root)
		cfg.MaxRoots = 2
		return cfg
	}())
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Scanned != 2 {
		t.Fatalf("scanned=%d, want bounded to 2 even though 4 candidates exist", result.Scanned)
	}
	if result.NextCursor == "" {
		t.Fatal("expected a reported next cursor after a bounded tick")
	}
}

func TestIdlePoolDiscovery_BoundedElapsedStopsEarlyDuringSlowInspection(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	for _, name := range []string{"pool-fac-1", "pool-fac-2", "pool-fac-3"} {
		makeIdlePoolRoot(t, root, name, "main")
	}

	calls := 0
	now := time.Unix(1000, 0)
	clock := func() time.Time {
		calls++
		// Call 1 captures the tick's start time. Call 2 is the in-loop
		// budget check for the first candidate, which must still pass so
		// exactly one root is fully inspected. From call 3 on, the clock
		// jumps far ahead, simulating slow per-candidate inspection/GC work
		// consuming the budget, so the second candidate's check fails.
		if calls > 2 {
			now = now.Add(time.Hour)
		}
		return now
	}

	cfg := testCfg(root)
	cfg.MaxElapsed = time.Second
	cfg.Now = clock
	result, err := DiscoverIdlePools(context.Background(), cfg)
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Scanned != 1 {
		t.Fatalf("scanned=%d, want exactly 1 under a tiny elapsed budget", result.Scanned)
	}
}

func TestIdlePoolDiscovery_FairContinuationEventuallyReachesEveryRoot(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	names := []string{"pool-fac-a", "pool-fac-b", "pool-fac-c", "pool-fac-d", "pool-fac-e"}
	for _, name := range names {
		makeIdlePoolRoot(t, root, name, "main")
	}

	seen := make(map[string]bool)
	for tick := 0; tick < len(names)+2; tick++ {
		cfg := testCfg(root)
		cfg.MaxRoots = 2
		result, err := ReclaimIdlePools(context.Background(), cfg)
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		for _, d := range result.Dispositions {
			seen[d.Root] = true
		}
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("root %s was never visited across %d ticks of fair continuation", name, len(names)+2)
		}
	}
}

func TestIdlePoolDiscovery_CorruptCursorRestartsInsteadOfFailing(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-only", "main")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idlePoolCursorPath(root), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := DiscoverIdlePools(context.Background(), testCfg(root))
	if err != nil {
		t.Fatalf("DiscoverIdlePools with corrupt cursor: %v", err)
	}
	if result.Eligible != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestIdlePoolDiscovery_ConcurrentTicksSerializeWithoutCorruption(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	names := []string{"pool-fac-a", "pool-fac-b", "pool-fac-c", "pool-fac-d"}
	for _, name := range names {
		makeIdlePoolRoot(t, root, name, "main")
	}

	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := testCfg(root)
			cfg.MaxRoots = 1
			_, err := ReclaimIdlePools(context.Background(), cfg)
			results[i] = err
		}(i)
	}
	wg.Wait()

	oneOfBusyOrNil := func(err error) bool {
		return err == nil || strings.Contains(err.Error(), "busy")
	}
	for i, err := range results {
		if !oneOfBusyOrNil(err) {
			t.Fatalf("tick %d returned an unexpected non-busy error: %v", i, err)
		}
	}
	// The cursor file, if present, must still be valid JSON -- no
	// interleaved partial writes corrupted it.
	if data, err := os.ReadFile(idlePoolCursorPath(root)); err == nil {
		var st idlePoolCursorState
		if jsonErr := json.Unmarshal(data, &st); jsonErr != nil {
			t.Fatalf("cursor file corrupted by concurrent ticks: %v (%s)", jsonErr, data)
		}
	}
}

// TestDiscoverIdlePools_CanceledContextIsRespectedNotSilentlyIgnored proves
// the per-candidate context.WithDeadline actually reaches the downstream
// git subprocess calls inside Pool.GCPlan: a caller-canceled context must
// surface as a real disposition (never a silent success and never a hang),
// and must never delete anything.
func TestDiscoverIdlePools_CanceledContextIsRespectedNotSilentlyIgnored(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-a", "main")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := DiscoverIdlePools(ctx, testCfg(root))
	if err != nil {
		t.Fatalf("DiscoverIdlePools with a pre-canceled context should still return a result, not an outer error: %v", err)
	}
	if result.Eligible != 0 {
		t.Fatalf("a canceled context must never be silently treated as eligible: %+v", result)
	}
	if result.Scanned != 1 || (result.Retained+result.Failed) != 1 {
		t.Fatalf("expected the one candidate to be scanned and refused (retained or failed), got %+v", result)
	}
	slots, err := NewPool(root, filepath.Join(root, ".herd", "pool-fac-a"), 0).Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("a canceled tick must not mutate anything: slots=%d err=%v", len(slots), err)
	}
}

// TestDiscoverIdlePools_PoolAbsentFromManifestRetains pins the positive-
// evidence contract at the discovery surface: a clean, unleased, registered,
// reachable pool root that the manifest registry never named is RETAINED,
// not eligible -- unknown scope is not automatically disposable.
func TestDiscoverIdlePools_PoolAbsentFromManifestRetains(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	makeIdlePoolRoot(t, root, "pool-fac-a", "main")
	// pool-fac-b gets NO evidence file rows: its manifest mention is
	// stripped after the fixture created it.
	evidenced := makeIdlePoolRoot(t, root, "pool-fac-b", "main")
	_ = evidenced
	manifestPath := filepath.Join(root, "retirement-manifests.jsonl")
	journalPath := filepath.Join(root, "retirement-phases.jsonl")
	stripRow := func(path, pool string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if !strings.Contains(line, `.herd/`+pool+`"`) {
				kept = append(kept, line)
			}
		}
		if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stripRow(manifestPath, "pool-fac-b")
	stripRow(journalPath, "pool-fac-b")

	result, err := DiscoverIdlePools(context.Background(), testCfg(root))
	if err != nil {
		t.Fatalf("DiscoverIdlePools: %v", err)
	}
	if result.Eligible != 1 || result.Retained != 1 {
		t.Fatalf("absent-evidence pool must retain while evidenced pool is eligible: %+v", result)
	}
	for _, d := range result.Dispositions {
		if d.Root == "pool-fac-b" && d.Status != IdlePoolRetained {
			t.Fatalf("pool-fac-b must be retained, got %+v", d)
		}
	}
}
