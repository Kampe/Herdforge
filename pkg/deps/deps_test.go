package deps

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func seedBoard(t *testing.T) *MemoryStore {
	t.Helper()
	m := NewMemoryStore()
	for _, ref := range []string{"FAC-75", "FAC-90", "FAC-93", "FAC-105", "FAC-136", "FAC-69", "FAC-101", "FAC-119", "FAC-73", "FAC-117"} {
		st := "to-do"
		if ref == "FAC-119" || ref == "FAC-136" || ref == "FAC-117" {
			st = "done"
		}
		m.EnsureTask(ref, st, provider.PriorityHigh)
	}
	return m
}

func prov(ref string, edges ...DependencyEdge) *Provenance {
	return &Provenance{
		Version: SchemaVersion,
		TaskRef: Ref(ref),
		TaskID:  TaskID("id-" + strings.ToLower(ref)),
		Edges:   edges,
		Present: true,
	}
}

func TestProviderStore_ConcurrentSnapshotAndNewTask(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "a", Ref: "FAC-1", Status: "to-do", ProjectID: "p"})
	store := NewProviderStore(mp, "p")
	done := make(chan error, 8)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := store.SnapshotGraph(context.Background())
			done <- err
		}()
	}
	// Concurrent add
	go func() {
		mp.AddTask(&provider.Task{ID: "b", Ref: "FAC-2", Status: "to-do", ProjectID: "p"})
		done <- nil
	}()
	for i := 0; i < 5; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// Fresh snapshot must be able to see FAC-2 after add.
	mp.AddTask(&provider.Task{ID: "c", Ref: "FAC-3", Status: "to-do", ProjectID: "p"})
	snap, err := store.SnapshotGraph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Empty graph edges ok; resolve FAC-3.
	id, err := store.ResolveRef(context.Background(), "FAC-3")
	if err != nil || id != "c" {
		t.Fatalf("new task not resolved: id=%s err=%v snap=%v", id, err, snap != nil)
	}
}

func TestMigrate_DryRunAndApply(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "t1", Ref: "FAC-1", Status: "to-do", ProjectID: "p", Title: "a"})
	mp.AddTask(&provider.Task{ID: "t2", Ref: "FAC-2", Status: "to-do", ProjectID: "p", Title: "b",
		Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-2\",\"task_id\":\"t2\",\"edges\":[]}\n```\n"})
	store := StoreFor(mp, "p")
	plan, err := PlanMigration(context.Background(), store, mp, "p")
	if err != nil {
		t.Fatal(err)
	}
	var wrote, skipped int
	for _, it := range plan.Items {
		switch it.Action {
		case "write_empty", "write_from_board":
			wrote++
		case "skip_fresh":
			skipped++
		}
	}
	if wrote < 1 || skipped < 1 {
		t.Fatalf("plan items=%+v wrote=%d skipped=%d", plan.Items, wrote, skipped)
	}
	jdir := t.TempDir()
	applied, err := ApplyMigration(context.Background(), store, mp, "p", MemoryDescriptionWriter{MP: mp}, jdir)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.OK {
		t.Fatalf("apply not ok: %+v", applied)
	}
	if applied.Mode != "apply-description" {
		t.Fatalf("mode want apply-description got %s", applied.Mode)
	}
	if applied.JournalPath == "" {
		t.Fatal("expected journal path")
	}
	// FAC-1 now launchable with empty provenance.
	t1, _ := mp.GetTask(context.Background(), "t1")
	p, err := ExtractProvenanceFromText(t1.Description)
	if err != nil || !p.Present {
		t.Fatalf("after migrate FAC-1 fence missing: %v", err)
	}
	if err := p.BindAndValidate("FAC-1", "t1"); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeMultisetEqual_CountsDuplicates(t *testing.T) {
	e := DependencyEdge{SourceRef: "A", TargetRef: "B", SourceID: "a", TargetID: "b", Type: EdgeBlocks}
	// Set semantics would treat [e,e] == [e]; multiset must not.
	if EdgeMultisetEqual([]DependencyEdge{e, e}, []DependencyEdge{e}) {
		t.Fatal("duplicate edges must not hide under set equality")
	}
	if !EdgeMultisetEqual([]DependencyEdge{e, e}, []DependencyEdge{e, e}) {
		t.Fatal("equal multisets should match")
	}
}

func TestLeaseOwnership_TwoIndependentManagers_ExactlyOneWinner(t *testing.T) {
	db := filepath.Join(t.TempDir(), "launch.db")
	wins, conflicts, err := TwoIndependentManagersClaim(
		context.Background(), db, "herd", "memory", "p", "FAC-1", "rev-abc",
	)
	if err != nil {
		t.Fatal(err)
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("want 1 win + 1 conflict, got wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestLeaseOwnership_ReleaseRefusesStaleGeneration(t *testing.T) {
	db := filepath.Join(t.TempDir(), "launch.db")
	a, err := OpenLeaseOwnership(db, "herd", "memory", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenLeaseOwnership(db, "herd", "memory", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	tokA, err := a.ClaimExclusive(context.Background(), "id1", "FAC-1", "launch", "rev1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Release A, re-claim with B so generation advances.
	if err := a.ReleaseIfOwner(context.Background(), tokA, "done"); err != nil {
		t.Fatal(err)
	}
	tokB, err := b.ClaimExclusive(context.Background(), "id1", "FAC-1", "launch", "rev1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Stale token A must not release B's lease.
	if err := a.ReleaseIfOwner(context.Background(), tokA, "stale"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("want ErrNotOwner for stale gen, got %v", err)
	}
	owns, err := b.StillOwns(context.Background(), tokB)
	if err != nil || !owns {
		t.Fatalf("B should still own: owns=%v err=%v", owns, err)
	}
}

// TestLeaseOwnership_ReleaseBeforeAcquireInterleaving proves the BAD order
// (release then B acquires) leaves A unable to release/stomp B — and the
// CORRECT order keeps A owner through durable work until explicit release.
func TestLeaseOwnership_ReleaseBeforeAcquireInterleaving(t *testing.T) {
	db := filepath.Join(t.TempDir(), "launch.db")
	a, err := OpenLeaseOwnership(db, "herd", "memory", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenLeaseOwnership(db, "herd", "memory", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	tokA, err := a.ClaimExclusive(context.Background(), "id1", "FAC-1", "launch", "rev1", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// --- BAD window simulation: release first (what failOwned used to do) ---
	if err := a.ReleaseIfOwner(context.Background(), tokA, "early_release"); err != nil {
		t.Fatal(err)
	}
	tokB, err := b.ClaimExclusive(context.Background(), "id1", "FAC-1", "launch", "rev1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Stale A must not be able to drop B's generation.
	if err := a.ReleaseIfOwner(context.Background(), tokA, "stale_after_B"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale A release must be ErrNotOwner, got %v", err)
	}
	ownsB, err := b.StillOwns(context.Background(), tokB)
	if err != nil || !ownsB {
		t.Fatalf("B must still own after stale A: owns=%v err=%v", ownsB, err)
	}
	if err := b.ReleaseIfOwner(context.Background(), tokB, "cleanup"); err != nil {
		t.Fatal(err)
	}

	// --- CORRECT order: hold through "durable", then release, then B acquires ---
	tokA2, err := a.ClaimExclusive(context.Background(), "id1", "FAC-1", "launch", "rev2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// While A still owns, B cannot acquire.
	if _, err := b.ClaimExclusive(context.Background(), "id1", "FAC-1", "launch", "rev2", "", ""); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("B must conflict while A holds: %v", err)
	}
	owns, err := a.StillOwns(context.Background(), tokA2)
	if err != nil || !owns {
		t.Fatalf("A must own through durable window: owns=%v err=%v", owns, err)
	}
	// Durable compensate would run here (caller-owned); only then release.
	if err := a.ReleaseIfOwner(context.Background(), tokA2, "after_durable"); err != nil {
		t.Fatal(err)
	}
	tokB2, err := b.ClaimExclusive(context.Background(), "id1", "FAC-1", "launch", "rev2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseIfOwner(context.Background(), tokA2, "stale"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("A after release must not touch B: %v", err)
	}
	if owns, _ := b.StillOwns(context.Background(), tokB2); !owns {
		t.Fatal("B must own after correct-order handoff")
	}
}

func TestMigrate_RepairStaleMissingTaskID(t *testing.T) {
	mp := provider.NewMemoryProvider()
	// Fence present but missing required task_id → repair_stale, not skip.
	mp.AddTask(&provider.Task{ID: "t1", Ref: "FAC-1", Status: "to-do", ProjectID: "p",
		Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-1\",\"edges\":[]}\n```\n"})
	store := StoreFor(mp, "p")
	plan, err := PlanMigration(context.Background(), store, mp, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Action != "repair_stale" {
		t.Fatalf("want repair_stale for missing task_id, got %+v", plan.Items)
	}
}

func TestReconcile_FAC75MissingEdge(t *testing.T) {
	desired := []DependencyEdge{
		{SourceRef: "FAC-136", TargetRef: "FAC-75", Type: EdgeBlocks},
	}
	rep := Reconcile("FAC-75", desired, nil, ReconcileOpts{FullClosure: []DependencyEdge{}, RequireFullClosure: true})
	if rep.OK {
		t.Fatal("expected missing drift")
	}
	if len(rep.Findings) == 0 || rep.Findings[0].Class != DriftMissing {
		t.Fatalf("want missing finding, got %+v", rep.Findings)
	}
	b, err := MarshalReport(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReconcileReport
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_ExtraEvenWhenDesiredEmpty(t *testing.T) {
	board := []DependencyEdge{
		{SourceRef: "FAC-136", TargetRef: "FAC-75", Type: EdgeBlocks, RelationID: "r1"},
	}
	rep := Reconcile("FAC-75", nil, board, ReconcileOpts{
		FullClosure: board, RequireFullClosure: true,
	})
	if rep.OK {
		t.Fatal("empty desired must still reject live extra board edges")
	}
	found := false
	for _, f := range rep.Findings {
		if f.Class == DriftExtra {
			found = true
		}
	}
	if !found {
		t.Fatalf("want extra finding, got %+v", rep.Findings)
	}
}

func TestReconcile_RequiresFullClosure(t *testing.T) {
	rep := Reconcile("FAC-75", nil, nil, ReconcileOpts{RequireFullClosure: true})
	if rep.OK {
		t.Fatal("missing full closure must fail")
	}
	found := false
	for _, f := range rep.Findings {
		if f.Class == DriftUnresolved && strings.Contains(f.Detail, "full graph") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want unresolved full graph finding: %+v", rep.Findings)
	}
}

func TestReconcile_DuplicateAndCycle(t *testing.T) {
	board := []DependencyEdge{
		{SourceRef: "A", TargetRef: "B", Type: EdgeBlocks, RelationID: "1"},
		{SourceRef: "A", TargetRef: "B", Type: EdgeBlocks, RelationID: "2"},
		{SourceRef: "B", TargetRef: "C", Type: EdgeBlocks, RelationID: "3"},
		{SourceRef: "C", TargetRef: "A", Type: EdgeBlocks, RelationID: "4"},
	}
	rep := Reconcile("B", nil, board, ReconcileOpts{FullClosure: board, RequireFullClosure: true})
	classes := map[DriftClass]int{}
	for _, f := range rep.Findings {
		classes[f.Class]++
	}
	if classes[DriftDuplicate] < 1 {
		t.Fatalf("want duplicate, got %+v", rep.Findings)
	}
	if classes[DriftCyclic] < 1 {
		t.Fatalf("want cyclic, got %+v", rep.Findings)
	}
}

func TestGate_RequiresProvenance(t *testing.T) {
	m := seedBoard(t)
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-75", nil, "")
	if err == nil {
		t.Fatal("missing provenance must fail")
	}
	var be *BlockedError
	if !errors.As(err, &be) || be.Code != "missing_provenance" {
		t.Fatalf("code=%v", err)
	}
}

func TestGate_OpenBlockerBlocks(t *testing.T) {
	m := seedBoard(t)
	if _, err := m.SeedBlocks("FAC-136", "FAC-75"); err != nil {
		t.Fatal(err)
	}
	// Desired must match board edge; blocker FAC-136 is done.
	des := prov("FAC-75", DependencyEdge{SourceRef: "FAC-136", TargetRef: "FAC-75", Type: EdgeBlocks})
	gr, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-75", des, "")
	if err != nil {
		t.Fatalf("done blocker should unlock: %v", err)
	}
	if !gr.OK {
		t.Fatal("expected OK")
	}
	if len(gr.GraphRevision) != 64 { // sha256 hex
		t.Fatalf("want sha256 hex revision, got %q", gr.GraphRevision)
	}

	if _, err := m.SeedBlocks("FAC-93", "FAC-105"); err != nil {
		t.Fatal(err)
	}
	des105 := prov("FAC-105", DependencyEdge{SourceRef: "FAC-93", TargetRef: "FAC-105", Type: EdgeBlocks})
	_, err = ValidateLaunch(context.Background(), m, EntryPulse, "FAC-105", des105, "")
	if err == nil {
		t.Fatal("expected open blocker")
	}
	var be *BlockedError
	errors.As(err, &be)
	if be.Code != "open_blocker" {
		t.Fatalf("code=%s", be.Code)
	}
}

func transitiveStatusFixture(t *testing.T, ancestorStatus string) (*MemoryStore, *Provenance) {
	t.Helper()
	m := NewMemoryStore()
	// Keep the relation-provider revision stable across status mutations. The
	// proof must depend on transitive status reads, not synthetic revision churn.
	m.ProviderRevisionToken = "fixed-relation-revision"
	m.EnsureTask("FAC-A", ancestorStatus, provider.PriorityHigh)
	m.EnsureTask("FAC-B", provider.StatusDone, provider.PriorityHigh)
	m.EnsureTask("FAC-T", provider.StatusToDo, provider.PriorityUrgent)
	if _, err := m.SeedBlocks("FAC-A", "FAC-B"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SeedBlocks("FAC-B", "FAC-T"); err != nil {
		t.Fatal(err)
	}
	return m, prov("FAC-T", DependencyEdge{
		SourceRef: "FAC-B", TargetRef: "FAC-T", Type: EdgeBlocks,
	})
}

func TestGate_IndirectPrerequisiteNonDoneBlocks(t *testing.T) {
	m, desired := transitiveStatusFixture(t, provider.StatusToDo)
	result, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-T", desired, "")
	if err == nil {
		t.Fatal("indirect prerequisite FAC-A is to-do; launch must fail closed")
	}
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Code != "open_blocker" {
		t.Fatalf("want open_blocker for indirect prerequisite, got %v", err)
	}
	if result == nil || len(result.BlockedBy) != 1 || result.BlockedBy[0] != "FAC-A" {
		t.Fatalf("want only indirect FAC-A blocked, got %+v", result)
	}
	if got := result.StatusByBlocker["FAC-A"]; got != provider.StatusToDo {
		t.Fatalf("want FAC-A status %q, got %q", provider.StatusToDo, got)
	}
}

func TestFencedClaim_IndirectPrerequisiteStatusMutationCompensatesOnce(t *testing.T) {
	m, desired := transitiveStatusFixture(t, provider.StatusDone)
	pre, err := ValidateLaunch(context.Background(), m, EntryClaim, "FAC-T", desired, "")
	if err != nil {
		t.Fatalf("all transitive prerequisites start done: %v", err)
	}

	compensations := 0
	_, err = FencedClaim(
		context.Background(), m, "FAC-T", "id-fac-t", desired, pre.GraphRevision,
		func(context.Context) error {
			return m.SetStatus("FAC-A", provider.StatusToDo)
		},
		func(context.Context, TaskID, string) error {
			compensations++
			return nil
		},
	)
	if err == nil || !errors.Is(err, ErrPostClaimDrift) {
		t.Fatalf("want post-claim drift for indirect status mutation, got %v", err)
	}
	if compensations != 1 {
		t.Fatalf("want exactly one compensation, got %d", compensations)
	}
}

func TestGate_UnknownStatusFailClosed(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-1", "to-do", provider.PriorityUrgent)
	m.EnsureTask("FAC-2", "to-do", provider.PriorityHigh)
	if _, err := m.SeedBlocks("FAC-2", "FAC-1"); err != nil {
		t.Fatal(err)
	}
	_ = m.SetStatus("FAC-2", "mystery-status")
	des := prov("FAC-1", DependencyEdge{SourceRef: "FAC-2", TargetRef: "FAC-1", Type: EdgeBlocks})
	// status mystery normalizes to unknown: — TaskStatus on memory returns stored;
	// gate uses NormalizeStatus; unknown is not StatusDone → open_blocker.
	// If we want unreadable hard fail, TaskStatus would need to error — memory returns raw.
	// After Normalize in SetStatus it's unknown:mystery-status which is not done.
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-1", des, "")
	if err == nil {
		t.Fatal("unknown blocker status must block")
	}
}

func TestGate_TOCTOU_RevisionMismatch(t *testing.T) {
	m := seedBoard(t)
	des := EmptyProvenanceBound("FAC-75", "id-fac-75")
	gr, err := ValidateLaunch(context.Background(), m, EntryPulse, "FAC-75", des, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SeedBlocks("FAC-93", "FAC-75"); err != nil {
		t.Fatal(err)
	}
	// After concurrent edge: either drift (extra without desired) or toctou.
	_, err = ValidateClaim(context.Background(), m, "FAC-75", des, gr.GraphRevision)
	if err == nil {
		t.Fatal("expected TOCTOU/drift after concurrent relation")
	}
}

func TestGate_ClaimFenceRequired(t *testing.T) {
	m := seedBoard(t)
	_, err := ValidateClaim(context.Background(), m, "FAC-75", EmptyProvenanceBound("FAC-75", "id-fac-75"), "")
	if err == nil {
		t.Fatal("empty selection revision must fail fence")
	}
	var be *BlockedError
	if !errors.As(err, &be) || be.Code != "toctou" {
		t.Fatalf("want toctou fence, got %v", err)
	}
}

func TestGate_DesiredMissingOnBoard(t *testing.T) {
	m := seedBoard(t)
	des := &Provenance{
		Version: SchemaVersion,
		TaskRef: "FAC-75",
		TaskID:  "id-fac-75",
		Present: true,
		Holds: []Hold{{
			Kind:         HoldCollisionOwnership,
			BlockerRef:   "FAC-136",
			DependentRef: "FAC-75",
			Paths:        []string{"pkg/verifier"},
		}},
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-75", des, "")
	if err == nil {
		t.Fatal("hold without board edge must fail closed")
	}
	var be *BlockedError
	errors.As(err, &be)
	if be.Code != "drift" {
		t.Fatalf("code=%s want drift", be.Code)
	}
}

func TestProvenance_NeverParsesProse(t *testing.T) {
	text := `## Dependencies
This card depends on FAC-136 and blocks FAC-105. required by FAC-90.
`
	p, err := ExtractProvenanceFromText(text)
	if err != nil {
		t.Fatal(err)
	}
	if p.Present {
		t.Fatal("prose must not yield Present provenance")
	}
}

func TestProvenance_RejectsUnknownType(t *testing.T) {
	_, err := ParseProvenanceJSON([]byte(`{"version":1,"edges":[{"source_ref":"A","target_ref":"B","type":"blocks-ish"}]}`))
	if err == nil {
		t.Fatal("unknown type must fail")
	}
}

func TestProvenance_FenceAuthoritative(t *testing.T) {
	raw := "intro\n```herd-deps-v1\n" +
		`{"version":1,"task_ref":"FAC-75","edges":[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]}` +
		"\n```\n"
	p, err := ExtractProvenanceFromText(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Present {
		t.Fatal("fence must be Present")
	}
	if err := func() error { p.TaskID = "id-fac-75"; return p.BindAndValidate("FAC-75", "id-fac-75") }(); err != nil {
		t.Fatal(err)
	}
	blocks, err := p.DesiredBlocks()
	if err != nil || len(blocks) != 1 {
		t.Fatalf("want 1 edge: %v %+v", err, blocks)
	}
}

func TestProvenance_RejectsReplayWrongTaskRef(t *testing.T) {
	p := prov("FAC-75", DependencyEdge{SourceRef: "FAC-136", TargetRef: "FAC-75", Type: EdgeBlocks})
	if err := p.BindAndValidate("FAC-90", "id-fac-90"); err == nil {
		t.Fatal("wrong task_ref must fail bind")
	}
}

func TestProvenance_RejectsMultipleFences(t *testing.T) {
	raw := "```herd-deps-v1\n" +
		`{"version":1,"task_ref":"FAC-75","edges":[]}` +
		"\n```\n```herd-deps-v1\n" +
		`{"version":1,"task_ref":"FAC-75","edges":[]}` +
		"\n```\n"
	if _, err := ExtractProvenanceFromText(raw); err == nil {
		t.Fatal("multiple fences must fail")
	}
}

func TestProvenance_RejectsUnknownJSONFields(t *testing.T) {
	_, err := ParseProvenanceJSON([]byte(`{"version":1,"task_ref":"FAC-1","edges":[],"nope":true}`))
	if err == nil {
		t.Fatal("unknown fields must fail")
	}
}

func TestProvenance_RejectsDuplicateEdges(t *testing.T) {
	p := &Provenance{
		Version: 1, TaskRef: "FAC-1", Present: true,
		Edges: []DependencyEdge{
			{SourceRef: "A", TargetRef: "FAC-1", Type: EdgeBlocks},
			{SourceRef: "A", TargetRef: "FAC-1", Type: EdgeBlocks},
		},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("duplicate edges must fail Validate")
	}
}

func TestCapabilityUnsupported_FailsWholeSelection(t *testing.T) {
	m := NewMemoryStore()
	m.Capable = false
	m.EnsureTask("FAC-1", "to-do", provider.PriorityLow)
	cands := []*provider.Task{{ID: "id-fac-1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityLow}}
	_, _, _, err := SelectEligibleRefs(context.Background(), m, EntryWave, cands, nil)
	if err == nil {
		t.Fatal("capability must fail whole selection")
	}
	if !IsHardSelectionFailure(err) {
		t.Fatalf("want hard selection failure: %v", err)
	}
}

func TestSelectEligible_PreservesPriorityOrder(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-10", "to-do", provider.PriorityMedium)
	m.EnsureTask("FAC-2", "to-do", provider.PriorityUrgent)
	m.EnsureTask("FAC-3", "to-do", provider.PriorityUrgent)
	cands := []*provider.Task{
		{ID: "id-fac-2", Ref: "FAC-2", Status: "to-do", Priority: provider.PriorityUrgent},
		{ID: "id-fac-3", Ref: "FAC-3", Status: "to-do", Priority: provider.PriorityUrgent},
		{ID: "id-fac-10", Ref: "FAC-10", Status: "to-do", Priority: provider.PriorityMedium},
	}
	desired := map[string]*Provenance{
		"FAC-2":  EmptyProvenanceBound("FAC-2", "id-fac-2"),
		"FAC-3":  EmptyProvenanceBound("FAC-3", "id-fac-3"),
		"FAC-10": EmptyProvenanceBound("FAC-10", "id-fac-10"),
	}
	// Block FAC-2 with open blocker
	if _, err := m.SeedBlocks("FAC-10", "FAC-2"); err != nil {
		t.Fatal(err)
	}
	// FAC-2 desired must declare the edge or drift; with empty desired + board edge = extra drift.
	// So FAC-2 blocked by drift; FAC-3 and FAC-10 OK with empty desired (no board edges for them).
	// Wait - FAC-10 is source of edge involving FAC-10 as source - board edge FAC-10→FAC-2 involves FAC-10
	// so FAC-10 has extra edge without provenance. Fix: give FAC-2 and FAC-10 matching desired.
	desired["FAC-2"] = prov("FAC-2", DependencyEdge{SourceRef: "FAC-10", TargetRef: "FAC-2", Type: EdgeBlocks})
	desired["FAC-10"] = prov("FAC-10", DependencyEdge{SourceRef: "FAC-10", TargetRef: "FAC-2", Type: EdgeBlocks})

	el, _, blocked, err := SelectEligibleRefs(context.Background(), m, EntryPulse, cands, desired)
	if err != nil {
		t.Fatal(err)
	}
	// FAC-2 open-blocked (FAC-10 to-do); FAC-3 eligible; FAC-10 has outbound edge only — inbound none, desired matches board involving FAC-10
	// FAC-10: managed board has edge where source is FAC-10 — desired has same → OK; no open inbound blockers.
	if len(el) < 1 || el[0].Ref != "FAC-3" {
		t.Fatalf("eligible order=%v blocked=%d", refsOf(el), len(blocked))
	}
}

func refsOf(ts []*provider.Task) []string {
	var o []string
	for _, t := range ts {
		o = append(o, t.Ref)
	}
	return o
}

func TestMemory_CreateDeleteDualReadback(t *testing.T) {
	m := seedBoard(t)
	e, err := m.CreateRelation(context.Background(), DependencyEdge{
		SourceRef: "FAC-136", TargetRef: "FAC-90", Type: EdgeBlocks,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Self-edge rejected.
	if _, err := m.CreateRelation(context.Background(), DependencyEdge{
		SourceRef: "FAC-136", TargetRef: "FAC-136", Type: EdgeBlocks,
	}); !errors.Is(err, ErrSelfEdge) {
		t.Fatalf("want self-edge err, got %v", err)
	}
	// Unknown type rejected.
	if _, err := m.CreateRelation(context.Background(), DependencyEdge{
		SourceRef: "FAC-136", TargetRef: "FAC-90", Type: "nope",
	}); !errors.Is(err, ErrUnknownEdgeType) {
		t.Fatalf("want unknown type, got %v", err)
	}
	// Idempotent create.
	e2, err := m.CreateRelation(context.Background(), DependencyEdge{
		SourceRef: "FAC-136", TargetRef: "FAC-90", Type: EdgeBlocks,
	})
	if err != nil || e2.RelationID != e.RelationID {
		t.Fatalf("idempotent create: %+v err=%v", e2, err)
	}
	if err := m.DeleteRelation(context.Background(), e.RelationID, e.SourceID, e.TargetID); err != nil {
		t.Fatal(err)
	}
	list, _ := m.ListRelations(context.Background(), TaskID("id-fac-90"))
	if len(list) != 0 {
		t.Fatalf("delete dual readback still has %v", list)
	}
}

func TestGraphRevision_IsSHA256(t *testing.T) {
	rev := GraphRevision([]DependencyEdge{{
		SourceID: "a", TargetID: "b", Type: EdgeBlocks, RelationID: "r1",
	}}, map[string]string{"FAC-1": "done"}, "prov-1")
	if len(rev) != 64 {
		t.Fatalf("sha256 hex length want 64 got %d (%s)", len(rev), rev)
	}
	// Stable.
	rev2 := GraphRevision([]DependencyEdge{{
		SourceID: "a", TargetID: "b", Type: EdgeBlocks, RelationID: "r1",
	}}, map[string]string{"FAC-1": "done"}, "prov-1")
	if rev != rev2 {
		t.Fatal("revision not stable")
	}
}

func TestMutationControl_ReconcileNotVacuouslyOK(t *testing.T) {
	rep := Reconcile("FAC-93", []DependencyEdge{
		{SourceRef: "FAC-117", TargetRef: "FAC-93", Type: EdgeBlocks},
	}, nil, ReconcileOpts{FullClosure: []DependencyEdge{}, RequireFullClosure: true})
	if rep.OK {
		t.Fatal("MUTATION: Reconcile returned OK for missing edge — gate is vacuous")
	}
}

func TestFencedClaim_CompensatesOnPostDrift(t *testing.T) {
	m := seedBoard(t)
	des := EmptyProvenanceBound("FAC-75", "id-fac-75")
	pre, err := ValidateLaunch(context.Background(), m, EntryPulse, "FAC-75", des, "")
	if err != nil {
		t.Fatal(err)
	}
	claimed := false
	compensated := false
	// During claimFn, mutate graph so post-claim fails.
	_, err = FencedClaim(context.Background(), m, "FAC-75", "id-fac-75", des, pre.GraphRevision,
		func(ctx context.Context) error {
			claimed = true
			_, _ = m.SeedBlocks("FAC-93", "FAC-75")
			return nil
		},
		func(ctx context.Context, taskID TaskID, reason string) error {
			compensated = true
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected post-claim drift")
	}
	if !claimed || !compensated {
		t.Fatalf("claimed=%v compensated=%v", claimed, compensated)
	}
	if !errors.Is(err, ErrPostClaimDrift) {
		t.Fatalf("want ErrPostClaimDrift, got %v", err)
	}
}
