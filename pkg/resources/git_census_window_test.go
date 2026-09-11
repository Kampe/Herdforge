package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// windowSeamGit runs a git command in dir with the seam identity, failing the
// test on any error. The same hermetic env contract as the landing seam.
func windowSeamGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seam", "GIT_AUTHOR_EMAIL=seam@herdforge.local",
		"GIT_COMMITTER_NAME=seam", "GIT_COMMITTER_EMAIL=seam@herdforge.local",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// windowSeamRepo builds a canonical checkout with base committed and count
// registered worktrees. Lanes at even indexes hold no unique commit (their
// HEAD is an ancestor of the base, so ancestry alone proves them landed);
// odd-index lanes hold one unique commit each.
func windowSeamRepo(t *testing.T, count int) (string, []RegisteredWorktree) {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	windowSeamGit(t, root, "init", "-q", "-b", "main", ".")
	windowSeamGit(t, root, "config", "user.email", "seam@herdforge.local")
	windowSeamGit(t, root, "config", "user.name", "seam")
	windowSeamGit(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	windowSeamGit(t, root, "add", ".")
	windowSeamGit(t, root, "commit", "-qm", "base")
	lanes := make([]RegisteredWorktree, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("lane-%d", i)
		dir := filepath.Join(root, name)
		windowSeamGit(t, root, "worktree", "add", "-q", "-b", name, dir)
		if i%2 == 1 {
			if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte("unique work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			windowSeamGit(t, dir, "add", ".")
			windowSeamGit(t, dir, "commit", "-qm", "unique work "+name)
		}
		head := strings.TrimSpace(windowSeamGit(t, dir, "rev-parse", "HEAD"))
		lanes = append(lanes, RegisteredWorktree{Path: dir, Branch: name, Head: head})
	}
	return root, lanes
}

// windowSeamEvidence records every lane whose lease evidence was read. A
// wanted error fails the Read for exactly the lanes the fixture names.
type windowSeamEvidence struct {
	mu     sync.Mutex
	read   []string
	failFor map[string]bool
	// onReadHook, when set, runs after a read is recorded: the cancellation
	// fixture uses it to fire the sweep's context mid-window.
	onReadHook func(branch string)
}

func (e *windowSeamEvidence) Read(ctx context.Context, repoRoot, hostID string, lane RegisteredWorktree) (LifecycleEvidence, error) {
	e.mu.Lock()
	e.read = append(e.read, lane.Branch)
	e.mu.Unlock()
	if e.onReadHook != nil {
		e.onReadHook(lane.Branch)
	}
	if e.failFor[lane.Branch] {
		return LifecycleEvidence{}, errors.New("seam lifecycle evidence failure")
	}
	return LifecycleEvidence{}, nil
}

// windowSeamProcesses records every path the batch population probe touched.
type windowSeamProcesses struct {
	mu     sync.Mutex
	probed []string
}

func (p *windowSeamProcesses) InUse(ctx context.Context, path string) (ProcessUsage, error) {
	return ProcessUsage{}, nil
}

func (p *windowSeamProcesses) InUseMany(ctx context.Context, paths []string) (map[string]ProcessUsage, error) {
	p.mu.Lock()
	p.probed = append(p.probed, paths...)
	p.mu.Unlock()
	usage := make(map[string]ProcessUsage, len(paths))
	for _, path := range paths {
		usage[path] = ProcessUsage{}
	}
	return usage, nil
}

type windowSeamEnumerator struct {
	enumerator GitWorktreeEnumerator
	landing    []string
	processes  *windowSeamProcesses
	evidence   *windowSeamEvidence
	cursor     int
	advances   []int
	onRead     func(branch string)
}

func newWindowSeamEnumerator(t *testing.T, root string, window int) *windowSeamEnumerator {
	t.Helper()
	seam := &windowSeamEnumerator{
		processes: &windowSeamProcesses{},
		evidence:  &windowSeamEvidence{failFor: map[string]bool{}},
	}
	seam.enumerator = GitWorktreeEnumerator{
		Processes: seam.processes,
		Now:       time.Now,
		HostID:    "seam",
		Evidence:  seam.evidence,
		CensusWindow: window,
		WindowStart: func(entryCount int) int {
			if seam.cursor < 0 || seam.cursor >= entryCount {
				seam.cursor %= entryCount
			}
			return seam.cursor
		},
		WindowAdvance: func(next int) error {
			seam.advances = append(seam.advances, next)
			seam.cursor = next
			return nil
		},
		Landing: func(ctx context.Context, probe LandingProbe) (bool, error) {
			seam.landing = append(seam.landing, probe.Branch)
			return false, nil
		},
	}
	return seam
}

func windowSeamIndex(lanes []RegisteredWorktree, branch string) int {
	for i := range lanes {
		if lanes[i].Branch == branch {
			return i
		}
	}
	return -1
}

// The rotating window must bound EVERY expensive per-lane operation to the
// selected slice: unselected lanes carry the explicit census_window_deferred
// reason, no landing predicate / lease evidence / process probe reaches them,
// and a selected lane still reaches a meaningful final state.
func TestRegisteredCensusWindowBoundsExpensiveOpsAndQualifiesSelected(t *testing.T) {
	root, lanes := windowSeamRepo(t, 5)
	seam := newWindowSeamEnumerator(t, root, 2)

	got, err := seam.enumerator.evaluate(context.Background(), root, lanes, "main")
	if err != nil {
		t.Fatalf("windowed evaluate must answer: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("the full registered ring must stay in the report, got %d lanes", len(got))
	}
	// Cursor starts at 0, window 2: lanes 0 and 1 are selected.
	selected := map[string]bool{"lane-0": true, "lane-1": true}
	for _, lane := range got {
		if selected[lane.Branch] {
			continue
		}
		if lane.State != LaneUnknown || lane.PreserveReason != "census_window_deferred" {
			t.Fatalf("unselected lane %s must be preserved as census_window_deferred unknown, got state=%v reason=%q", lane.Branch, lane.State, lane.PreserveReason)
		}
	}
	// Lane 0 holds no unique commit: ancestry proves it landed and no active
	// lease exists, so the state is a definitive done, not unknown.
	if got[0].State != LaneDone {
		t.Fatalf("selected merged-ancestor lane must reach a meaningful state, got %v (%q)", got[0].State, got[0].PreserveReason)
	}
	// Lane 1 holds a unique commit and nothing active: definitive idle.
	if got[1].State != LaneIdle {
		t.Fatalf("selected unmerged idle lane must reach a meaningful state, got %v (%q)", got[1].State, got[1].PreserveReason)
	}
	if seam.evidence.read == nil || len(seam.evidence.read) != 2 || seam.evidence.read[0] != "lane-0" || seam.evidence.read[1] != "lane-1" {
		t.Fatalf("lease evidence must be read for exactly the selected lanes in order, got %v", seam.evidence.read)
	}
	for _, branch := range seam.landing {
		if !selected[branch] {
			t.Fatalf("landing predicate reached unselected lane %s", branch)
		}
	}
	for _, path := range seam.processes.probed {
		for _, lane := range got {
			if lane.Branch == "lane-2" || lane.Branch == "lane-3" || lane.Branch == "lane-4" {
				if filepath.Clean(path) == filepath.Clean(lane.Path) {
					t.Fatalf("process probe reached unselected lane %s", lane.Branch)
				}
			}
		}
	}
	if len(seam.advances) != 1 || seam.advances[0] != 2 {
		t.Fatalf("cursor must advance by the window exactly once after accounting, got %v", seam.advances)
	}
}

// The next sweep must rotate: with the cursor the previous sweep advanced,
// a different slice pays for the evidence and the former window defers.
func TestRegisteredCensusWindowRotatesToNextSlice(t *testing.T) {
	root, lanes := windowSeamRepo(t, 5)
	seam := newWindowSeamEnumerator(t, root, 2)
	if _, err := seam.enumerator.evaluate(context.Background(), root, lanes, "main"); err != nil {
		t.Fatalf("first sweep must answer: %v", err)
	}
	if seam.cursor != 2 {
		t.Fatalf("first sweep must advance the cursor to 2, got %d", seam.cursor)
	}
	seam.landing = nil
	seam.evidence.read = nil
	seam.processes.probed = nil

	got, err := seam.enumerator.evaluate(context.Background(), root, lanes, "main")
	if err != nil {
		t.Fatalf("second sweep must answer: %v", err)
	}
	selected := map[string]bool{"lane-2": true, "lane-3": true}
	for _, lane := range got {
		if selected[lane.Branch] {
			continue
		}
		if lane.State != LaneUnknown || lane.PreserveReason != "census_window_deferred" {
			t.Fatalf("rotated-out lane %s must defer, got state=%v reason=%q", lane.Branch, lane.State, lane.PreserveReason)
		}
	}
	if len(seam.evidence.read) != 2 || seam.evidence.read[0] != "lane-2" || seam.evidence.read[1] != "lane-3" {
		t.Fatalf("second sweep must read evidence for exactly the rotated-in lanes, got %v", seam.evidence.read)
	}
	if seam.advances[len(seam.advances)-1] != 4 {
		t.Fatalf("second sweep must advance the cursor to 4, got %v", seam.advances)
	}
}

// A selected lane whose lease evidence fails must stay conservatively unknown
// with the existing fence reason; the window must not turn an evidence
// failure into a definitive state.
func TestRegisteredCensusWindowSelectedEvidenceFailureStaysUnknown(t *testing.T) {
	root, lanes := windowSeamRepo(t, 5)
	seam := newWindowSeamEnumerator(t, root, 2)
	seam.evidence.failFor["lane-1"] = true

	got, err := seam.enumerator.evaluate(context.Background(), root, lanes, "main")
	if err != nil {
		t.Fatalf("windowed evaluate must answer despite a lane-level evidence failure: %v", err)
	}
	if got[1].State != LaneUnknown || got[1].PreserveReason != "canonical_lifecycle_evidence_unavailable" {
		t.Fatalf("selected lane with failed evidence must stay unknown, got state=%v reason=%q", got[1].State, got[1].PreserveReason)
	}
}

// A cancellation that lands mid-window must advance the cursor by the lanes
// actually accounted, never by the window's full width: the unexamined
// remainder stays conservatively unknown this sweep and is RE-PROCESSED by
// the next sweep instead of being skipped.
func TestRegisteredCensusWindowCancellationAdvancesByAccountedLanes(t *testing.T) {
	root, lanes := windowSeamRepo(t, 5)
	seam := newWindowSeamEnumerator(t, root, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seam.evidence.onReadHook = func(branch string) {
		if branch == "lane-0" {
			cancel()
		}
	}

	got, err := seam.enumerator.evaluate(ctx, root, lanes, "main")
	if err != nil {
		t.Fatalf("a cancelled sweep must still answer with the partial census: %v", err)
	}
	// lane-0 completed its whole evidence pipeline before cancellation.
	if got[0].State != LaneDone {
		t.Fatalf("the lane accounted before cancellation must keep its definitive state, got %v (%q)", got[0].State, got[0].PreserveReason)
	}
	// lane-1 was selected but never examined: conservative unknown, and NOT
	// the deferred reason (it is the next sweep's first lane, not skipped).
	if got[1].State != LaneUnknown || got[1].PreserveReason != "census_budget_exhausted" {
		t.Fatalf("unexamined remainder must stay census_budget_exhausted unknown, got state=%v reason=%q", got[1].State, got[1].PreserveReason)
	}
	for _, lane := range got[2:] {
		if lane.State != LaneUnknown || lane.PreserveReason != "census_window_deferred" {
			t.Fatalf("unselected lane %s must defer, got state=%v reason=%q", lane.Branch, lane.State, lane.PreserveReason)
		}
	}
	if len(seam.advances) != 1 || seam.advances[0] != 1 {
		t.Fatalf("cursor must advance by the one accounted lane only, got %v", seam.advances)
	}

	// The next sweep re-processes the unexamined remainder: lane-1 pays for
	// its evidence this time and reaches its definitive state.
	seam.evidence.read = nil
	seam.evidence.onReadHook = nil
	got, err = seam.enumerator.evaluate(context.Background(), root, lanes, "main")
	if err != nil {
		t.Fatalf("second sweep must answer: %v", err)
	}
	if len(seam.evidence.read) != 2 || seam.evidence.read[0] != "lane-1" || seam.evidence.read[1] != "lane-2" {
		t.Fatalf("next sweep must account the unexamined remainder first, got %v", seam.evidence.read)
	}
	if got[1].State != LaneIdle {
		t.Fatalf("the previously unexamined lane must reach a definitive state on reprocessing, got %v (%q)", got[1].State, got[1].PreserveReason)
	}
}

// The default window (CensusWindow unset) is the defensible small bound: a
// 20-lane ring pays for exactly 16 lanes per sweep and the rest defer.
func TestRegisteredCensusWindowDefaultBoundsWorkPerSweep(t *testing.T) {
	root, lanes := windowSeamRepo(t, 20)
	seam := newWindowSeamEnumerator(t, root, 0)

	got, err := seam.enumerator.evaluate(context.Background(), root, lanes, "main")
	if err != nil {
		t.Fatalf("default-window evaluate must answer: %v", err)
	}
	if len(seam.evidence.read) != 16 {
		t.Fatalf("default window must bound expensive evidence to exactly 16 lanes, got %d: %v", len(seam.evidence.read), seam.evidence.read)
	}
	if len(seam.processes.probed) != 16 {
		t.Fatalf("default window must bound the process batch to exactly 16 paths, got %d", len(seam.processes.probed))
	}
	for _, lane := range got[16:] {
		if lane.State != LaneUnknown || lane.PreserveReason != "census_window_deferred" {
			t.Fatalf("lanes beyond the default window must defer, lane %s got state=%v reason=%q", lane.Branch, lane.State, lane.PreserveReason)
		}
	}
	if seam.advances[len(seam.advances)-1] != 16 {
		t.Fatalf("default window must advance the cursor by 16, got %v", seam.advances)
	}
}

// An enumerator without the cursor hook keeps the exact legacy behavior: the
// whole registered ring is evaluated every sweep.
func TestRegisteredCensusWindowUnwiredEnumeratesAllLanes(t *testing.T) {
	root, lanes := windowSeamRepo(t, 5)
	enumerator := GitWorktreeEnumerator{
		Processes: &windowSeamProcesses{},
		Now:       time.Now,
		HostID:    "seam",
		Evidence:  &windowSeamEvidence{failFor: map[string]bool{}},
	}
	got, err := enumerator.evaluate(context.Background(), root, lanes, "main")
	if err != nil {
		t.Fatalf("unwired evaluate must answer: %v", err)
	}
	for _, lane := range got {
		if lane.PreserveReason == "census_window_deferred" {
			t.Fatalf("unwired enumerator deferred %s; windowing must be off without the hook", lane.Branch)
		}
	}
}
