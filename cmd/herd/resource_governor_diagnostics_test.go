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
// unregistered-orphan census produced no phase attribution, although the sweep
// had already built a report carrying per-stage timings and counts.
//
// These drive the REAL boundary and inspect the error it RETURNS. Nothing here
// decorates the error itself: a test that called the decorator would still
// pass if the boundary stopped calling it, which is the defect.

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

// THE REGRESSION: the boundary's own error carries the stage evidence.
func TestLifecycleBoundaryRefusalCarriesCensusStages(t *testing.T) {
	governor := diagnosticsGovernor(t, diagnosticsFailingWorktrees{err: errDiagnosticsCensus})

	report, err := runLifecycleGovernorSweep(context.Background(), governor, resources.SweepReviewBeforeRefusal)
	if err == nil {
		t.Fatal("the fixture census must refuse")
	}
	if len(report.Stages) == 0 {
		t.Fatal("the sweep recorded no stages, so this test could not prove they survive")
	}
	if !strings.Contains(err.Error(), "census stages:") {
		t.Fatalf("the boundary returned a refusal with no stage summary: %v", err)
	}
	for _, stage := range report.Stages {
		if !strings.Contains(err.Error(), stage.Name) {
			t.Fatalf("the refusal omits the %q stage the sweep recorded: %v", stage.Name, err)
		}
	}
}

// The refusal's IDENTITY is unchanged, which is what every caller gates on.
func TestLifecycleBoundaryRefusalPreservesErrorIdentity(t *testing.T) {
	governor := diagnosticsGovernor(t, diagnosticsFailingWorktrees{err: errDiagnosticsCensus})

	_, err := runLifecycleGovernorSweep(context.Background(), governor, resources.SweepReviewBeforeRefusal)
	if err == nil {
		t.Fatal("the fixture census must refuse")
	}
	if !errors.Is(err, errDiagnosticsCensus) {
		t.Fatalf("errors.Is no longer answers for the cause: %v", err)
	}
}

// A CANCELLED caller still fails closed, and still as a cancellation.
func TestLifecycleBoundaryCancellationKeepsItsIdentity(t *testing.T) {
	governor := diagnosticsGovernor(t, lifecycleCanceledAwareWorktrees{})
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runLifecycleGovernorSweep(parent, governor, resources.SweepReviewBeforeRefusal)
	if err == nil {
		t.Fatal("a cancelled caller must fail closed")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost its identity: %v", err)
	}
}

// SUCCESS IS NOT TOUCHED. The message deliberately does not format the error:
// under a mutant that wraps a nil refusal, rendering it would panic, and a
// crash is not a kill.
func TestLifecycleSuccessIsNeverWrapped(t *testing.T) {
	if err := lifecycleSweepFailure(resources.GovernorReport{
		Stages: []resources.CensusStage{{Name: "registered_census", DurationMS: 5}},
	}, nil); err != nil {
		t.Fatal("a successful sweep was turned into a refusal")
	}
}

// representativeStages is the shape a real sweep produces: statfs first, the
// registered census next, and the unregistered-orphan census LAST — the phase
// the two field refusals stopped in.
func representativeStages() []resources.CensusStage {
	return []resources.CensusStage{
		{Name: "statfs", DurationMS: 12},
		{Name: "registered_census", DurationMS: 31500, Scanned: 148, Deferred: 22,
			ProbeCompleted: 126, ProbeDeferred: 22},
		{Name: "unregistered_orphan_census", DurationMS: 13200, Scanned: 64, Deferred: 9,
			ProbeCompleted: 55, ProbeDeferred: 9,
			CursorError: "/var/folders/zz/cursor.state: no space left on device"},
	}
}

// The failing phase and its numbers must survive, whatever else does not.
func TestSummaryKeepsTheFailingPhaseAndItsCounts(t *testing.T) {
	summary := censusStageSummary(representativeStages())

	for _, want := range []string{
		"unregistered_orphan_census", "ms=13200", "scanned=64", "deferred=9",
		"probe_completed=55", "probe_deferred=9", "cursor_error",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("the failing phase lost %q: %s", want, summary)
		}
	}
	if len(summary) > censusStageSummaryBytes {
		t.Fatalf("summary is unbounded at %d bytes: %s", len(summary), summary)
	}
}

// When the set does not fit, the EARLIEST stages go and the omission is
// stated. Dropping from the end would discard the phase that refused.
func TestSummaryDropsEarliestStagesAndSaysSo(t *testing.T) {
	stages := representativeStages()
	for i := 0; i < 30; i++ {
		stages = append([]resources.CensusStage{{
			Name: "registered_census", DurationMS: 9999, Scanned: 999, Deferred: 999,
			ProbeCompleted: 999, ProbeDeferred: 999,
		}}, stages...)
	}

	summary := censusStageSummary(stages)
	if len(summary) > censusStageSummaryBytes {
		t.Fatalf("summary is unbounded at %d bytes", len(summary))
	}
	if !strings.Contains(summary, "earlier stage(s) omitted") {
		t.Fatalf("stages were dropped without saying so: %s", summary)
	}
	if !strings.Contains(summary, "unregistered_orphan_census ms=13200") {
		t.Fatalf("truncation discarded the failing phase: %s", summary)
	}
}

// Identifiers and numbers only: no path, and no verbatim stage cause.
func TestSummaryCarriesNoPathOrCause(t *testing.T) {
	summary := censusStageSummary([]resources.CensusStage{{
		Name: "unregistered_orphan_census", DurationMS: 1,
		CursorError: "/var/folders/zz/cursor.state: no space left on device",
		Cause:       "stat /Users/someone/secret/path: permission denied",
	}})
	if strings.Contains(summary, "/") {
		t.Fatalf("summary leaked a path: %q", summary)
	}
	if strings.Contains(summary, "permission denied") {
		t.Fatalf("summary quoted a stage cause verbatim: %q", summary)
	}
	if !strings.Contains(summary, "cursor_error") {
		t.Fatalf("a failed cursor advance must remain visible as a flag: %q", summary)
	}
}

// An empty report changes nothing: the caller receives its own error.
func TestRefusalWithoutStagesIsReturnedUnchanged(t *testing.T) {
	if got := lifecycleSweepFailure(resources.GovernorReport{}, errDiagnosticsCensus); got != errDiagnosticsCensus {
		t.Fatalf("a stageless refusal was rewritten: %v", got)
	}
}
