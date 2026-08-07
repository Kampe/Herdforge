package stop

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeFleet records every boundary call in order so ordering properties are
// provable, and serves a scripted sequence of live reads so an agent can
// settle (or refuse to) between the plan and the close.
type fakeFleet struct {
	calls    []string
	reads    [][]Agent
	readN    int
	stopErr  map[string]error
	closeErr map[string]error
}

func (f *fakeFleet) Agents() ([]Agent, error) {
	f.calls = append(f.calls, "list")
	if len(f.reads) == 0 {
		return nil, nil
	}
	i := f.readN
	if i >= len(f.reads) {
		i = len(f.reads) - 1
	}
	f.readN++
	return f.reads[i], nil
}

func (f *fakeFleet) RequestStop(paneID string) error {
	f.calls = append(f.calls, "stop:"+paneID)
	return f.stopErr[paneID]
}

func (f *fakeFleet) CloseTab(tabID string) error {
	f.calls = append(f.calls, "close:"+tabID)
	return f.closeErr[tabID]
}

func (f *fakeFleet) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func testExec(f *fakeFleet, posture func(context.Context) error, out io.Writer) Exec {
	return Exec{Fleet: f, EnterWinddown: posture, Out: out, Execute: true, Poll: time.Millisecond}
}

func okPosture(*fakeFleet) func(context.Context) error {
	return func(context.Context) error { return nil }
}

// AC: no new work starts after wind-down is durable. The posture write has to
// land before the first worker hears anything, or the kick loop can re-claim a
// worker while it settles.
func TestWinddownIsDurableBeforeAnyWorkerIsSignaled(t *testing.T) {
	f := &fakeFleet{reads: [][]Agent{{{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"}}}}
	order := []string{}
	posture := func(context.Context) error { order = append(order, "winddown"); return nil }
	plan := Plan([]Agent{{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"}}, Options{})
	if _, err := testExec(f, posture, io.Discard).Run(context.Background(), plan); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(order) != 1 || len(f.calls) == 0 || f.calls[0] != "stop:p1" {
		t.Fatalf("posture must precede the first fleet call: posture=%v calls=%v", order, f.calls)
	}

	// Fail-closed: an undurable posture signals nobody at all.
	f2 := &fakeFleet{}
	boom := errors.New("disk full")
	_, err := testExec(f2, func(context.Context) error { return boom }, io.Discard).Run(context.Background(), plan)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("undurable posture must abort: %v", err)
	}
	if len(f2.calls) != 0 {
		t.Fatalf("no worker may be touched without a durable posture: %v", f2.calls)
	}
}

// AC: active workers either finish a safe checkpoint or remain recoverable.
// Capacity is released only after the checkpoint is observed on a fresh read.
func TestCapacityReleasedOnlyAfterObservedCheckpoint(t *testing.T) {
	working := []Agent{{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"}}
	settled := []Agent{{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"}}
	plan := Plan(working, Options{})

	// Never settles inside the window: preserved, tab untouched.
	stuck := &fakeFleet{reads: [][]Agent{working}}
	var out strings.Builder
	e := testExec(stuck, okPosture(stuck), &out)
	e.Wait = 5 * time.Millisecond
	res, err := e.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stuck.count("close:") != 0 || res.Closed != 0 || res.Preserved != 1 {
		t.Fatalf("unsettled worker was not preserved: %+v calls=%v", res, stuck.calls)
	}
	if !strings.Contains(out.String(), "PRESERVE name=smith") {
		t.Fatalf("missing status output: %s", out.String())
	}

	// Settles on the second read: capacity released.
	drained := &fakeFleet{reads: [][]Agent{working, settled}}
	e = testExec(drained, okPosture(drained), io.Discard)
	e.Wait = time.Second
	res, err = e.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if drained.count("close:t1") != 1 || res.Closed != 1 || res.Signaled != 1 {
		t.Fatalf("checkpointed worker was not released: %+v calls=%v", res, drained.calls)
	}
}

// The plan is a snapshot. An agent that picked up work between planning and
// closing must not lose its tab to a stale decision.
func TestAgentActiveAtCloseTimeIsNeverClosed(t *testing.T) {
	planned := []Agent{{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"}}
	plan := Plan(planned, Options{})
	if plan[0].Action != Close {
		t.Fatalf("fixture must plan a close: %+v", plan[0])
	}

	raced := &fakeFleet{reads: [][]Agent{{{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"}}}}
	res, err := testExec(raced, okPosture(raced), io.Discard).Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if raced.count("close:") != 0 || res.Preserved != 1 {
		t.Fatalf("raced agent was closed on stale state: %+v calls=%v", res, raced.calls)
	}

	// Same plan, still idle on the fresh read: the close does happen.
	quiet := &fakeFleet{reads: [][]Agent{planned}}
	res, err = testExec(quiet, okPosture(quiet), io.Discard).Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if quiet.count("close:t1") != 1 || res.Closed != 1 {
		t.Fatalf("settled agent must still be closed: %+v calls=%v", res, quiet.calls)
	}
}

// Capacity is released exactly once even when several panes share a tab.
func TestTabIsClosedExactlyOnce(t *testing.T) {
	agents := []Agent{
		{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"},
		{Name: "scout", Status: "idle", PaneID: "p2", TabID: "t1"},
	}
	f := &fakeFleet{reads: [][]Agent{agents}}
	res, err := testExec(f, okPosture(f), io.Discard).Run(context.Background(), Plan(agents, Options{}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.count("close:t1") != 1 || res.Closed != 1 {
		t.Fatalf("shared tab closed %d times: %+v", f.count("close:t1"), res)
	}
}

// AC: a signal during the drain leaves every worker recoverable, and the
// durable posture is what a restart resumes from.
func TestSignalDuringDrainClosesNothing(t *testing.T) {
	working := []Agent{{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"}}
	f := &fakeFleet{reads: [][]Agent{working}}
	ctx, cancel := context.WithCancel(context.Background())
	postures := 0
	e := testExec(f, func(context.Context) error { postures++; return nil }, io.Discard)
	e.Wait = time.Minute
	e.Poll = 5 * time.Millisecond
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	res, err := e.Run(ctx, Plan(working, Options{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled drain must surface the cancellation: %v", err)
	}
	if f.count("close:") != 0 || res.Closed != 0 || res.Preserved != 1 {
		t.Fatalf("cancellation closed a tab: %+v calls=%v", res, f.calls)
	}
	if postures != 1 {
		t.Fatalf("posture writes = %d", postures)
	}

	// Restart: the same plan against a now-settled fleet completes the
	// shutdown, and the idempotent posture is re-entered rather than reset.
	f2 := &fakeFleet{reads: [][]Agent{{{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"}}}}
	e2 := testExec(f2, func(context.Context) error { postures++; return nil }, io.Discard)
	res, err = e2.Run(context.Background(), Plan(working, Options{}))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Closed != 1 || postures != 2 {
		t.Fatalf("restart did not resume the shutdown: %+v postures=%d", res, postures)
	}
}

// An unproven stop request means ownership was never checkpointed: the tab
// stays open unless the operator explicitly forces it.
func TestUnprovenStopRequestBlocksCloseUnlessForced(t *testing.T) {
	working := []Agent{{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"}}
	settled := []Agent{{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"}}
	deaf := map[string]error{"p1": errors.New("never confirmed consumption")}

	f := &fakeFleet{reads: [][]Agent{settled}, stopErr: deaf}
	res, err := testExec(f, okPosture(f), io.Discard).Run(context.Background(), Plan(working, Options{}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.count("close:") != 0 || res.Unproven != 1 || res.Preserved != 1 {
		t.Fatalf("unproven stop released capacity: %+v calls=%v", res, f.calls)
	}

	forced := &fakeFleet{reads: [][]Agent{settled}, stopErr: deaf}
	fe := testExec(forced, okPosture(forced), io.Discard)
	fe.ForceWorking = true
	res, err = fe.Run(context.Background(), Plan(working, Options{ForceWorking: true}))
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if forced.count("close:t1") != 1 || res.Closed != 1 {
		t.Fatalf("explicit force did not close: %+v calls=%v", res, forced.calls)
	}
}

// Force is the "kill them all" instruction and still only closes tabs: the
// boundary stop drives has no way to touch a worktree, branch, or ref.
func TestForceClosesUnsettledButOnlyEverClosesTabs(t *testing.T) {
	working := []Agent{{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"}}
	f := &fakeFleet{reads: [][]Agent{working}}
	e := testExec(f, okPosture(f), io.Discard)
	e.ForceWorking = true
	e.Wait = 5 * time.Millisecond
	res, err := e.Run(context.Background(), Plan(working, Options{ForceWorking: true}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Closed != 1 {
		t.Fatalf("force must close an unsettled worker: %+v", res)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "list") && !strings.HasPrefix(c, "stop:") && !strings.HasPrefix(c, "close:") {
			t.Fatalf("force reached beyond tab closure: %v", f.calls)
		}
	}
}

// A close that fails leaves the agent counted as preserved, never as released.
func TestFailedCloseIsNotCountedAsReleasedCapacity(t *testing.T) {
	agents := []Agent{{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"}}
	f := &fakeFleet{reads: [][]Agent{agents}, closeErr: map[string]error{"t1": errors.New("tab gone")}}
	res, err := testExec(f, okPosture(f), io.Discard).Run(context.Background(), Plan(agents, Options{}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Closed != 0 || res.Preserved != 1 {
		t.Fatalf("failed close counted as released: %+v", res)
	}
}

// The default run must be observable without being destructive.
func TestDryRunTouchesNeitherPostureNorFleet(t *testing.T) {
	agents := []Agent{
		{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"},
		{Name: "scout", Status: "idle", PaneID: "p2", TabID: "t2"},
		{Name: "coordinator", Status: "idle", PaneID: "p3", TabID: "t3"},
	}
	f := &fakeFleet{reads: [][]Agent{agents}}
	postures := 0
	var out strings.Builder
	res, err := Exec{
		Fleet:         f,
		EnterWinddown: func(context.Context) error { postures++; return nil },
		Out:           &out,
	}.Run(context.Background(), Plan(agents, Options{}))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if postures != 0 || len(f.calls) != 0 {
		t.Fatalf("dry run had side effects: postures=%d calls=%v", postures, f.calls)
	}
	if res.Closed != 1 || res.Preserved != 1 || res.Protected != 1 {
		t.Fatalf("dry run summary = %+v", res)
	}
	for _, want := range []string{"WOULD_CLOSE name=scout", "WOULD_PRESERVE name=smith", "PROTECT name=coordinator"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
}

// Execute without a boundary must refuse rather than silently no-op.
func TestExecuteRequiresBoundaries(t *testing.T) {
	for name, e := range map[string]Exec{
		"no fleet":   {EnterWinddown: func(context.Context) error { return nil }, Execute: true},
		"no posture": {Fleet: &fakeFleet{}, Execute: true},
	} {
		if _, err := e.Run(context.Background(), nil); err == nil {
			t.Fatalf("%s: expected refusal", name)
		}
	}
}

// A fleet re-read that fails is not evidence of settlement.
func TestUnreadableFleetNeverCloses(t *testing.T) {
	agents := []Agent{{Name: "smith", Status: "idle", PaneID: "p1", TabID: "t1"}}
	bad := &readFailFleet{}
	res, err := Exec{Fleet: bad, EnterWinddown: func(context.Context) error { return nil }, Out: io.Discard, Execute: true}.
		Run(context.Background(), Plan(agents, Options{}))
	if err == nil {
		t.Fatal("unreadable fleet must fail the stop")
	}
	if bad.closes != 0 || res.Closed != 0 {
		t.Fatalf("closed on an unreadable fleet: %+v", res)
	}
}

type readFailFleet struct{ closes int }

func (r *readFailFleet) Agents() ([]Agent, error) { return nil, errors.New("herdr unavailable") }
func (r *readFailFleet) RequestStop(string) error { return nil }
func (r *readFailFleet) CloseTab(string) error    { r.closes++; return nil }
