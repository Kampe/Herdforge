package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// FAC-613: a lifecycle refusal used to arrive with no way to tell WHICH census
// phase spent the budget. Two native review-preparation refusals at the
// unregistered-orphan census produced no phase attribution, although the
// sweep had already built a report carrying per-stage timings and counts and
// sweepResourceGovernor threw it away.
//
// These drive the REAL boundary: a real Governor over the package's own
// hermetic policy, through runLifecycleGovernorSweep, and the real
// lifecycleSweepFailure that sweepResourceGovernor now calls.

// diagnosticsFailingWorktrees refuses the registered census immediately, which
// is the path that records a stage and then returns the partial report.
type diagnosticsFailingWorktrees struct{ err error }

func (w diagnosticsFailingWorktrees) List(context.Context, string, string) ([]resources.RegisteredWorktree, error) {
	return nil, w.err
}

var errDiagnosticsCensus = errors.New("diagnostics census refused")

func diagnosticsGovernor(t *testing.T, lister resources.WorktreeEnumerator) *resources.Governor {
	t.Helper()
	return &resources.Governor{
		Policy: lifecycleTestPolicy(t, false), Capacity: lifecycleCapacityBackend{},
		Worktrees: lister, Locks: resources.FileLockProvider{},
	}
}

// A refusal keeps the stage evidence the sweep already collected.
func TestLifecycleRefusalRetainsCensusStages(t *testing.T) {
	governor := diagnosticsGovernor(t, diagnosticsFailingWorktrees{err: errDiagnosticsCensus})

	report, err := runLifecycleGovernorSweep(context.Background(), governor, resources.SweepReviewBeforeRefusal)
	if err == nil {
		t.Fatal("the fixture census must refuse")
	}
	if len(report.Stages) == 0 {
		t.Fatal("the sweep recorded no stages, so this test could not prove they survive")
	}

	wrapped := lifecycleSweepFailure(report, err)
	if wrapped == nil {
		t.Fatal("a refusal must stay a refusal")
	}
	if !strings.Contains(wrapped.Error(), "census stages:") {
		t.Fatalf("refusal carries no stage summary: %v", wrapped)
	}
	for _, stage := range report.Stages {
		if !strings.Contains(wrapped.Error(), stage.Name) {
			t.Fatalf("refusal omits the %q stage it recorded: %v", stage.Name, wrapped)
		}
	}
}

// The refusal's IDENTITY is unchanged, which is what every caller gates on.
func TestLifecycleRefusalPreservesErrorIdentity(t *testing.T) {
	governor := diagnosticsGovernor(t, diagnosticsFailingWorktrees{err: errDiagnosticsCensus})

	report, err := runLifecycleGovernorSweep(context.Background(), governor, resources.SweepReviewBeforeRefusal)
	if err == nil {
		t.Fatal("the fixture census must refuse")
	}
	wrapped := lifecycleSweepFailure(report, err)
	if !errors.Is(wrapped, errDiagnosticsCensus) {
		t.Fatalf("errors.Is no longer answers for the cause: %v", wrapped)
	}
	if !errors.Is(wrapped, err) {
		t.Fatalf("the original refusal is no longer reachable: %v", wrapped)
	}
}

// A CANCELLED caller still fails closed, and still as a cancellation.
func TestLifecycleCancellationKeepsItsIdentityWithStages(t *testing.T) {
	governor := diagnosticsGovernor(t, lifecycleCanceledAwareWorktrees{})
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := runLifecycleGovernorSweep(parent, governor, resources.SweepReviewBeforeRefusal)
	if err == nil {
		t.Fatal("a cancelled caller must fail closed")
	}
	wrapped := lifecycleSweepFailure(report, err)
	if wrapped == nil {
		t.Fatal("a cancelled sweep must not become success")
	}
	if !errors.Is(wrapped, context.Canceled) {
		t.Fatalf("cancellation lost its identity: %v", wrapped)
	}
}

// SUCCESS IS NOT TOUCHED: a sweep that admitted must not acquire an error.
//
// The failure message deliberately does NOT format the error: under a mutant
// that wraps a nil refusal, rendering it would panic, and a crash is not a
// kill. The assertion is the fact, not the value.
func TestLifecycleSuccessIsNeverWrapped(t *testing.T) {
	if err := lifecycleSweepFailure(resources.GovernorReport{
		Stages: []resources.CensusStage{{Name: "registered_census", DurationMS: 5}},
	}, nil); err != nil {
		t.Fatal("a successful sweep was turned into a refusal")
	}
}

// The summary is BOUNDED and carries identifiers and numbers only. A refusal
// is an operational line, not a report dump, and must not leak a path.
func TestCensusStageSummaryIsBoundedAndPathFree(t *testing.T) {
	many := make([]resources.CensusStage, 0, censusStageSummaryLimit+4)
	for i := 0; i < censusStageSummaryLimit+4; i++ {
		many = append(many, resources.CensusStage{
			Name: "registered_census", DurationMS: 1234, Scanned: 7, Deferred: 2,
			ProbeCompleted: 5, ProbeDeferred: 1,
			CursorError: "/var/folders/zz/cursor.state: no space left on device",
			Cause:       "stat /Users/someone/secret/path: permission denied",
		})
	}
	summary := censusStageSummary(many)
	if len(summary) > censusStageSummaryBytes+3 {
		t.Fatalf("summary is unbounded at %d bytes", len(summary))
	}
	if strings.Contains(summary, "/") {
		t.Fatalf("summary leaked a path: %q", summary)
	}
	if !strings.Contains(summary, "cursor_error") {
		t.Fatalf("a failed cursor advance must still be visible as a flag: %q", summary)
	}
	if strings.Contains(summary, "permission denied") {
		t.Fatalf("summary quoted a stage cause verbatim: %q", summary)
	}
}

// An empty report changes nothing: the caller still receives its own error.
func TestRefusalWithoutStagesIsReturnedUnchanged(t *testing.T) {
	wrapped := lifecycleSweepFailure(resources.GovernorReport{}, errDiagnosticsCensus)
	if wrapped != errDiagnosticsCensus {
		t.Fatalf("a stageless refusal was rewritten: %v", wrapped)
	}
}
