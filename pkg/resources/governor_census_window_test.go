package resources

import (
	"context"
	"errors"
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

// censusWindowRecordingMeasurer records every path the allocation walk touched
// and can fail for named lanes. The name is task-specific because this package
// already has an unrelated recordingMeasurer fixture in physical_context_test.go.
//
// Lanes are addressed by directory BASE name rather than full path: the
// governor resolves and cleans each lane path before measuring, so matching on
// the basename is stable against that rewrite.
type censusWindowRecordingMeasurer struct {
	mu       sync.Mutex
	failBase map[string]bool
	measured []string
	failed   []string
}

func (m *censusWindowRecordingMeasurer) Measure(path string, _ int) (PhysicalUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clean := filepath.Clean(path)
	m.measured = append(m.measured, clean)
	if m.failBase[filepath.Base(clean)] {
		m.failed = append(m.failed, clean)
		return PhysicalUsage{}, errors.New("census window fixture measurement failure")
	}
	return PhysicalUsage{Bytes: 4096}, nil
}

func (m *censusWindowRecordingMeasurer) touched(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, seen := range m.measured {
		if seen == filepath.Clean(path) {
			return true
		}
	}
	return false
}

func (m *censusWindowRecordingMeasurer) failedFor(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, seen := range m.failed {
		if seen == filepath.Clean(path) {
			return true
		}
	}
	return false
}

// censusSeamGovernor wires a real GitWorktreeEnumerator, with the window seam's
// hermetic doubles, onto a Governor over a throwaway repository. Nothing here
// touches a real fleet: the repository is built by windowSeamRepo in t.TempDir.
func censusSeamGovernor(t *testing.T, laneCount, window int) (*Governor, string, *censusWindowRecordingMeasurer, *windowSeamEvidence) {
	t.Helper()
	root, _ := windowSeamRepo(t, laneCount)
	evidence := &windowSeamEvidence{failFor: map[string]bool{}}
	measurer := &censusWindowRecordingMeasurer{failBase: map[string]bool{}}
	g := &Governor{
		// These fixtures call g.census directly, which Run and
		// AcquireDispatch only ever reach through validate(). The policy must
		// therefore satisfy the SAME invariants validate() enforces, or the
		// fixture is exercising census outside its contract: the first
		// version of this helper left PressureBytes and TaskReserveBytes
		// zero, and setConcurrency divided by that zero reserve.
		Policy: GovernorPolicy{
			HostID: "census-seam", RepositoryRoot: root, BaseRef: "main",
			LockPath:               filepath.Join(root, "governor.lock"),
			GeneratedDirectories:   []string{"node_modules"},
			PressureBytes:          1000,
			RecoveryBytes:          2000,
			TaskReserveBytes:       1000,
			MaxDispatchConcurrency: 4,
			ReapBatchLimit:         2,
			MaxScanEntries:         64,
			LockTimeout:            time.Second,
			LockRetry:              time.Millisecond,
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
	// defaults() wires the window hooks onto the concrete enumerator, and the
	// validate() that follows is the contract check every direct-census
	// fixture owes: it fails HERE, naming the invalid field, instead of
	// panicking deep inside the arithmetic that assumes it.
	g.defaults()
	if err := g.Policy.validate(); err != nil {
		t.Fatalf("census fixture policy must satisfy the same validation Run enforces: %v", err)
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
// counted once -- INCLUDING when the governor's own allocation measurement
// then fails for that same lane, which is the only way a single lane can be
// charged twice by a running counter.
func TestGovernorCensusCountsEnumeratorUnknownsWithoutDoubleCounting(t *testing.T) {
	// Window wider than the ring, so every lane is selected and the case does
	// not depend on where git happens to order the seeded lanes.
	g, _, measurer, evidence := censusSeamGovernor(t, 5, 10)
	// lane-0 fails BOTH: lifecycle evidence in the enumerator, then the
	// governor's allocation walk. lane-1 fails only lifecycle, so the
	// enumerator-origin case is still covered on its own.
	evidence.failFor["lane-0"] = true
	evidence.failFor["lane-1"] = true
	measurer.failBase["lane-0"] = true
	seedRegisteredCursor(t, g, 0)

	report, err := g.census(context.Background())
	if err != nil {
		t.Fatalf("census must answer despite lane-level failures: %v", err)
	}
	stage := censusStage(t, report)
	if len(report.Worktrees) == 0 {
		t.Fatal("census reported no registered worktrees; the fixture proves nothing")
	}

	var doubleFailed, lifecycleOnly *RegisteredWorktree
	unknown, known := 0, 0
	for i := range report.Worktrees {
		lane := &report.Worktrees[i]
		if lane.State == LaneUnknown {
			unknown++
		} else {
			known++
		}
		switch lane.Branch {
		case "lane-0":
			doubleFailed = lane
		case "lane-1":
			lifecycleOnly = lane
		}
	}
	if doubleFailed == nil || lifecycleOnly == nil {
		t.Fatalf("fixture lanes missing from the report: lane-0=%v lane-1=%v", doubleFailed, lifecycleOnly)
	}

	// Positive proof the double failure actually happened, rather than the
	// measurer simply never being asked.
	if !measurer.failedFor(doubleFailed.Path) {
		t.Fatalf("lane-0's allocation measurement never failed, so this case cannot detect a double count (measured=%v)", measurer.touched(doubleFailed.Path))
	}
	if doubleFailed.State != LaneUnknown {
		t.Fatalf("lane-0 failed lifecycle evidence AND measurement and must stay unknown, got %v", doubleFailed.State)
	}
	if lifecycleOnly.State != LaneUnknown || lifecycleOnly.PreserveReason != "canonical_lifecycle_evidence_unavailable" {
		t.Fatalf("lane-1 must keep its enumerator-origin reason, got state=%v reason=%q", lifecycleOnly.State, lifecycleOnly.PreserveReason)
	}
	if unknown < 2 {
		t.Fatalf("expected at least the two seeded failures to be unknown, got %d", unknown)
	}
	if known == 0 {
		t.Fatal("no lane reached a known state; the fixture is not exercising the healthy path at all")
	}

	// The exact detection: a counter that charged lane-0 for both its
	// lifecycle failure and its measurement failure would report unknown+1.
	if stage.Deferred != unknown {
		t.Fatalf("stage Deferred %d must equal the %d distinct unknown lanes, counting the double-failed lane once", stage.Deferred, unknown)
	}
	if stage.Scanned != len(report.Worktrees) {
		t.Fatalf("stage Scanned %d must match the full inventory %d", stage.Scanned, len(report.Worktrees))
	}
}

// A failed durable-cursor write is a partial diagnostic: it must be visible on
// the stage, must not fail the sweep, and must not promote any unknown lane.
func TestGovernorCensusCursorWriteFailureIsVisibleAndChangesNoLane(t *testing.T) {
	g, root, _, _ := censusSeamGovernor(t, 5, 2)
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
