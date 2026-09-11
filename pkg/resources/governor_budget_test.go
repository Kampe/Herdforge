package resources

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

func (c *countingBatchInspector) InUseManyPopulation(ctx context.Context, paths []string, population *ProcessPopulation) (map[string]ProcessUsage, error) {
	c.inUseManyPopulationCalls++
	usage := make(map[string]ProcessUsage, len(paths))
	return usage, nil
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
	if report.Error != "" {
		t.Fatalf("clean run must stay clean: %v", report.Error)
	}
}

// The registered census must CAPTURE the population it used into the shared
// holder so the orphan stage finds it, and must REUSE a non-empty holder
// instead of walking again.
func TestGitWorktreeEnumeratorCapturesAndReusesSharedPopulation(t *testing.T) {
	snapshots := 0
	inspector := LSOFProcessInspector{
		Timeout: 2 * time.Second,
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
	enum := GitWorktreeEnumerator{Processes: inspector, Now: time.Now, HostID: "h", SharedPopulation: shared}
	lanes := []RegisteredWorktree{{Path: filepath.Join(root, "wt"), Branch: "b", Head: strings.Repeat("a", 40), State: LaneDone, Category: LaneTask}}
	if _, err := enum.evaluateForTest(context.Background(), root, lanes, "origin/main"); err != nil {
		t.Fatalf("first census: %v", err)
	}
	if snapshots != 1 || len(shared.PIDs) != 1 {
		t.Fatalf("first census must capture the population once: snapshots=%d pids=%d", snapshots, len(shared.PIDs))
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
