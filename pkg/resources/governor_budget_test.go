package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-613: a failed census must still return its structured partial report —
// phase timing, scanned/deferred counts, and the causal stage — alongside the
// nonzero error. The refusal stays; the report becomes actionable.
func TestGovernorCensusReturnsPartialReportWhenOrphanStageFails(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000)
	// An orphan root that cannot be read fails the orphan stage AFTER the
	// registered stage completed - exactly the retained production shape.
	orphanRoot := filepath.Join(g.Policy.RepositoryRoot, ".herd", "worktrees")
	if err := os.MkdirAll(filepath.Dir(orphanRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.Policy.OrphanRoots = []string{orphanRoot}
	g.SharedPopulation = &ProcessPopulation{}
	report, err := g.Run(context.Background(), RunOptions{})
	if err == nil {
		t.Fatal("a failed orphan census must remain a failed run")
	}
	if !strings.Contains(err.Error(), "unregistered-orphan census") {
		t.Fatalf("error must name the failing phase: %v", err)
	}
	if report.Error == "" {
		t.Fatal("the partial report must carry the causal error")
	}
	var registered, orphan *CensusStage
	for i := range report.Stages {
		switch report.Stages[i].Name {
		case "registered_census":
			registered = &report.Stages[i]
		case "unregistered_orphan_census":
			orphan = &report.Stages[i]
		}
	}
	if registered == nil || registered.Scanned != 1 {
		t.Fatalf("registered stage must be recorded with its scanned count: %+v", report.Stages)
	}
	if orphan == nil || orphan.Cause == "" {
		t.Fatalf("orphan stage must be recorded with its cause: %+v", report.Stages)
	}
	data, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !strings.Contains(string(data), `"census_stages"`) || !strings.Contains(string(data), `"error"`) {
		t.Fatalf("partial JSON must expose stages and error: %s", data)
	}
}

// FAC-613: the orphan census stage must reuse the population snapshot the
// registered stage captured instead of rescanning the process population.
type countingBatchInspector struct {
	inUseManyCalls           int
	inUseManyPopulationCalls int
	snapshotCalls            int
}

func (c *countingBatchInspector) InUseMany(ctx context.Context, paths []string) (map[string]ProcessUsage, error) {
	c.inUseManyCalls++
	usage := make(map[string]ProcessUsage, len(paths))
	return usage, nil
}

func (c *countingBatchInspector) InUseManyPopulation(ctx context.Context, paths []string, population *ProcessPopulation) (map[string]ProcessUsage, BatchProbeStats, error) {
	c.inUseManyPopulationCalls++
	usage := make(map[string]ProcessUsage, len(paths))
	return usage, BatchProbeStats{Completed: len(paths)}, nil
}

func (c *countingBatchInspector) InUse(context.Context, string) (ProcessUsage, error) {
	return ProcessUsage{}, nil
}

func TestGovernorOrphanStageReusesSharedPopulation(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000)
	orphanRoot := filepath.Join(g.Policy.RepositoryRoot, ".herd", "worktrees")
	if err := os.MkdirAll(filepath.Join(orphanRoot, "orphan-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanRoot, "orphan-a", "graph.db"), []byte("graph"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.Policy.OrphanRoots = []string{orphanRoot}
	g.Policy.OrphanDerivedTargets = []string{"graph.db"}
	g.Policy.OrphanCacheTTL = time.Hour
	g.Policy.OrphanCacheBudgetBytes = 1 << 20
	counter := &countingBatchInspector{}
	g.Processes = counter
	g.SharedPopulation = &ProcessPopulation{PIDs: []int{4242}, Owners: map[int]int{4242: os.Getuid()}}
	report, err := g.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if counter.inUseManyPopulationCalls != 1 {
		t.Fatalf("orphan stage must run exactly one population-reusing batch census: %+v", counter)
	}
	if counter.inUseManyCalls != 0 || counter.snapshotCalls != 0 {
		t.Fatalf("no rescan may occur when the population is shared: %+v", counter)
	}
	var orphanStage *CensusStage
	for i := range report.Stages {
		if report.Stages[i].Name == "unregistered_orphan_census" {
			orphanStage = &report.Stages[i]
		}
	}
	if orphanStage == nil || orphanStage.ProbeCompleted != 1 || orphanStage.ProbeDeferred != 0 {
		t.Fatalf("orphan stage must state its real target-probe progress: %+v", report.Stages)
	}
	if report.Error != "" {
		t.Fatalf("clean run must stay clean: %v", report.Error)
	}
}

// The registered census must CAPTURE the population it used into the shared
// holder so the orphan stage finds it, and must REUSE a non-empty holder
// instead of walking again.
func TestGitWorktreeEnumeratorCapturesAndReusesSharedPopulation(t *testing.T) {
	// Hermetic lsof stand-in: exits 0 with no output, so the batch probe is
	// deterministic and host-lsof independent.
	lsof := filepath.Join(t.TempDir(), "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	snapshots := 0
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 15 * time.Second,
		populationFn: func(context.Context) ([]int, map[int]int, error) {
			snapshots++
			return []int{4242}, map[int]int{4242: os.Getuid()}, nil
		},
		processReferencesManyFn: func(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error) {
			refs := make(map[string]bool, len(paths))
			return refs, nil
		},
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := &ProcessPopulation{}
	probeStats := &BatchProbeStats{}
	enum := GitWorktreeEnumerator{Processes: inspector, Now: time.Now, HostID: "h", SharedPopulation: shared, ProbeStats: probeStats}
	lanes := []RegisteredWorktree{{Path: filepath.Join(root, "wt"), Branch: "b", Head: strings.Repeat("a", 40), State: LaneDone, Category: LaneTask}}
	if _, err := enum.evaluateForTest(context.Background(), root, lanes, "origin/main"); err != nil {
		t.Fatalf("first census: %v", err)
	}
	if snapshots != 1 || len(shared.PIDs) != 1 {
		t.Fatalf("first census must capture the population once: snapshots=%d pids=%d", snapshots, len(shared.PIDs))
	}
	if probeStats.Completed != 1 || probeStats.Deferred != 0 {
		t.Fatalf("registered batch must record its target-probe progress: %+v", probeStats)
	}
	if _, err := enum.evaluateForTest(context.Background(), root, lanes, "origin/main"); err != nil {
		t.Fatalf("second census: %v", err)
	}
	if snapshots != 1 {
		t.Fatalf("second census must reuse the shared population: snapshots=%d", snapshots)
	}
}

// evaluateForTest runs the enumerator's lane evaluation over pre-listed
// lanes; the production List path resolves lanes from git first.
func (e GitWorktreeEnumerator) evaluateForTest(ctx context.Context, root string, lanes []RegisteredWorktree, baseRef string) ([]RegisteredWorktree, error) {
	if len(lanes) == 0 {
		return nil, errors.New("registered worktree allowlist is empty")
	}
	return e.evaluate(ctx, root, lanes, baseRef)
}

// FAC-613 correction: the batched census must probe ALL targets in bounded
// chunks instead of capping the lsof spawn at 64 and silently deferring the
// remainder. With 100 targets the fake lsof must be spawned twice, every
// target — including the late ones past the old cap — must receive real
// lsof evidence, and the reported stats must state completed=100/deferred=0.
func TestLSOFBatchProbesAllTargetsBeyondBatchCap(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	root := t.TempDir()
	countFile := filepath.Join(root, "spawns")
	script := "#!/bin/sh\necho run >> " + countFile + "\nshift 2\nwhile [ $# -ge 2 ]; do\n  printf 'p99999\\nf3\\nn%s\\n' \"$2\"\n  shift 2\ndone\n"
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 15 * time.Second,
		processReferencesManyFn: func(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error) {
			refs := make(map[string]bool, len(paths))
			return refs, nil
		},
	}
	var paths []string
	for i := 0; i < 100; i++ {
		dir := filepath.Join(root, fmt.Sprintf("wt-%03d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, dir)
	}
	population := &ProcessPopulation{PIDs: []int{4242}, Owners: map[int]int{4242: os.Getuid()}}
	usage, stats, err := inspector.InUseManyPopulation(context.Background(), paths, population)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Completed != 100 || stats.Deferred != 0 {
		t.Fatalf("all 100 targets must be probed in bounded chunks, got stats=%+v", stats)
	}
	if len(usage) != 100 {
		t.Fatalf("every target must receive an evidence entry, got %d", len(usage))
	}
	spawns, readErr := os.ReadFile(countFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := strings.Count(string(spawns), "run"); got != 2 {
		t.Fatalf("100 targets must need exactly 2 bounded lsof spawns, got %d", got)
	}
	// Late targets past the old 64-target cap carry real evidence: an open
	// file descriptor, not a silently deferred clean default.
	for i, rawPath := range paths {
		resolved, resolveErr := filepath.EvalSymlinks(rawPath)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		entry, ok := usage[filepath.Clean(resolved)]
		if !ok {
			t.Fatalf("target %d missing from evidence map", i)
		}
		if !entry.OpenFile {
			t.Fatalf("late target %d must carry real lsof evidence, got %+v", i, entry)
		}
		if entry.MetadataUnavailable {
			t.Fatalf("target %d must not be metadata-unknown when its probe completed: %s", i, entry.MetadataCause)
		}
	}
}

// The chunked batch fails closed: when the probe budget dies before late
// chunks run — or a chunk's lsof spawn fails — those targets must stay
// metadata-unknown with the cause, never read as evidence-clean, and the
// deferred count must state exactly how many probes did not complete.
func TestLSOFBatchFailClosedWhenLateProbeChunksIncomplete(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	root := t.TempDir()
	countFile := filepath.Join(root, "spawns")
	script := "#!/bin/sh\nruns=$(cat " + countFile + " 2>/dev/null || echo 0)\nruns=$((runs+1))\necho $runs > " + countFile + "\nshift 2\nif [ $runs -gt 1 ]; then\n  exit 7\nfi\nwhile [ $# -ge 2 ]; do\n  printf 'p99999\\nf3\\nn%s\\n' \"$2\"\n  shift 2\ndone\n"
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 15 * time.Second,
		processReferencesManyFn: func(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error) {
			refs := make(map[string]bool, len(paths))
			return refs, nil
		},
	}
	var paths []string
	for i := 0; i < 100; i++ {
		dir := filepath.Join(root, fmt.Sprintf("wt-%03d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, dir)
	}
	population := &ProcessPopulation{PIDs: []int{4242}, Owners: map[int]int{4242: os.Getuid()}}
	usage, stats, err := inspector.InUseManyPopulation(context.Background(), paths, population)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Completed != 64 || stats.Deferred != 36 {
		t.Fatalf("stats must state the real probe split, got %+v", stats)
	}
	// First-chunk targets keep their evidence; every later target is
	// metadata-unknown with the causal error — never a clean default.
	for i := 0; i < 64; i++ {
		resolved, resolveErr := filepath.EvalSymlinks(paths[i])
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if entry := usage[filepath.Clean(resolved)]; !entry.OpenFile || entry.MetadataUnavailable {
			t.Fatalf("probed target %d must keep its evidence: %+v", i, entry)
		}
	}
	for i := 64; i < 100; i++ {
		resolved, resolveErr := filepath.EvalSymlinks(paths[i])
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		entry := usage[filepath.Clean(resolved)]
		if !entry.MetadataUnavailable {
			t.Fatalf("unprobed target %d must fail closed, got %+v", i, entry)
		}
		if !strings.Contains(entry.MetadataCause, "lsof target probe failed") {
			t.Fatalf("unprobed target %d must carry the cause, got %q", i, entry.MetadataCause)
		}
	}
}

// A lane whose batch entry is metadata-unknown must stay unknown with the
// process-evidence preserve reason — it can never read as reap-eligible
// evidence-clean output. Full stack: a real git repository (status and merge
// gates must pass) plus an admitting evidence reader, so the run actually
// reaches the batched process-evidence consumption.
func TestGitWorktreeEnumeratorLaneUnknownWhenBatchMetadataUnavailable(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "seed.txt")
	run("commit", "-q", "-m", "seed")
	run("branch", "origin/main")
	lanePath := filepath.Join(root, "wt")
	if err := os.MkdirAll(lanePath, 0o700); err != nil {
		t.Fatal(err)
	}
	inspector := &unavailableMetadataBatchInspector{path: lanePath}
	enum := GitWorktreeEnumerator{
		Processes: inspector, Now: time.Now, HostID: "h",
		Evidence: stubLifecycleEvidence{},
	}
	lanes := []RegisteredWorktree{{Path: lanePath, Branch: "b", Head: strings.Repeat("a", 40), State: LaneDone, Category: LaneTask}}
	got, err := enum.evaluateForTest(context.Background(), root, lanes, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != LaneUnknown || got[0].PreserveReason != "process_evidence_unavailable" {
		t.Fatalf("metadata-unknown batch evidence must keep the lane unknown, got state=%s reason=%s", got[0].State, got[0].PreserveReason)
	}
}

type unavailableMetadataBatchInspector struct{ path string }

func (u *unavailableMetadataBatchInspector) InUse(context.Context, string) (ProcessUsage, error) {
	return ProcessUsage{}, nil
}

func (u *unavailableMetadataBatchInspector) InUseMany(ctx context.Context, paths []string) (map[string]ProcessUsage, error) {
	resolved, resolveErr := filepath.EvalSymlinks(u.path)
	if resolveErr != nil {
		resolved = u.path
	}
	usage := map[string]ProcessUsage{filepath.Clean(resolved): {MetadataUnavailable: true, MetadataCause: "lsof target probe failed: injected"}}
	return usage, nil
}

type stubLifecycleEvidence struct{}

func (stubLifecycleEvidence) Read(context.Context, string, string, RegisteredWorktree) (LifecycleEvidence, error) {
	return LifecycleEvidence{}, nil
}
