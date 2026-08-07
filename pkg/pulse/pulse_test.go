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
	want := fmt.Sprintf("agents=%d healthy_idle=%d busy=%d blocked=%d done=%d stale=%d unknown=%d actions=%d renew_leases=%d consume_callbacks=%d dispatch=%d would_run=%d reconcile=%d applied=%d",
		snap.Counts.Agents, snap.Counts.HealthyIdle, snap.Counts.Busy, snap.Counts.Blocked,
		snap.Counts.Done, snap.Counts.Stale, snap.Counts.Unknown, snap.Counts.Actions,
		snap.Counts.RenewLeases, snap.Counts.ConsumeCallback, snap.Counts.Dispatch,
		snap.Counts.WouldRun, snap.Counts.Reconcile, snap.Counts.Applied)
	if !strings.Contains(human, want) {
		t.Fatalf("human missing counts line %q\n---\n%s", want, human)
	}
}

type recordingActor struct {
	renewed   []LeaseObservation
	consumed  []CallbackObservation
	reconcile int
	dispatch  int
	// failRenewGeneration when non-zero forces RenewLease to fail for that gen.
	failRenewGeneration int64
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
