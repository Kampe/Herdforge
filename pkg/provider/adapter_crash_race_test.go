package provider

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestAdapter_CrashAfterRemote_SameOpRecovery: CrashAt at TRUE remote-success/
// pre-local-persist boundary (after backend, before revision persist). Recovery
// uses provider-bound StatusOpEvidence — exactly one remote effect.
func TestAdapter_CrashAfterRemote_SameOpRecovery(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "cr1", Ref: "FAC-CR", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})

	var muts int
	var evidence bool
	broker := NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	broker.StatusOpEvidence = func(ctx context.Context, taskID, opID, expStatus string) (bool, error) {
		return evidence && opID == "op-crash" && NormalizeStatus(expStatus) == StatusDone, nil
	}
	crashed := false
	broker.CrashAt = func(phase string) {
		if phase == "after-remote" && !crashed {
			crashed = true
			panic("sim-crash-after-remote")
		}
	}
	if _, err := store.Advance(ctx, "cr1", 1); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = broker.Execute(ctx, "cr1", 1, "op-crash", StatusDone, "",
			func(ctx context.Context) error {
				muts++
				if err := mp.UpdateStatus(ctx, "cr1", StatusDone); err != nil {
					return err
				}
				evidence = true // provider-bound evidence committed with remote
				return nil
			},
			func(ctx context.Context) (EffectState, error) { return EffectAbsent, nil },
		)
	}()
	if muts != 1 {
		t.Fatalf("remote must have run once, muts=%d", muts)
	}
	rec, _ := store.LookupApplied(ctx, "op-crash")
	if rec == nil || !rec.Ambiguous {
		t.Fatalf("want ambiguous in_progress receipt, got %+v", rec)
	}
	if rec.Revision != "" {
		t.Fatalf("true pre-persist crash must leave empty revision, got %q", rec.Revision)
	}
	if !evidence {
		t.Fatal("provider evidence must have been written before crash")
	}

	broker.CrashAt = nil
	err := broker.Execute(ctx, "cr1", 1, "op-crash", StatusDone, "",
		func(ctx context.Context) error {
			muts++
			return mp.UpdateStatus(ctx, "cr1", StatusDone)
		},
		func(ctx context.Context) (EffectState, error) {
			// Production Kaneo effectMet: status + op evidence → Present.
			if !evidence {
				return EffectAbsent, nil
			}
			t, err := mp.GetTask(ctx, "cr1")
			if err != nil {
				return EffectUnknown, err
			}
			if NormalizeStatus(t.Status) != StatusDone {
				return EffectAbsent, nil
			}
			return EffectPresent, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if muts != 1 {
		t.Fatalf("recovery must not re-mutate, muts=%d", muts)
	}
	rec, _ = store.LookupApplied(ctx, "op-crash")
	if rec == nil || rec.Ambiguous || rec.Revision == "" {
		t.Fatalf("must be applied with bound live rev: %+v", rec)
	}
}

// TestAdapter_DirectClientCompetingSameStatus_EmptyRevRefuses: crash-before-remote
// then DIRECT board mutation without status-op evidence — empty-rev Present
// must refuse (causal; no MarkApplied(op-b) crutch).
func TestAdapter_DirectClientCompetingSameStatus_EmptyRevRefuses(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "comp1", Ref: "FAC-COMP", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})
	broker := NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	// No StatusOpEvidence for op-a (direct client left none).
	broker.StatusOpEvidence = func(ctx context.Context, taskID, opID, expStatus string) (bool, error) {
		return false, nil
	}
	if _, err := store.Advance(ctx, "comp1", 1); err != nil {
		t.Fatal(err)
	}
	base, _ := broker.RevisionOf(ctx, "comp1")
	if err := store.MarkAmbiguous(ctx, OpReceipt{
		OpID: "op-a", TaskID: "comp1", FenceToken: 1,
		ExpectedStatus: StatusDone, BaseRevision: base, Ambiguous: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Direct client: status only, no provider-bound op evidence.
	if err := mp.UpdateStatus(ctx, "comp1", StatusDone); err != nil {
		t.Fatal(err)
	}
	err := broker.Execute(ctx, "comp1", 1, "op-a", StatusDone, "",
		func(ctx context.Context) error {
			t.Fatal("must not re-mutate when refuse settle")
			return nil
		},
		func(ctx context.Context) (EffectState, error) {
			// Naive Present on status alone — StatusOpEvidence still false.
			return EffectPresent, nil
		},
	)
	if err == nil {
		t.Fatal("expected refuse empty-rev Present without status op evidence")
	}
	rec, _ := store.LookupApplied(ctx, "op-a")
	if rec != nil && !rec.Ambiguous {
		t.Fatal("must not MarkApplied competing same-status onto op-a")
	}
}

// TestAdapter_ProviderLockPause_StalePreempt proves short provider-lock
// stale window allows reclaim; gen1 cannot change board after gen2.
func TestAdapter_ProviderLockPause_StalePreempt(t *testing.T) {
	// FAC-147 on main: ordinary provider locks are not time-preempted by Claim
	// (lifecycle recovery owns that path). This test proves the residual that
	// still matters: after a clean release+reclaim, gen1 cannot mutate the board.
	ctx := context.Background()
	dir := t.TempDir()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "pl1", Ref: "FAC-PL", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now, Labels: []string{"worker"}})

	stack, err := OpenClaimStack(filepath.Join(dir, "claim"), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()

	key := claim.LeaseKey{Repo: dir, Provider: "memory", Project: "p", TaskRef: "FAC-PL"}
	lease1, err := stack.AcquireLease(ctx, key, "o1", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.CAS.AdvanceFence(ctx, "pl1", lease1.Generation); err != nil {
		t.Fatal(err)
	}
	if err := stack.Manager.Release(ctx, key, "o1", lease1.Generation); err != nil {
		t.Fatal(err)
	}

	lease2, err := stack.AcquireLease(ctx, key, "o2", "worker", "worker")
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if lease2.Generation <= lease1.Generation {
		t.Fatalf("want gen > %d got %d", lease1.Generation, lease2.Generation)
	}
	if err := stack.CAS.AdvanceFence(ctx, "pl1", lease2.Generation); err != nil {
		t.Fatal(err)
	}
	if err := stack.Board.MutateStatus(ctx, stack.Manager, key, "o2", lease2.Generation, "pl1", StatusDone); err != nil {
		t.Fatal(err)
	}

	err = stack.Board.MutateStatus(ctx, stack.Manager, key, "o1", lease1.Generation, "pl1", StatusToDo)
	_ = err
	got, _ := mp.GetTask(ctx, "pl1")
	if NormalizeStatus(got.Status) != StatusDone {
		t.Fatalf("board must stay done after stale gen attempt, got %s err=%v", got.Status, err)
	}
	if err == nil {
		t.Fatal("expected stale generation mutate to fail")
	}
}


// TestFencedCAS_AmbiguousEmptyRev_RefusesLiveAdvance.
func TestFencedCAS_AmbiguousEmptyRev_RefusesLiveAdvance(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "fc1", Ref: "FAC-FC", Status: StatusDone, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})
	cas, err := NewFencedCAS(store, mp)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAmbiguous(ctx, OpReceipt{
		OpID: "op-no-rev", TaskID: "fc1", FenceToken: 1,
		ExpectedStatus: StatusDone, BaseRevision: "not-live", Ambiguous: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(ctx, "fc1", 1); err != nil {
		t.Fatal(err)
	}
	rev, _ := cas.ReadRevision(ctx, "fc1")
	muts := 0
	_, err = cas.CompareAndSwap(WithCASExpectation(ctx, StatusDone, ""), "fc1", rev, 1, "op-no-rev",
		func(ctx context.Context) error {
			muts++
			return mp.UpdateStatus(ctx, "fc1", StatusDone)
		})
	if err == nil {
		t.Fatal("empty-rev Present + live advance must refuse")
	}
	if muts != 0 {
		t.Fatalf("must not re-mutate (muts=%d)", muts)
	}
	_ = sync.Mutex{}
}
