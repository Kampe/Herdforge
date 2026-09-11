package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// countdownContext reports cancellation after a fixed number of Err() calls, so
// a walk can be cancelled at an exact point instead of racing a timer.
type countdownContext struct {
	context.Context
	mu sync.Mutex
	n  int
}

func (c *countdownContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n <= 0 {
		return context.Canceled
	}
	c.n--
	return nil
}

func physicalTree(t *testing.T, files int) string {
	t.Helper()
	root := t.TempDir()
	for i := range files {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d", i)), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPhysicalMeasureContextStopsVisitingOnCancellation(t *testing.T) {
	root := physicalTree(t, 20)
	// One Err() is consumed by the pre-walk guard, five by visited entries.
	ctx := &countdownContext{Context: context.Background(), n: 6}
	usage, err := OSPhysicalMeasurer{}.MeasureContext(ctx, root, 1000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled walk error = %v, want context.Canceled", err)
	}
	if usage.Entries != 5 {
		t.Fatalf("cancelled walk visited %d entries, want exactly 5: traversal did not stop", usage.Entries)
	}
	if !usage.Truncated {
		t.Fatalf("cancelled walk usage=%+v, want Truncated so no caller reads it as complete", usage)
	}
}

func TestPhysicalMeasureContextRefusesAlreadyCancelledContext(t *testing.T) {
	root := physicalTree(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	usage, err := OSPhysicalMeasurer{}.MeasureContext(ctx, root, 1000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled walk error = %v, want context.Canceled", err)
	}
	if usage.Bytes != 0 || usage.Entries != 0 || !usage.Truncated {
		t.Fatalf("pre-cancelled usage=%+v, want zero and Truncated", usage)
	}
}

func TestPhysicalMeasureNormalMeasurementUnchanged(t *testing.T) {
	root := physicalTree(t, 12)
	legacy, err := OSPhysicalMeasurer{}.Measure(root, 1000)
	if err != nil {
		t.Fatalf("legacy Measure: %v", err)
	}
	live, err := OSPhysicalMeasurer{}.MeasureContext(context.Background(), root, 1000)
	if err != nil {
		t.Fatalf("MeasureContext: %v", err)
	}
	if legacy != live {
		t.Fatalf("context-aware measurement drifted: legacy=%+v live=%+v", legacy, live)
	}
	if live.Bytes == 0 || live.Entries != 13 || live.Truncated {
		t.Fatalf("live measurement=%+v, want 13 entries, non-zero bytes, not truncated", live)
	}
}

func TestPhysicalMeasureContextStillHonorsEntryBudget(t *testing.T) {
	root := physicalTree(t, 20)
	usage, err := OSPhysicalMeasurer{}.MeasureContext(context.Background(), root, 4)
	if err != nil {
		t.Fatalf("bounded walk: %v", err)
	}
	if !usage.Truncated || usage.Entries != 5 {
		t.Fatalf("entry budget=%+v, want truncated at the 5th entry", usage)
	}
}

type measureCall struct {
	path        string
	contextual  bool
	hasDeadline bool
	ctxErr      error
}

// recordingMeasurer wraps a measurer and records how the governor reached it.
type recordingMeasurer struct {
	inner PhysicalMeasurer
	fail  error

	mu    sync.Mutex
	calls []measureCall
}

func (r *recordingMeasurer) record(call measureCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *recordingMeasurer) snapshot() []measureCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]measureCall(nil), r.calls...)
}

func (r *recordingMeasurer) Measure(path string, maxEntries int) (PhysicalUsage, error) {
	r.record(measureCall{path: path})
	return r.inner.Measure(path, maxEntries)
}

func (r *recordingMeasurer) MeasureContext(ctx context.Context, path string, maxEntries int) (PhysicalUsage, error) {
	_, hasDeadline := ctx.Deadline()
	r.record(measureCall{path: path, contextual: true, hasDeadline: hasDeadline, ctxErr: ctx.Err()})
	if r.fail != nil {
		return PhysicalUsage{Truncated: true}, r.fail
	}
	if aware, ok := r.inner.(ContextPhysicalMeasurer); ok {
		return aware.MeasureContext(ctx, path, maxEntries)
	}
	return r.inner.Measure(path, maxEntries)
}

// legacyMeasurer implements only PhysicalMeasurer, proving the governor keeps
// working with test and custom measurers that never learn about contexts.
type legacyMeasurer struct{ inner OSPhysicalMeasurer }

func (l legacyMeasurer) Measure(path string, maxEntries int) (PhysicalUsage, error) {
	return l.inner.Measure(path, maxEntries)
}

func TestGovernorSharedDeadlineReachesNativeWalk(t *testing.T) {
	g, _, _ := governorFor(t, "host", 900000, 850000)
	measurer := &recordingMeasurer{inner: OSPhysicalMeasurer{}}
	g.Measure = measurer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := g.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	calls := measurer.snapshot()
	if len(calls) == 0 {
		t.Fatal("governor measured nothing")
	}
	bounded := 0
	for _, call := range calls {
		if !call.contextual {
			t.Fatalf("governor measured %q through the context-blind path; the sweep deadline cannot reach that walk", call.path)
		}
		if call.hasDeadline {
			bounded++
		}
	}
	if bounded == 0 {
		t.Fatalf("no measurement carried the sweep deadline: %+v", calls)
	}
}

func TestGovernorCancelledMeasurementIsNeverEligible(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000, 850000)
	// The context-blind path would succeed and make the target look reapable;
	// only the context-aware path reports the cancellation.
	g.Measure = &recordingMeasurer{inner: OSPhysicalMeasurer{}, fail: context.DeadlineExceeded}
	report, err := g.Run(context.Background(), RunOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Reaped != 0 || report.ReclaimedBytes != 0 {
		t.Fatalf("cancelled measurement produced reclaim: reaped=%d bytes=%d", report.Reaped, report.ReclaimedBytes)
	}
	if len(report.Worktrees) != 1 || report.Worktrees[0].State != LaneUnknown ||
		report.Worktrees[0].PreserveReason != "worktree_allocation_unavailable" {
		t.Fatalf("lane after cancelled measurement=%+v, want unknown and preserved", report.Worktrees)
	}
	for _, row := range report.Targets {
		if row.Decision == TargetWouldReap || row.Decision == TargetReaped {
			t.Fatalf("unknown allocation became eligible: %+v", row)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target was mutated on an unknown measurement: %v", err)
	}
}

func TestGovernorAcceptsMeasurerWithoutContextSupport(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000, 850000, 840000)
	g.Measure = legacyMeasurer{}
	report, err := g.Run(context.Background(), RunOptions{Apply: true, BatchLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Reaped != 1 || report.Targets[0].Decision != TargetReaped || report.Targets[0].BeforeBytes == 0 {
		t.Fatalf("legacy measurer broke normal measurement: %+v", report)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target survived reap: %v", err)
	}
}

func TestGovernorRefusesLegacyMeasurerOnCancelledSweep(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000, 850000)
	g.Measure = legacyMeasurer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := g.Run(ctx, RunOptions{Apply: true})
	if err == nil && report.Reaped != 0 {
		t.Fatalf("cancelled sweep reaped through a context-blind measurer: %+v", report)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("cancelled sweep mutated the target: %v", statErr)
	}
}

// The removal is already committed and the accounting runs under the reap's
// lock: a sweep cancelled at that instant must still read back, or the governor
// releases its lock mid-accounting and reports bytes it cannot prove.
func TestGovernorPostReapReadbackSurvivesCancellation(t *testing.T) {
	g, target, _ := governorFor(t, "host", 900000, 850000, 840000)
	measurer := &recordingMeasurer{inner: OSPhysicalMeasurer{}}
	g.Measure = measurer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removed := false
	g.RemoveTree = func(path string) error {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		// The sweep's deadline expires the instant the mutation commits.
		removed = true
		cancel()
		return nil
	}
	report, err := g.Run(ctx, RunOptions{Apply: true, BatchLimit: 1})
	if err != nil {
		t.Fatalf("cancellation during a committed removal failed the sweep: %v", err)
	}
	if !removed {
		t.Fatal("removal never ran")
	}
	if report.Reaped != 1 || report.Targets[0].Decision != TargetReaped || report.Targets[0].AfterBytes != 0 {
		t.Fatalf("post-reap accounting lost: %+v", report.Targets)
	}
	if report.ReclaimedBytes == 0 {
		t.Fatal("committed removal reported zero reclaimed bytes")
	}
	calls := measurer.snapshot()
	last := calls[len(calls)-1]
	// The governor measures realpaths; t.TempDir hands back the symlinked form.
	wantPath, resolveErr := filepath.EvalSymlinks(filepath.Dir(target))
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	wantPath = filepath.Join(wantPath, filepath.Base(target))
	if last.path != wantPath {
		t.Fatalf("last measurement was %q, want the post-reap readback of %q", last.path, wantPath)
	}
	if last.ctxErr != nil {
		t.Fatalf("post-reap readback ran on a cancelled context (%v); the readback must outlive the sweep deadline", last.ctxErr)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target survived: %v", statErr)
	}
}
