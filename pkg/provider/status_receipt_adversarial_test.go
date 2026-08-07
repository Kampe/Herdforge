package provider

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestStatusReceipt_HMACBindsExactFields: forged receipt with wrong op/task/
// status/fence fails Verify; only exact binding with valid MAC passes.
func TestStatusReceipt_HMACBindsExactFields(t *testing.T) {
	rec, err := MintStatusReceipt("task-1", "op-exact", StatusDone, "base-rev", "actor-a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyStatusReceipt(rec, "task-1", "op-exact", StatusDone, 7); err != nil {
		t.Fatalf("valid receipt: %v", err)
	}
	// Wrong op
	if err := VerifyStatusReceipt(rec, "task-1", "op-other", StatusDone, 7); err == nil {
		t.Fatal("wrong op must fail")
	}
	// Wrong task
	if err := VerifyStatusReceipt(rec, "task-9", "op-exact", StatusDone, 7); err == nil {
		t.Fatal("wrong task must fail")
	}
	// Wrong status
	if err := VerifyStatusReceipt(rec, "task-1", "op-exact", StatusInProgress, 7); err == nil {
		t.Fatal("wrong status must fail")
	}
	// Wrong fence
	if err := VerifyStatusReceipt(rec, "task-1", "op-exact", StatusDone, 8); err == nil {
		t.Fatal("wrong fence must fail")
	}
	// Tamper MAC
	bad := *rec
	bad.MAC = strings.Repeat("00", 32)
	if err := VerifyStatusReceipt(&bad, "task-1", "op-exact", StatusDone, 7); err == nil {
		t.Fatal("tampered MAC must fail")
	}
	// Field swap without re-MAC
	swap := *rec
	swap.OpID = "op-forged"
	if err := VerifyStatusReceipt(&swap, "task-1", "op-forged", StatusDone, 7); err == nil {
		t.Fatal("field swap without re-MAC must fail")
	}
	// Predictable substring body is never evidence
	if MatchStatusOpEvidence("[herd-status-op:op-exact:done]", "op-exact", StatusDone) {
		t.Fatal("substring tags must never match")
	}
	if StatusOpEvidenceBody("op-exact", StatusDone) != "" {
		t.Fatal("StatusOpEvidenceBody must be empty (no dual comment writes)")
	}
}

// TestStatusReceipt_ForgedEvidenceCannotSettleEmptyRev: attacker plants a
// comment tag and/or bare status without a valid MAC receipt — empty-rev
// Present recovery must refuse.
func TestStatusReceipt_ForgedEvidenceCannotSettleEmptyRev(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "fe1", Ref: "FAC-FE", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})

	broker := NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	// No ServerOpDedupe: empty-rev Present without real evidence must refuse
	// (not re-mutate). Client HMAC is not production evidence (con62fkm).
	broker.ServerOpDedupe = false
	broker.StatusOpEvidence = func(ctx context.Context, taskID, opID, expStatus string) (bool, error) {
		return false, nil // never trust client-forged receipt bodies
	}
	if _, err := store.Advance(ctx, "fe1", 1); err != nil {
		t.Fatal(err)
	}
	base, _ := broker.RevisionOf(ctx, "fe1")
	if err := store.MarkAmbiguous(ctx, OpReceipt{
		OpID: "op-forged-evi", TaskID: "fe1", FenceToken: 1,
		ExpectedStatus: StatusDone, BaseRevision: base, Ambiguous: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Direct client: bare status + forgeable comment tag, no signed receipt.
	if err := mp.UpdateStatus(ctx, "fe1", StatusDone); err != nil {
		t.Fatal(err)
	}
	_ = mp.AddComment(ctx, "fe1", "[herd-status-op:op-forged-evi:done]")
	// Plant a forged receipt JSON with bad MAC.
	forged := StatusMutationReceipt{
		OpID: "op-forged-evi", TaskID: "fe1", Status: StatusDone,
		FenceToken: 1, BaseRevision: base, Actor: "attacker",
		Nonce: "deadbeef", IssuedAtUnix: time.Now().Unix(),
		MAC: hex.EncodeToString(make([]byte, 32)),
	}
	raw, _ := json.Marshal(forged)
	if err := mp.UpdateStatusAtomic(ctx, "fe1", StatusDone, string(raw)); err != nil {
		t.Fatal(err)
	}

	err := broker.Execute(ctx, "fe1", 1, "op-forged-evi", StatusDone, "",
		func(ctx context.Context) error {
			t.Fatal("must not re-mutate when forged evidence refused")
			return nil
		},
		func(ctx context.Context) (EffectState, error) {
			// Naive Present on status alone — broker still requires StatusOpEvidence.
			return EffectPresent, nil
		},
	)
	if err == nil {
		t.Fatal("forged receipt must not settle empty-rev Present")
	}
	if !errors.Is(err, claim.ErrProviderAmbiguous) && !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("want ambiguous/evidence refuse, got %v", err)
	}
	rec, _ := store.LookupApplied(ctx, "op-forged-evi")
	if rec != nil && !rec.Ambiguous {
		t.Fatal("must not MarkApplied on forged evidence")
	}
}

// TestStatusReceipt_AtomicCrashAfterRemote_RecoveryOnce: CrashAt(after-remote)
// fires after the single atomic status+receipt write and before local
// revision persist; restart settles via verified receipt without re-mutate.
func TestStatusReceipt_AtomicCrashAfterRemote_RecoveryOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "ac1", Ref: "FAC-AC", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})

	var muts int
	broker := NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	// Unit test of optional evidence probe only (not production Kaneo path).
	broker.ServerOpDedupe = false
	broker.StatusOpEvidence = func(ctx context.Context, taskID, opID, expStatus string) (bool, error) {
		tk, err := mp.GetTask(ctx, taskID)
		if err != nil {
			return false, err
		}
		return TaskHasVerifiedStatusReceipt(tk, taskID, opID, expStatus, 0), nil
	}
	crashed := false
	broker.CrashAt = func(phase string) {
		if phase == "after-remote" && !crashed {
			crashed = true
			panic("sim-crash-after-atomic-remote")
		}
	}
	if _, err := store.Advance(ctx, "ac1", 1); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = broker.Execute(ctx, "ac1", 1, "op-atomic", StatusDone, "",
			func(ctx context.Context) error {
				muts++
				// ONE write: status + receipt together (atomic effect).
				base, _ := broker.RevisionOf(ctx, "ac1")
				rec, err := MintStatusReceipt("ac1", "op-atomic", StatusDone, base, "worker", 1)
				if err != nil {
					return err
				}
				js, _ := EncodeStatusReceiptJSON(rec)
				return mp.UpdateStatusAtomic(ctx, "ac1", StatusDone, js)
			},
			func(ctx context.Context) (EffectState, error) { return EffectAbsent, nil },
		)
	}()
	if muts != 1 {
		t.Fatalf("remote must run once before crash, muts=%d", muts)
	}
	if !crashed {
		t.Fatal("CrashAt(after-remote) must fire after atomic write")
	}
	got, _ := mp.GetTask(ctx, "ac1")
	if !TaskHasVerifiedStatusReceipt(got, "ac1", "op-atomic", StatusDone, 1) {
		t.Fatalf("atomic receipt must be durable on board after crash: %+v", got)
	}
	rec, _ := store.LookupApplied(ctx, "op-atomic")
	if rec == nil || !rec.Ambiguous || rec.Revision != "" {
		t.Fatalf("want empty-rev ambiguous in_progress, got %+v", rec)
	}

	// Restart recovery: Present + verified receipt → MarkApplied, no re-mutate.
	broker.CrashAt = nil
	err := broker.Execute(ctx, "ac1", 1, "op-atomic", StatusDone, "",
		func(ctx context.Context) error {
			muts++
			return errors.New("must not re-mutate")
		},
		func(ctx context.Context) (EffectState, error) {
			tk, err := mp.GetTask(ctx, "ac1")
			if err != nil {
				return EffectUnknown, err
			}
			if TaskHasVerifiedStatusReceipt(tk, "ac1", "op-atomic", StatusDone, 1) {
				return EffectPresent, nil
			}
			return EffectAbsent, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if muts != 1 {
		t.Fatalf("recovery must not re-mutate, muts=%d", muts)
	}
	rec, _ = store.LookupApplied(ctx, "op-atomic")
	if rec == nil || rec.Ambiguous || rec.Revision == "" {
		t.Fatalf("must be applied with live rev: %+v", rec)
	}
}

// TestAtomicFenceServer_StatusPUT_NoClientReceipt: enforcing sandbox accepts
// fence+op status PUT; stock path stays fail-closed; no client HMAC required.
func TestAtomicFenceServer_StatusPUT_NoClientReceipt(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{ID: "hp1", Ref: "FAC-HP", Title: "t", Status: "to-do", ProjectID: "proj",
		Priority: PriorityMedium, Position: 1, UpdatedAt: now, CreatedAt: now})
	srv := board.serve()
	defer srv.Close()

	// Stock: fail closed.
	stock := NewKaneoProvider(srv.URL, "proj", false)
	store := NewMemoryFenceStore()
	stock.Receiver = NewAuthBroker(store).BindRevisionReader(stock.GetTask)
	stock.RequireCASMeta = true
	ctx := WithCASMeta(context.Background(), 1, "op-stock")
	if err := stock.updateStatusOnce(ctx, "hp1", StatusInProgress); err == nil {
		t.Fatal("stock fenced status must fail closed (audit con62fkm)")
	}

	kp := NewKaneoProvider(srv.URL, "proj", false)
	enableAtomicFence(kp)
	broker := NewAuthBroker(store).BindRevisionReader(kp.GetTask)
	broker.ServerOpDedupe = true
	kp.Receiver = broker
	kp.RequireCASMeta = true
	ctx = WithCASMeta(context.Background(), 1, "op-http-atomic")
	ctx = WithCASExpectation(ctx, StatusInProgress, "")
	if err := kp.updateStatusOnce(ctx, "hp1", StatusInProgress); err != nil {
		t.Fatalf("atomic update: %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("effects=%d want 1", board.EffectCount())
	}
	if len(board.Comments("hp1")) != 0 {
		t.Fatalf("must not dual-write comments: %v", board.Comments("hp1"))
	}
	// Direct bypass without fence headers refused by board.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/task/hp1",
		strings.NewReader(`{"title":"t","description":"x","status":"done","priority":"medium","projectId":"proj","position":1}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("direct client without fence/op must be refused")
	}
}

// TestSharedMarker_V5SealIsDBAuthorityNotWritableSHARED: planting a SHARED
// pointer with stolen volume_id against an independent fences.db fails;
// only the provisioned store_authority.volume_seal row validates.
func TestSharedMarker_V5SealIsDBAuthorityNotWritableSHARED(t *testing.T) {
	dirA := t.TempDir()
	ProvisionSharedFenceForTest(t, dirA)
	volID := strings.TrimSpace(os.Getenv("HERD_FENCE_VOLUME_ID"))

	dirB := t.TempDir()
	pathB := filepath.Join(dirB, "fences.db")
	storeB, err := NewSQLiteFenceStore(pathB)
	if err != nil {
		t.Fatal(err)
	}
	_ = storeB.Close()
	absB, err := filepath.Abs(dirB)
	if err != nil {
		t.Fatal(err)
	}
	body := "herd-shared-fence-v5\nversion=5\nvolume_id=" + volID +
		"\nclaim_dir=" + absB + "\nauthority=store_authority.volume_seal\n"
	if err := os.WriteFile(filepath.Join(dirB, "SHARED"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CLAIM_DIR", absB)
	t.Setenv("HERD_FENCE_VOLUME_ID", volID)
	if err := ValidateSharedMarker(dirB); err == nil {
		t.Fatal("independent fences.db without matching volume_seal must fail")
	}
	absA, err := filepath.Abs(dirA)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CLAIM_DIR", absA)
	if err := ValidateSharedMarker(dirA); err != nil {
		t.Fatalf("provisioned fleet store: %v", err)
	}
}
