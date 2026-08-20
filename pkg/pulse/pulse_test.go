package pulse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixedNow is the fake clock anchor for all deterministic tests.
var fixedNow = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func healthyObs() Observation {
	return Observation{
		Provider: ProviderObservation{Known: true, QueueDepth: 2, Claimable: 2, InProgress: 1},
		Herdr: HerdrObservation{
			Known: true,
			Agents: []AgentObservation{
				{Name: "smith", Raw: "idle"},
				{Name: "worker", Raw: "working"},
				{Name: "reviewer", Raw: "done"},
			},
		},
		Leases: []LeaseObservation{
			{
				Repo: "herdforge", Provider: "kaneo", Project: "p",
				TaskRef: "FAC-1", OwnerID: "w1", Generation: 3, Active: true,
				ClaimedAt: fixedNow.Add(-8 * time.Minute),
				RenewedAt: fixedNow.Add(-8 * time.Minute),
				ExpiresAt: fixedNow.Add(2 * time.Minute), // half of 10m TTL → needs renew
			},
		},
		Callbacks: []CallbackObservation{
			{EnvelopeID: "env-1", Sequence: 10, Ref: "FAC-1", Kind: "complete", LeaseGeneration: 3},
			{EnvelopeID: "env-2", Sequence: 11, Ref: "FAC-2", Kind: "blocked", LeaseGeneration: 1},
		},
		Review:   ReviewObservation{Known: true, Pending: 0, NeedReview: 0},
		Quota:    QuotaObservation{Known: true},
		WindDown: WindDownObservation{Known: true, Enabled: false},
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		raw   string
		stale bool
		want  AgentStatus
	}{
		{"idle", false, StatusHealthyIdle},
		{"working", false, StatusBusy},
		{"starting", false, StatusBusy},
		{"blocked", false, StatusBlocked},
		{"done", false, StatusDone},
		{"idle", true, StatusStale},
		{"", false, StatusUnknown},
		{"mystery", false, StatusUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyStatus(tc.raw, tc.stale); got != tc.want {
			t.Fatalf("ClassifyStatus(%q, %v)=%s want %s", tc.raw, tc.stale, got, tc.want)
		}
	}
}

func TestPlanDeterministicSameInputsSameOrderedActions(t *testing.T) {
	obs := healthyObs()
	opts := Options{Act: true, Now: fixedNow, Reason: "test", BeatSequence: 7}
	a, err := Plan(obs, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Plan(obs, opts)
	if err != nil {
		t.Fatal(err)
	}
	aj, _ := json.Marshal(a.Actions)
	bj, _ := json.Marshal(b.Actions)
	if string(aj) != string(bj) {
		t.Fatalf("actions not deterministic:\nA=%s\nB=%s", aj, bj)
	}
	// Sequence positions are 1..n contiguous.
	for i, act := range a.Actions {
		if act.Sequence != i+1 {
			t.Fatalf("action[%d].Sequence=%d want %d", i, act.Sequence, i+1)
		}
	}
	if a.BeatSequence != 7 {
		t.Fatalf("beat sequence = %d", a.BeatSequence)
	}
}

func TestPlanObserveDoesNotMarkSafeMutations(t *testing.T) {
	snap, err := Plan(healthyObs(), Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != ModeObserve {
		t.Fatalf("mode=%s", snap.Mode)
	}
	for _, a := range snap.Actions {
		if a.Safe {
			t.Fatalf("observe action must not be Safe: %+v", a)
		}
		if a.Kind != ActionWouldRun {
			t.Fatalf("observe action kind=%s want would_run: %+v", a.Kind, a)
		}
	}
	if snap.Counts.WouldRun == 0 {
		t.Fatal("expected would_run actions in observe mode")
	}
}

func TestPlanActPlansRenewAndConsume(t *testing.T) {
	snap, err := Plan(healthyObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != ModeAct {
		t.Fatalf("mode=%s", snap.Mode)
	}
	var renew, consume int
	for _, a := range snap.Actions {
		switch a.Kind {
		case ActionRenewLease:
			renew++
			if !a.Safe || !strings.Contains(a.Reason, "generation 3") {
				t.Fatalf("renew action: %+v", a)
			}
		case ActionConsumeCallback:
			consume++
			if !a.Safe {
				t.Fatalf("consume not safe: %+v", a)
			}
		case ActionDispatch:
			t.Fatalf("dispatch must not plan without --spawn: %+v", a)
		}
	}
	if renew != 1 || consume != 2 {
		t.Fatalf("renew=%d consume=%d counts=%+v", renew, consume, snap.Counts)
	}
	if snap.Counts.RenewLeases != 1 || snap.Counts.ConsumeCallback != 2 {
		t.Fatalf("counts mismatch: %+v", snap.Counts)
	}
}

func TestPlanUnknownCriticalBlocksDispatchAndNonZeroExit(t *testing.T) {
	obs := healthyObs()
	obs.Provider = ProviderObservation{Known: false, Error: "kaneo timeout"}
	snap, err := Plan(obs, Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnknownCritical {
		t.Fatal("expected unknown critical")
	}
	if snap.ExitCode == 0 {
		t.Fatal("unknown critical must be non-zero exit")
	}
	if !snap.DispatchBlocked {
		t.Fatal("dispatch must be blocked")
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionDispatch {
			t.Fatalf("must not plan dispatch under unknown critical: %+v", a)
		}
	}
	// Renew/consume remain plannable — they do not require free capacity.
	if snap.Counts.RenewLeases+snap.Counts.ConsumeCallback == 0 {
		t.Fatal("safe renew/consume should still be planned")
	}
}

func TestPlanHerdrErrorIsNotFreeCapacity(t *testing.T) {
	obs := healthyObs()
	obs.Herdr = HerdrObservation{Known: false, Error: "herdr list failed"}
	// Clear claimable so the only path to dispatch would be mis-reading error as empty fleet.
	obs.Provider.Claimable = 5
	obs.Provider.QueueDepth = 5
	snap, err := Plan(obs, Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnknownCritical || snap.ExitCode == 0 {
		t.Fatalf("herdr error must be critical unknown: %+v", snap)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionDispatch {
			t.Fatal("dispatch planned despite herdr error")
		}
	}
}

func TestPlanDoesNotRenewNonActiveOrExpiredGeneration(t *testing.T) {
	obs := healthyObs()
	obs.Leases = []LeaseObservation{
		{ // stale generation, not active
			Repo: "r", Provider: "p", Project: "j", TaskRef: "FAC-9",
			OwnerID: "w", Generation: 1, Active: false,
			ExpiresAt: fixedNow.Add(time.Minute),
			ClaimedAt: fixedNow.Add(-9 * time.Minute),
			RenewedAt: fixedNow.Add(-9 * time.Minute),
		},
		{ // expired active — refuse renew
			Repo: "r", Provider: "p", Project: "j", TaskRef: "FAC-10",
			OwnerID: "w", Generation: 2, Active: true,
			ExpiresAt: fixedNow.Add(-time.Second),
			ClaimedAt: fixedNow.Add(-11 * time.Minute),
			RenewedAt: fixedNow.Add(-11 * time.Minute),
		},
		{ // fresh active with long remaining TTL — no renew yet
			Repo: "r", Provider: "p", Project: "j", TaskRef: "FAC-11",
			OwnerID: "w", Generation: 4, Active: true,
			ExpiresAt: fixedNow.Add(20 * time.Minute),
			ClaimedAt: fixedNow.Add(-time.Minute),
			RenewedAt: fixedNow.Add(-time.Minute),
		},
	}
	obs.Callbacks = nil
	snap, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionRenewLease {
			t.Fatalf("unexpected renew: %+v", a)
		}
	}
}

func TestJSONAndHumanCountsIdentical(t *testing.T) {
	snap, err := Plan(healthyObs(), Options{Act: true, Now: fixedNow, Reason: "parity"})
	if err != nil {
		t.Fatal(err)
	}
	human := FormatHuman(snap)
	raw, err := FormatJSON(snap)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Counts != snap.Counts {
		t.Fatalf("JSON counts %+v != snap counts %+v", decoded.Counts, snap.Counts)
	}
	// Human must embed the same numeric tallies.
	want := fmt.Sprintf("agents=%d healthy_idle=%d busy=%d blocked=%d done=%d stale=%d unknown=%d actions=%d renew_leases=%d consume_callbacks=%d dispatch=%d open_review=%d reap_lanes=%d would_run=%d reconcile=%d applied=%d",
		snap.Counts.Agents, snap.Counts.HealthyIdle, snap.Counts.Busy, snap.Counts.Blocked,
		snap.Counts.Done, snap.Counts.Stale, snap.Counts.Unknown, snap.Counts.Actions,
		snap.Counts.RenewLeases, snap.Counts.ConsumeCallback, snap.Counts.Dispatch,
		snap.Counts.OpenReview, snap.Counts.ReapLanes, snap.Counts.WouldRun, snap.Counts.Reconcile, snap.Counts.Applied)
	if !strings.Contains(human, want) {
		t.Fatalf("human missing counts line %q\n---\n%s", want, human)
	}
}

type recordingActor struct {
	renewed      []LeaseObservation
	consumed     []CallbackObservation
	reconcile    int
	dispatch     int
	reaped       []AgentObservation
	openedReview []AgentObservation
	// failRenewGeneration when non-zero forces RenewLease to fail for that gen.
	failRenewGeneration int64
	// failReapLane forces ReapLane to fail for any lane when true.
	failReapLane bool
	// failOpenReview forces OpenReview to fail for any lane when true.
	failOpenReview bool
	// consumeOnce tracks envelope IDs already acked (idempotent).
	acked map[string]int
}

func (a *recordingActor) Reconcile(context.Context) error {
	a.reconcile++
	return nil
}
func (a *recordingActor) RenewLease(_ context.Context, l LeaseObservation) error {
	if a.failRenewGeneration != 0 && l.Generation == a.failRenewGeneration {
		return errors.New("stale generation")
	}
	if !l.Active {
		return errors.New("not active")
	}
	a.renewed = append(a.renewed, l)
	return nil
}
func (a *recordingActor) ConsumeCallback(_ context.Context, cb CallbackObservation) error {
	if a.acked == nil {
		a.acked = map[string]int{}
	}
	a.acked[cb.EnvelopeID]++
	a.consumed = append(a.consumed, cb)
	return nil
}
func (a *recordingActor) Dispatch(context.Context, string, string) error {
	a.dispatch++
	return nil
}
func (a *recordingActor) ReapLane(_ context.Context, lane AgentObservation) error {
	if a.failReapLane {
		return errors.New("reap failed: fencing evidence incomplete")
	}
	a.reaped = append(a.reaped, lane)
	return nil
}
func (a *recordingActor) OpenReview(_ context.Context, lane AgentObservation) error {
	if a.failOpenReview {
		return errors.New("open_review failed: review supervisor not available")
	}
	a.openedReview = append(a.openedReview, lane)
	return nil
}

func TestApplyRenewsOnlyPlannedGenerationAndConsumesIdempotently(t *testing.T) {
	obs := healthyObs()
	actor := &recordingActor{}
	snap, err := Beat(context.Background(), obs, Options{Act: true, Now: fixedNow}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(actor.renewed) != 1 || actor.renewed[0].Generation != 3 {
		t.Fatalf("renewed=%+v", actor.renewed)
	}
	if len(actor.consumed) != 2 {
		t.Fatalf("consumed=%d", len(actor.consumed))
	}
	if snap.Counts.Applied != snap.Counts.RenewLeases+snap.Counts.ConsumeCallback {
		t.Fatalf("applied counts: %+v", snap.Counts)
	}

	// Restart: same plan applied again — actor acks are idempotent (count++ ok);
	// Plan against remaining callbacks only plans unacked ones.
	remaining := obs
	remaining.Callbacks = nil                            // durable consumer already acked
	remaining.Leases = []LeaseObservation{obs.Leases[0]} // still active same gen
	actor2 := &recordingActor{acked: map[string]int{"env-1": 1, "env-2": 1}}
	snap2, err := Beat(context.Background(), remaining, Options{Act: true, Now: fixedNow}, actor2)
	if err != nil {
		t.Fatal(err)
	}
	if len(actor2.consumed) != 0 {
		t.Fatalf("restart must not re-consume acked callbacks: %+v", actor2.consumed)
	}
	// Renew may still apply for the same current generation.
	if len(actor2.renewed) != 1 || actor2.renewed[0].Generation != 3 {
		t.Fatalf("restart renew gen: %+v", actor2.renewed)
	}
	if snap2.ExitCode != 0 {
		t.Fatalf("restart exit=%d", snap2.ExitCode)
	}
}

func TestApplyRefusesDispatchWhenUnknownCritical(t *testing.T) {
	obs := healthyObs()
	obs.Quota = QuotaObservation{Known: false, Error: "ledger stale"}
	// Force a dispatch action into the snapshot to prove Apply gates it
	// even if a caller mutated the plan (defense in depth).
	snap, err := Plan(obs, Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Dispatch != 0 {
		t.Fatalf("plan should not include dispatch: %+v", snap.Actions)
	}
	// Manually inject a dispatch action as a regression probe.
	snap.Actions = append(snap.Actions, Action{
		Sequence: len(snap.Actions) + 1,
		Kind:     ActionDispatch,
		Target:   "smith",
		Reason:   "probe",
		Safe:     true,
	})
	actor := &recordingActor{}
	out, err := Apply(context.Background(), snap, actor)
	if err == nil {
		t.Fatal("expected apply error for dispatch under unknown critical")
	}
	if actor.dispatch != 0 {
		t.Fatal("dispatch must not execute")
	}
	// Find the dispatch action error.
	found := false
	for _, a := range out.Actions {
		if a.Kind == ActionDispatch && a.ApplyError != "" {
			found = true
			if !strings.Contains(a.ApplyError, "unknown critical") {
				t.Fatalf("error=%q", a.ApplyError)
			}
		}
	}
	if !found {
		t.Fatalf("dispatch apply error missing: %+v", out.Actions)
	}
	if out.ExitCode == 0 {
		t.Fatal("exit must be non-zero")
	}
}

func TestApplyRenewFencedToObservationGeneration(t *testing.T) {
	obs := healthyObs()
	snap, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	actor := &recordingActor{failRenewGeneration: 3}
	out, err := Apply(context.Background(), snap, actor)
	if err == nil {
		t.Fatal("expected renew failure")
	}
	if out.ExitCode == 0 {
		t.Fatal("failed renew must set non-zero exit")
	}
	for _, a := range out.Actions {
		if a.Kind == ActionRenewLease && a.ApplyError == "" {
			t.Fatal("renew must record apply error")
		}
	}
}

func TestSpawnRequiresAct(t *testing.T) {
	_, err := Plan(healthyObs(), Options{Spawn: true, Now: fixedNow})
	if err == nil {
		t.Fatal("expected spawn-without-act error")
	}
}

func TestActSpawnPlansDispatchWhenHealthy(t *testing.T) {
	snap, err := Plan(healthyObs(), Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != ModeActSpawn {
		t.Fatalf("mode=%s", snap.Mode)
	}
	if snap.Counts.Dispatch != 1 {
		t.Fatalf("expected one dispatch, counts=%+v actions=%+v", snap.Counts, snap.Actions)
	}
	actor := &recordingActor{}
	out, err := Apply(context.Background(), snap, actor)
	if err != nil {
		t.Fatal(err)
	}
	if actor.dispatch != 1 {
		t.Fatalf("dispatch calls=%d", actor.dispatch)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit=%d", out.ExitCode)
	}
}

func TestActSpawnSkipsHeldIdleLaneAndDispatchesNextEligibleLane(t *testing.T) {
	obs := healthyObs()
	obs.Herdr.Agents = []AgentObservation{
		{Name: "forge-herd-smith", Raw: "idle"},
		{Name: "forge-platform-ops", Raw: "idle"},
	}
	obs.Leases = []LeaseObservation{{Held: true, HoldLane: "forge-herd-smith"}}

	snap, err := Plan(obs, Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Dispatch != 1 {
		t.Fatalf("expected one dispatch, counts=%+v actions=%+v", snap.Counts, snap.Actions)
	}
	for _, action := range snap.Actions {
		if action.Kind == ActionDispatch && action.Target != "forge-platform-ops" {
			t.Fatalf("dispatch target=%q want forge-platform-ops: %+v", action.Target, action)
		}
	}
}

func TestActSpawnWithAllIdleLanesHeldReportsNoEligibleTarget(t *testing.T) {
	obs := healthyObs()
	obs.Herdr.Agents = []AgentObservation{
		{Name: "forge-herd-smith", Raw: "idle"},
		{Name: "forge-platform-ops", Raw: "idle"},
	}
	obs.Leases = []LeaseObservation{
		{Held: true, HoldLane: "forge-herd-smith"},
		{Held: true, HoldLane: "forge-platform-ops"},
	}

	snap, err := Plan(obs, Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Dispatch != 0 {
		t.Fatalf("held lanes must not consume dispatch budget: %+v", snap)
	}
	found := false
	for _, action := range snap.Actions {
		if action.Kind == ActionWouldRun && strings.Contains(action.Reason, "all healthy idle lanes are held") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing no-eligible-target report: %+v", snap.Actions)
	}
}

func TestNeedsReconcilePlansReconcileAction(t *testing.T) {
	obs := healthyObs()
	obs.NeedsReconcile = true
	obs.Callbacks = nil
	obs.Leases = nil
	obs.Provider.Claimable = 0
	obs.Provider.QueueDepth = 0
	snap, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Reconcile != 1 {
		t.Fatalf("counts=%+v", snap.Counts)
	}
	actor := &recordingActor{}
	if _, err := Apply(context.Background(), snap, actor); err != nil {
		t.Fatal(err)
	}
	if actor.reconcile != 1 {
		t.Fatalf("reconcile=%d", actor.reconcile)
	}
}

func TestWindDownBlocksDispatch(t *testing.T) {
	obs := healthyObs()
	obs.WindDown.Enabled = true
	snap, err := Plan(obs, Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.DispatchBlocked || snap.Counts.Dispatch != 0 {
		t.Fatalf("wind-down must block dispatch: %+v", snap)
	}
}

func TestProviderErrorNeverZeroWork(t *testing.T) {
	obs := healthyObs()
	// Error with Known=false and zero depths — must not look like empty healthy queue.
	obs.Provider = ProviderObservation{Known: false, Error: "timeout", QueueDepth: 0, Claimable: 0}
	snap, err := Plan(obs, Options{Act: true, Spawn: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnknownCritical {
		t.Fatal("provider error must be critical")
	}
	// No "no claimable work" alone — reason must name provider.
	if !strings.Contains(snap.DispatchBlockReason, "provider") && !strings.Contains(strings.Join(snap.UnknownReasons, " "), "provider") {
		t.Fatalf("block reasons must cite provider: %q / %v", snap.DispatchBlockReason, snap.UnknownReasons)
	}
}

// Mutation guard: if someone "fixes" unknown critical by clearing ExitCode,
// this fails. The acceptance criterion is non-zero action result.
func TestUnknownCriticalExitCodeIsNonVacuous(t *testing.T) {
	obs := healthyObs()
	obs.Herdr.Known = false
	obs.Herdr.Error = "boom"
	snap, err := Plan(obs, Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.ExitCode == 0 {
		t.Fatal("vacuous: unknown critical produced exit 0")
	}
	// Flip Known on and prove exit clears — ensures the test can fail both ways.
	obs.Herdr.Known = true
	obs.Herdr.Error = ""
	ok, err := Plan(obs, Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if ok.ExitCode != 0 {
		t.Fatalf("healthy beat exit=%d", ok.ExitCode)
	}
}

// --- FAC-221: lane reap enforcement tests ---

// reapObs builds an observation with reap-eligible and keep lanes.
func reapObs() Observation {
	return Observation{
		Provider: ProviderObservation{Known: true, QueueDepth: 0, Claimable: 0},
		Herdr: HerdrObservation{
			Known: true,
			Agents: []AgentObservation{
				{Name: "idle-committed", Raw: "idle", CommittedWork: true, TabID: "wK:t1", Workspace: "wK"},
				{Name: "done-ticket", Raw: "done", TicketDone: true, TabID: "wK:t2", Workspace: "wK"},
				{Name: "idle-saferef", Raw: "idle", SafeRef: "safe/fac-201", TabID: "wK:t3", Workspace: "wK"},
				{Name: "idle-awaiting-verdict", Raw: "idle", CommittedWork: true, AwaitingVerdict: true, TabID: "wK:t4", Workspace: "wK"},
				{Name: "busy-worker", Raw: "working", CommittedWork: true, TabID: "wK:t5", Workspace: "wK"},
				{Name: "idle-no-evidence", Raw: "idle", TabID: "wK:t6", Workspace: "wK"},
				{Name: "blocked-lane", Raw: "blocked", CommittedWork: true, TabID: "wK:t7", Workspace: "wK"},
			},
		},
		Review:   ReviewObservation{Known: true},
		Quota:    QuotaObservation{Known: true},
		WindDown: WindDownObservation{Known: true, Enabled: false},
	}
}

func TestPlanReapsIdleLanesWithExitEvidence(t *testing.T) {
	snap, err := Plan(reapObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// FAC-226: idle-committed is now FINISHED — it gets OpenReview, not Reap.
	// Only done-ticket and idle-saferef are reaped.
	if snap.Counts.ReapLanes != 2 {
		t.Fatalf("expected 2 reap actions (done-ticket, idle-saferef), got %d: %+v", snap.Counts.ReapLanes, snap.Actions)
	}
	if snap.Counts.OpenReview != 1 {
		t.Fatalf("expected 1 open_review action (idle-committed), got %d: %+v", snap.Counts.OpenReview, snap.Actions)
	}
	for _, a := range snap.Actions {
		if a.Kind != ActionReapLane {
			continue
		}
		switch a.Target {
		case "wK:t2", "wK:t3":
			if !a.Safe {
				t.Fatalf("reap action %s must be Safe under --act: %+v", a.Target, a)
			}
		default:
			t.Fatalf("unexpected reap target %q: %+v", a.Target, a)
		}
	}
}

func TestPlanDoesNotReapBusyBlockedOrNoEvidence(t *testing.T) {
	snap, err := Plan(reapObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if a.Kind != ActionReapLane {
			continue
		}
		switch a.Target {
		case "wK:t5", "wK:t6", "wK:t7":
			t.Fatalf("must not reap busy/no-evidence/blocked lane: %+v", a)
		}
	}
}

func TestPlanKeepsIdleLaneAwaitingVerdict(t *testing.T) {
	snap, err := Plan(reapObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionReapLane && a.Target == "wK:t4" {
			t.Fatalf("must not reap lane awaiting verdict it must act on: %+v", a)
		}
	}
}

func TestPlanObserveReapIsWouldRunNotSafe(t *testing.T) {
	snap, err := Plan(reapObs(), Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != ModeObserve {
		t.Fatalf("mode=%s", snap.Mode)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionWouldRun && strings.Contains(a.WouldRun, "reap_lane") {
			if a.Safe {
				t.Fatalf("observe reap must not be Safe: %+v", a)
			}
		}
		if a.Kind == ActionReapLane {
			t.Fatalf("observe must not plan concrete reap: %+v", a)
		}
	}
}

func TestApplyReapsEligibleLanes(t *testing.T) {
	obs := reapObs()
	actor := &recordingActor{}
	snap, err := Beat(context.Background(), obs, Options{Act: true, Now: fixedNow}, actor)
	if err != nil {
		t.Fatalf("beat: %v", err)
	}
	// FAC-226: idle-committed gets OpenReview, not Reap. Only done-ticket
	// and idle-saferef are reaped.
	if len(actor.reaped) != 2 {
		t.Fatalf("expected 2 reaped lanes, got %d: %+v", len(actor.reaped), actor.reaped)
	}
	for _, lane := range actor.reaped {
		switch lane.TabID {
		case "wK:t2", "wK:t3":
		default:
			t.Fatalf("unexpected reaped lane: %+v", lane)
		}
	}
	if len(actor.openedReview) != 1 {
		t.Fatalf("expected 1 open_review lane, got %d: %+v", len(actor.openedReview), actor.openedReview)
	}
	if actor.openedReview[0].TabID != "wK:t1" {
		t.Fatalf("open_review target should be idle-committed wK:t1, got %+v", actor.openedReview[0])
	}
	if snap.ExitCode != 0 {
		t.Fatalf("successful reap beat must exit 0, got %d", snap.ExitCode)
	}
	if snap.Counts.Applied < 3 {
		t.Fatalf("expected at least 3 applied actions (1 open_review + 2 reaps), got %d: %+v", snap.Counts.Applied, snap.Counts)
	}
}

func TestApplyReapFailureIsHardError(t *testing.T) {
	obs := reapObs()
	actor := &recordingActor{failReapLane: true}
	snap, err := Beat(context.Background(), obs, Options{Act: true, Now: fixedNow}, actor)
	if err == nil {
		t.Fatal("expected hard error from reap failure")
	}
	if len(actor.reaped) != 0 {
		t.Fatalf("no lanes should be reaped on failure, got %d", len(actor.reaped))
	}
	if snap.ExitCode == 0 {
		t.Fatal("reap failure must set non-zero exit")
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionReapLane && a.ApplyError == "" {
			t.Fatalf("reap action must record apply error: %+v", a)
		}
	}
}

func TestApplyObserveDoesNotReap(t *testing.T) {
	obs := reapObs()
	actor := &recordingActor{}
	snap, err := Beat(context.Background(), obs, Options{Now: fixedNow}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(actor.reaped) != 0 {
		t.Fatalf("observe must not reap any lanes, got %d", len(actor.reaped))
	}
	if snap.ExitCode != 0 {
		t.Fatalf("observe exit=%d", snap.ExitCode)
	}
}

// Mutation guard: if someone silences the reap for awaiting-verdict by
// clearing AwaitingVerdict, this test proves an action fires — ensuring the
// KEEP distinction is non-vacuous. FAC-226: a lane with CommittedWork and no
// SafeRef gets OpenReview (not Reap) when AwaitingVerdict is cleared.
func TestReapAwaitingVerdictFlipIsNonVacuous(t *testing.T) {
	obs := reapObs()
	snap, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if (a.Kind == ActionReapLane || a.Kind == ActionOpenReview) && a.Target == "wK:t4" {
			t.Fatalf("awaiting-verdict lane must not get reap or open_review: %+v", a)
		}
	}
	obs.Herdr.Agents[3].AwaitingVerdict = false
	snap2, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range snap2.Actions {
		// wK:t4 has CommittedWork=true, no SafeRef, not TicketDone → OpenReview.
		if a.Kind == ActionOpenReview && a.Target == "wK:t4" {
			found = true
		}
	}
	if !found {
		t.Fatal("clearing AwaitingVerdict must produce an open_review — KEEP distinction is non-vacuous")
	}
}

// --- FAC-226: finished-lane open-review tests ---

// finishedObs builds an observation focused on finished-lane detection.
func finishedObs() Observation {
	return Observation{
		Provider: ProviderObservation{Known: true, QueueDepth: 0, Claimable: 0},
		Herdr: HerdrObservation{
			Known: true,
			Agents: []AgentObservation{
				// FINISHED: idle + committed work, no SafeRef, not TicketDone.
				{Name: "finished-lane", Raw: "idle", CommittedWork: true, TabID: "wK:t10", Workspace: "wK"},
				// Already out for review: should be reaped, not open-reviewed.
				{Name: "review-pending", Raw: "idle", CommittedWork: true, SafeRef: "safe/fac-200", TabID: "wK:t11", Workspace: "wK"},
				// Ticket done: should be reaped, not open-reviewed.
				{Name: "landed-lane", Raw: "done", TicketDone: true, TabID: "wK:t12", Workspace: "wK"},
				// Awaiting verdict: KEPT — no action.
				{Name: "verdict-pending", Raw: "idle", CommittedWork: true, AwaitingVerdict: true, TabID: "wK:t13", Workspace: "wK"},
				// Idle but no committed work: no action.
				{Name: "idle-empty", Raw: "idle", TabID: "wK:t14", Workspace: "wK"},
				// Busy: no action.
				{Name: "busy-lane", Raw: "working", CommittedWork: true, TabID: "wK:t15", Workspace: "wK"},
			},
		},
		Review:   ReviewObservation{Known: true},
		Quota:    QuotaObservation{Known: true},
		WindDown: WindDownObservation{Known: true, Enabled: false},
	}
}

func TestPlanOpenReviewForFinishedLanes(t *testing.T) {
	snap, err := Plan(finishedObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.OpenReview != 1 {
		t.Fatalf("expected 1 open_review (finished-lane), got %d: %+v", snap.Counts.OpenReview, snap.Actions)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionOpenReview {
			if a.Target != "wK:t10" {
				t.Fatalf("open_review target should be wK:t10, got %s: %+v", a.Target, a)
			}
			if !a.Safe {
				t.Fatalf("open_review must be Safe under --act: %+v", a)
			}
		}
	}
}

func TestPlanDoesNotOpenReviewForSafeRefOrTicketDone(t *testing.T) {
	snap, err := Plan(finishedObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionOpenReview {
			switch a.Target {
			case "wK:t11", "wK:t12":
				t.Fatalf("must not open_review for SafeRef or TicketDone lane: %+v", a)
			}
		}
	}
	// review-pending and landed-lane should be reaped instead.
	if snap.Counts.ReapLanes != 2 {
		t.Fatalf("expected 2 reaps (review-pending, landed-lane), got %d: %+v", snap.Counts.ReapLanes, snap.Actions)
	}
}

func TestPlanDoesNotOpenReviewForAwaitingVerdictOrBusyOrEmpty(t *testing.T) {
	snap, err := Plan(finishedObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionOpenReview {
			switch a.Target {
			case "wK:t13", "wK:t14", "wK:t15":
				t.Fatalf("must not open_review for awaiting-verdict/idle-empty/busy lane: %+v", a)
			}
		}
		if a.Kind == ActionReapLane {
			switch a.Target {
			case "wK:t13", "wK:t14", "wK:t15":
				t.Fatalf("must not reap awaiting-verdict/idle-empty/busy lane: %+v", a)
			}
		}
	}
}

func TestPlanKeepsPacketPendingIdleLane(t *testing.T) {
	obs := healthyObs()
	obs.Herdr.Agents = []AgentObservation{{
		Name: "packet-pending", Raw: "idle", Status: StatusHealthyIdle,
		CommittedWork: true, PacketPending: true, TicketDone: true,
		TabID: "tab-packet", Workspace: "w", PaneID: "pane-packet",
	}}
	plan, err := Plan(obs, Options{Act: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.Actions {
		if action.Target == "tab-packet" && (action.Kind == ActionReapLane || action.Kind == ActionOpenReview) {
			t.Fatalf("packet-pending lane was acted on: %+v", action)
		}
	}
	obs.Herdr.Agents[0].PacketPending = false
	plan, err = Plan(obs, Options{Act: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, action := range plan.Actions {
		if action.Target == "tab-packet" && action.Kind == ActionReapLane {
			found = true
		}
	}
	if !found {
		t.Fatal("clearing PacketPending must make the done lane reap-eligible")
	}
}

func TestPlanObserveOpenReviewIsWouldRunNotSafe(t *testing.T) {
	snap, err := Plan(finishedObs(), Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != ModeObserve {
		t.Fatalf("mode=%s", snap.Mode)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionWouldRun && strings.Contains(a.WouldRun, "open_review") {
			if a.Safe {
				t.Fatalf("observe open_review must not be Safe: %+v", a)
			}
		}
		if a.Kind == ActionOpenReview {
			t.Fatalf("observe must not plan concrete open_review: %+v", a)
		}
	}
}

func TestApplyOpenReviewCallsActor(t *testing.T) {
	obs := finishedObs()
	actor := &recordingActor{}
	snap, err := Beat(context.Background(), obs, Options{Act: true, Now: fixedNow}, actor)
	if err != nil {
		t.Fatalf("beat: %v", err)
	}
	if len(actor.openedReview) != 1 {
		t.Fatalf("expected 1 open_review call, got %d: %+v", len(actor.openedReview), actor.openedReview)
	}
	if actor.openedReview[0].TabID != "wK:t10" {
		t.Fatalf("open_review target should be wK:t10, got %+v", actor.openedReview[0])
	}
	if snap.ExitCode != 0 {
		t.Fatalf("successful beat must exit 0, got %d", snap.ExitCode)
	}
}

func TestApplyOpenReviewFailureIsHardError(t *testing.T) {
	obs := finishedObs()
	actor := &recordingActor{failOpenReview: true}
	snap, err := Beat(context.Background(), obs, Options{Act: true, Now: fixedNow}, actor)
	if err == nil {
		t.Fatal("expected hard error from open_review failure")
	}
	if len(actor.openedReview) != 0 {
		t.Fatalf("no lanes should be opened on failure, got %d", len(actor.openedReview))
	}
	if snap.ExitCode == 0 {
		t.Fatal("open_review failure must set non-zero exit")
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionOpenReview && a.ApplyError == "" {
			t.Fatalf("open_review action must record apply error: %+v", a)
		}
	}
}

// Mutation guard: if someone removes the CommittedWork check from OpenReview,
// this test proves the open_review fires only when CommittedWork is true —
// ensuring the detection is non-vacuous.
func TestOpenReviewCommittedWorkFlipIsNonVacuous(t *testing.T) {
	obs := finishedObs()
	snap, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.OpenReview != 1 {
		t.Fatalf("expected 1 open_review, got %d", snap.Counts.OpenReview)
	}
	// Clear CommittedWork — open_review must disappear for that lane.
	obs.Herdr.Agents[0].CommittedWork = false
	snap2, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap2.Actions {
		if a.Kind == ActionOpenReview && a.Target == "wK:t10" {
			t.Fatal("clearing CommittedWork must remove open_review — detection is non-vacuous")
		}
	}
}

// Mutation guard: if someone adds a SafeRef to the finished lane, the
// open_review must turn into a reap — proving the SafeRef distinction is
// non-vacuous.
func TestOpenReviewSafeRefFlipIsNonVacuous(t *testing.T) {
	obs := finishedObs()
	snap, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range snap.Actions {
		if a.Kind == ActionOpenReview && a.Target == "wK:t10" {
			found = true
		}
	}
	if !found {
		t.Fatal("baseline: finished-lane must get open_review")
	}
	// Add SafeRef — open_review must become reap.
	obs.Herdr.Agents[0].SafeRef = "safe/fac-226"
	snap2, err := Plan(obs, Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap2.Actions {
		if a.Kind == ActionOpenReview && a.Target == "wK:t10" {
			t.Fatal("adding SafeRef must remove open_review — lane is already out for review")
		}
	}
	reaped := false
	for _, a := range snap2.Actions {
		if a.Kind == ActionReapLane && a.Target == "wK:t10" {
			reaped = true
		}
	}
	if !reaped {
		t.Fatal("adding SafeRef must produce a reap — SafeRef distinction is non-vacuous")
	}
}
