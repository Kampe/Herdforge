package resources

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Governor-seam coverage for the rotating registered census. The window tests
// next door drive the enumerator directly; these drive Governor.census, which
// is where the window hooks are wired, where the allocation walk is skipped,
// and where the stage's Scanned/Deferred are reported.

// recordingMeasurer records every path the allocation walk touched.
type recordingMeasurer struct {
	mu       sync.Mutex
	measured []string
}

func (m *recordingMeasurer) Measure(path string, _ int) (PhysicalUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.measured = append(m.measured, filepath.Clean(path))
	return PhysicalUsage{Bytes: 4096}, nil
}

func (m *recordingMeasurer) touched(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, seen := range m.measured {
		if seen == filepath.Clean(path) {
			return true
		}
	}
	return false
}

// censusSeamGovernor wires a real GitWorktreeEnumerator, with the window seam's
// hermetic doubles, onto a Governor over a throwaway repository. Nothing here
// touches a real fleet: the repository is built by windowSeamRepo in t.TempDir.
func censusSeamGovernor(t *testing.T, laneCount, window int) (*Governor, string, *recordingMeasurer, *windowSeamEvidence) {
	t.Helper()
	root, _ := windowSeamRepo(t, laneCount)
	evidence := &windowSeamEvidence{failFor: map[string]bool{}}
	measurer := &recordingMeasurer{}
	g := &Governor{
		Policy: GovernorPolicy{
			HostID: "census-seam", RepositoryRoot: root, BaseRef: "main",
			LockPath:       filepath.Join(root, "governor.lock"),
			MaxScanEntries: 64, LockTimeout: time.Second, LockRetry: time.Millisecond,
		},
		Worktrees: GitWorktreeEnumerator{
			Processes:    &windowSeamProcesses{},
			Evidence:     evidence,
			HostID:       "census-seam",
			Now:          time.Now,
			CensusWindow: window,
			Landing: func(ctx context.Context, probe LandingProbe) (bool, error) {
				return false, nil
			},
		},
		Capacity: &capacitySequence{values: []Capacity{capacity(900000, "census-seam-fs")}},
		Source:   staticSourceInspector{},
		Measure:  measurer,
		Locks:    &channelLocks{},
		Now:      func() time.Time { return time.Unix(200, 0).UTC() },
	}
	return g, root, measurer, evidence
}

// seedRegisteredCursor pins the window's start so the sweep is deterministic
// instead of falling back to the minute-bucket stride.
func seedRegisteredCursor(t *testing.T, g *Governor, at int) {
	t.Helper()
	path := g.registeredCursorPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(at)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func censusStage(t *testing.T, report GovernorReport) CensusStage {
	t.Helper()
	for _, stage := range report.Stages {
		if stage.Name == "registered_census" {
			return stage
		}
	}
	t.Fatal("registered_census stage missing from the report")
	return CensusStage{}
}

// The windowed census must report the whole inventory and every lane it left
// unknown. The stage previously counted only the failures it produced itself,
// so a normal sweep with most of the ring explicitly deferred reported zero.
func TestGovernorCensusStageCountsDeferredAndUnknownLanes(t *testing.T) {
	g, _, measurer, _ := censusSeamGovernor(t, 5, 2)
	g.defaults()
	seedRegisteredCursor(t, g, 0)

	report, err := g.census(context.Background())
	if err != nil {
		t.Fatalf("windowed census must answer: %v", err)
	}
	stage := censusStage(t, report)
	if len(report.Worktrees) == 0 {
		t.Fatal("census reported no registered worktrees; the fixture proves nothing")
	}
	if stage.Scanned != len(report.Worktrees) {
		t.Fatalf("stage Scanned %d must match the full inventory %d", stage.Scanned, len(report.Worktrees))
	}

	unknown := 0
	deferredPaths := make([]string, 0, len(report.Worktrees))
	measuredPaths := make([]string, 0, len(report.Worktrees))
	for _, lane := range report.Worktrees {
		if lane.State == LaneUnknown {
			unknown++
		}
		if lane.PreserveReason == "census_window_deferred" {
			deferredPaths = append(deferredPaths, lane.Path)
			continue
		}
		if lane.State != LaneUnknown {
			measuredPaths = append(measuredPaths, lane.Path)
		}
	}
	if len(deferredPaths) == 0 {
		t.Fatal("the window deferred no lane; widen the fixture or this case proves nothing")
	}
	if unknown == 0 {
		t.Fatal("no lane was left unknown; this case cannot detect the undercount")
	}
	if stage.Deferred != unknown {
		t.Fatalf("stage Deferred %d must equal the %d lanes left unknown", stage.Deferred, unknown)
	}

	// The allocation walk is the expensive step the window exists to bound.
	for _, path := range deferredPaths {
		if measurer.touched(path) {
			t.Fatalf("deferred lane %s paid for the allocation walk", path)
		}
	}
	if len(measuredPaths) == 0 {
		t.Fatal("no selected lane reached a known state; the measurement assertion below would be vacuous")
	}
	for _, path := range measuredPaths {
		if !measurer.touched(path) {
			t.Fatalf("selected known lane %s was never measured", path)
		}
	}
}

// A lane the ENUMERATOR could not prove arrives already unknown. It must be
// counted once, and must not be counted twice when the governor's own
// measurement would also have failed for it.
func TestGovernorCensusCountsEnumeratorUnknownsWithoutDoubleCounting(t *testing.T) {
	// Window wider than the ring, so every lane is selected and the case does
	// not depend on where git happens to order the seeded lanes.
	g, _, _, evidence := censusSeamGovernor(t, 5, 10)
	evidence.failFor["lane-0"] = true
	evidence.failFor["lane-1"] = true
	g.defaults()
	seedRegisteredCursor(t, g, 0)

	report, err := g.census(context.Background())
	if err != nil {
		t.Fatalf("census must answer despite lane-level evidence failures: %v", err)
	}
	stage := censusStage(t, report)

	unknown := 0
	lifecycleUnknown := 0
	for _, lane := range report.Worktrees {
		if lane.State == LaneUnknown {
			unknown++
		}
		if lane.PreserveReason == "canonical_lifecycle_evidence_unavailable" {
			lifecycleUnknown++
		}
	}
	if lifecycleUnknown != 2 {
		t.Fatalf("expected the 2 seeded lifecycle failures to survive into the report, got %d", lifecycleUnknown)
	}
	if stage.Deferred != unknown {
		t.Fatalf("stage Deferred %d must equal the %d distinct unknown lanes, counted once each", stage.Deferred, unknown)
	}
	if stage.Deferred < lifecycleUnknown {
		t.Fatalf("stage Deferred %d omits enumerator-origin unknown lanes (%d of them)", stage.Deferred, lifecycleUnknown)
	}
}

// A failed durable-cursor write is a partial diagnostic: it must be visible on
// the stage, must not fail the sweep, and must not promote any unknown lane.
func TestGovernorCensusCursorWriteFailureIsVisibleAndChangesNoLane(t *testing.T) {
	g, root, _, _ := censusSeamGovernor(t, 5, 2)
	g.defaults()
	// Block the cursor directory with a regular file so the advance cannot
	// create it. The read side falls back to the deterministic stride.
	blocked := filepath.Join(root, ".herd", "governor")
	if err := os.MkdirAll(filepath.Dir(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := g.census(context.Background())
	if err != nil {
		t.Fatalf("a broken cursor must not fail the sweep: %v", err)
	}
	stage := censusStage(t, report)
	if stage.CursorError == "" {
		t.Fatal("a failed cursor advance must surface as a partial diagnostic on the stage")
	}
	if len(report.Worktrees) == 0 {
		t.Fatal("census reported no registered worktrees; the fixture proves nothing")
	}
	unknown := 0
	for _, lane := range report.Worktrees {
		if lane.State != LaneUnknown {
			continue
		}
		unknown++
		if lane.PreserveReason == "" {
			t.Fatalf("unknown lane %s lost its fail-closed reason under a cursor failure", lane.Path)
		}
	}
	if unknown == 0 {
		t.Fatal("no lane was unknown; this case cannot show that unknown lanes stay fenced")
	}
	if stage.Deferred != unknown {
		t.Fatalf("stage Deferred %d must still equal the %d unknown lanes when the cursor write failed", stage.Deferred, unknown)
	}
}
