package resources

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Causal reproduction of the production failure 2681 (installed 2277f2b4,
// observed 03:50:32Z, exit 1 after 72.06s): all 576 registered worktrees
// came back process_evidence_unavailable with probe_completed 0 /
// probe_deferred 576 despite batching, the registered_census stage was
// recorded twice, and the unregistered-orphan census died instantly with
// "context deadline exceeded" because the registered phase consumed the
// whole 45s budget.
//
// The wiring defects were:
//  1. the registered_census stage was appended twice;
//  2. the population snapshot carried no open-file table, so the census
//     still ran per-target lsof +D chunks whose fixed 2s deadline cannot
//     observe a 64-target subtree descent on a real host — every chunk
//     failed and every lane went evidence-unavailable;
//  3. the registered phase had no budget reservation or ctx-aware loop, so
//     the allocation walks starved the orphan phase.
//
// These tests pin the corrected wiring. The fake lsof executable fails
// instantly so a regression back into per-target spawning stays observable.
func TestGovernorEnumeratorBulkOpenFileEvidence(t *testing.T) {
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
	lanePaths := []string{
		filepath.Join(root, "wt-holds"),
		filepath.Join(root, "wt-clean"),
	}
	for _, lanePath := range lanePaths {
		if err := os.MkdirAll(lanePath, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	heldPID := 4242
	heldResolved, resolveErr := filepath.EvalSymlinks(lanePaths[0])
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	heldResolved = filepath.Clean(heldResolved)
	var spawned int
	inspector := LSOFProcessInspector{
		Executable: failFastLsofScript(t, &spawned),
		populationFn: func(context.Context) ([]int, map[int]int, error) {
			return []int{heldPID}, map[int]int{heldPID: os.Getuid()}, nil
		},
		openFilesFn: func(context.Context) (map[int][]ProcessOpenFile, error) {
			// One open file DEEP INSIDE wt-holds: the subtree match must
			// attribute it to the lane root; fd 3 is an ordinary open file.
			return map[int][]ProcessOpenFile{heldPID: {{
				FD: "3", Path: filepath.Join(heldResolved, "sub", "dir", "graph.db"),
			}}}, nil
		},
		processReferencesManyFn: func(_ context.Context, _ int, paths []string, _ map[int]int) (map[string]bool, error) {
			references := make(map[string]bool, len(paths))
			for _, path := range paths {
				references[path] = false
			}
			return references, nil
		},
	}
	stats := &BatchProbeStats{}
	enum := GitWorktreeEnumerator{
		Processes: inspector, Now: time.Now, HostID: "h",
		Evidence: stubLifecycleEvidence{}, SharedPopulation: &ProcessPopulation{}, ProbeStats: stats,
	}
	lanes := make([]RegisteredWorktree, 0, len(lanePaths))
	for i, lanePath := range lanePaths {
		lanes = append(lanes, RegisteredWorktree{
			Path: lanePath, Branch: "b", Head: strings.Repeat("a", 40),
			State: LaneDone, Category: LaneTask, LastUse: time.Unix(100, 0).UTC(),
		})
		lanes[i].Path = lanePath
	}
	got, err := enum.evaluateForTest(context.Background(), root, lanes, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if spawned != 0 {
		t.Fatalf("bulk evidence must never spawn per-target lsof, got %d spawns", spawned)
	}
	if stats.Completed != 2 || stats.Deferred != 0 {
		t.Fatalf("bulk open-file evidence must complete every target probe, got completed=%d deferred=%d", stats.Completed, stats.Deferred)
	}
	if !got[0].OpenFile || got[0].State != LaneActive {
		t.Fatalf("wt-holds holds an open file and must read active/open, got open=%t state=%s", got[0].OpenFile, got[0].State)
	}
	if got[1].State == LaneUnknown || got[1].PreserveReason == "process_evidence_unavailable" {
		t.Fatalf("wt-clean with completed bulk evidence must stay decidable, got %+v", got[1])
	}
}

// The registered_census stage must be recorded exactly once, and a census
// budget that expires mid-loop must stop the allocation walk and mark the
// remaining lanes fail-closed unknown instead of starving the orphan phase
// into an immediate deadline failure (production 2681: orphan died at 26ms).
func TestGovernorCensusSingleStageAndBudgetAwareLaneLoop(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000)
	g.Processes = batchOnlyProcessInspector{}
	root := filepath.Join(g.Policy.RepositoryRoot, ".herd", "worktrees")
	g.Policy.OrphanRoots = []string{root}
	g.Policy.OrphanDerivedTargets = []string{"graph.db"}
	g.Policy.OrphanCacheTTL, g.Policy.OrphanCacheBudgetBytes = time.Hour, 1<<20
	for i := 0; i < 20; i++ {
		orphan := filepath.Join(root, fmt.Sprintf("small-%02d", i))
		if err := os.MkdirAll(orphan, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(orphan, "graph.db"), []byte("immutable"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lanes := &staticWorktrees{}
	for i := 0; i < 20; i++ {
		lanePath := filepath.Join(g.Policy.RepositoryRoot, fmt.Sprintf("lane-%02d", i))
		if err := os.MkdirAll(lanePath, 0o700); err != nil {
			t.Fatal(err)
		}
		lanes.lanes = append(lanes.lanes, RegisteredWorktree{
			Path:   lanePath,
			Branch: "b", Head: strings.Repeat("a", 40), Category: LaneTask,
			State: LaneDone, LastUse: time.Unix(100, 0).UTC(),
		})
	}
	g.Worktrees = lanes
	// A deadline whose registered share (4/5) expires inside the first
	// allocation walk: the loop must stop there and mark the remaining
	// lanes fail-closed, leaving the orphan phase a live budget.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	budget := &cancelingMeasurer{release: 900 * time.Millisecond}
	g.Measure = budget
	report, err := g.Run(ctx, RunOptions{})
	if err != nil {
		t.Fatalf("budget exhaustion is a partial diagnostic, not a run failure: %v", err)
	}
	registeredStages := 0
	for _, stage := range report.Stages {
		if stage.Name == "registered_census" {
			registeredStages++
		}
	}
	if registeredStages != 1 {
		t.Fatalf("registered_census stage must be recorded exactly once, got %d: %+v", registeredStages, report.Stages)
	}
	exhausted := 0
	for i := range report.Worktrees {
		if report.Worktrees[i].PreserveReason == "census_budget_exhausted" {
			exhausted++
		}
	}
	if exhausted == 0 {
		t.Fatalf("lanes left unmeasured by the expired budget must be fail-closed unknown: %+v", report.Worktrees)
	}
	for _, stage := range report.Stages {
		if stage.Name == "unregistered_orphan_census" && strings.Contains(stage.Cause, "context deadline exceeded") {
			t.Fatalf("orphan phase must not starve on the registered phase budget: %+v", report.Stages)
		}
	}
}

// cancelingMeasurer blocks the FIRST allocation walk past the registered
// phase budget share, simulating the production budget expiry mid-loop.
type cancelingMeasurer struct {
	release time.Duration
	calls   int
}

func (m *cancelingMeasurer) Measure(path string, limit int) (PhysicalUsage, error) {
	m.calls++
	if m.calls == 1 {
		time.Sleep(m.release)
	}
	return PhysicalUsage{Bytes: 4096}, nil
}

func failFastLsofScript(t *testing.T, spawned *int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lsof-failfast")
	script := "#!/bin/sh\necho spawned >> " + path + ".count\nexit 9\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(path + ".count"); err == nil {
			*spawned = len(strings.Split(strings.TrimSpace(string(data)), "\n"))
		}
	})
	return path
}
