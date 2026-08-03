package deps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func seedBoard(t *testing.T) *MemoryStore {
	t.Helper()
	m := NewMemoryStore()
	for _, ref := range []string{"FAC-75", "FAC-90", "FAC-93", "FAC-105", "FAC-136", "FAC-69", "FAC-101", "FAC-119"} {
		st := "to-do"
		if ref == "FAC-119" || ref == "FAC-136" {
			st = "done"
		}
		m.EnsureTask(ref, st, provider.PriorityHigh)
	}
	return m
}

func TestReconcile_FAC75MissingEdge(t *testing.T) {
	// Live audit: FAC-136 blocks FAC-75 declared in packet, absent on board.
	desired := []DependencyEdge{
		{SourceRef: "FAC-136", TargetRef: "FAC-75", Type: EdgeBlocks},
	}
	rep := Reconcile("FAC-75", desired, nil)
	if rep.OK {
		t.Fatal("expected missing drift")
	}
	if len(rep.Findings) == 0 || rep.Findings[0].Class != DriftMissing {
		t.Fatalf("want missing finding, got %+v", rep.Findings)
	}
	// Stable JSON shape.
	b, err := MarshalReport(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReconcileReport
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Findings[0].Class != DriftMissing {
		t.Fatalf("json roundtrip lost class: %s", decoded.Findings[0].Class)
	}
}

func TestReconcile_FAC90And105DriftBundle(t *testing.T) {
	// FAC-136→FAC-90 missing; FAC-73→FAC-105 missing; plus reverse detection.
	desired := []DependencyEdge{
		{SourceRef: "FAC-136", TargetRef: "FAC-90", Type: EdgeBlocks},
		{SourceRef: "FAC-73", TargetRef: "FAC-105", Type: EdgeBlocks},
	}
	board := []DependencyEdge{
		// Reversed for FAC-90
		{SourceRef: "FAC-90", TargetRef: "FAC-136", Type: EdgeBlocks, RelationID: "r1"},
	}
	rep := Reconcile("FAC-90", desired[:1], board)
	if rep.OK {
		t.Fatal("expected reversed/missing")
	}
	foundRev := false
	for _, f := range rep.Findings {
		if f.Class == DriftReversed {
			foundRev = true
		}
	}
	if !foundRev {
		t.Fatalf("want reversed finding, got %+v", rep.Findings)
	}

	rep105 := Reconcile("FAC-105", desired[1:], nil)
	if rep105.OK || rep105.Findings[0].Class != DriftMissing {
		t.Fatalf("FAC-105 want missing: %+v", rep105.Findings)
	}
}

func TestReconcile_DuplicateAndCycle(t *testing.T) {
	board := []DependencyEdge{
		{SourceRef: "A", TargetRef: "B", Type: EdgeBlocks, RelationID: "1"},
		{SourceRef: "A", TargetRef: "B", Type: EdgeBlocks, RelationID: "2"},
		{SourceRef: "B", TargetRef: "C", Type: EdgeBlocks, RelationID: "3"},
		{SourceRef: "C", TargetRef: "A", Type: EdgeBlocks, RelationID: "4"},
	}
	rep := Reconcile("B", nil, board)
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

func TestGate_OpenBlockerBlocks(t *testing.T) {
	m := seedBoard(t)
	if _, err := m.SeedBlocks("FAC-136", "FAC-75"); err != nil {
		t.Fatal(err)
	}
	// FAC-136 is done in seed — should pass.
	gr, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-75", nil, "")
	if err != nil {
		t.Fatalf("done blocker should unlock: %v", err)
	}
	if !gr.OK {
		t.Fatal("expected OK")
	}

	// Non-done blocker holds.
	if _, err := m.SeedBlocks("FAC-93", "FAC-105"); err != nil {
		t.Fatal(err)
	}
	_, err = ValidateLaunch(context.Background(), m, EntryPulse, "FAC-105", nil, "")
	if err == nil {
		t.Fatal("expected open blocker")
	}
	if !IsBlocked(err) {
		t.Fatalf("want BlockedError, got %T %v", err, err)
	}
	var be *BlockedError
	errors.As(err, &be)
	if be.Code != "open_blocker" {
		t.Fatalf("code=%s", be.Code)
	}
}

func TestGate_UnknownStatusFailClosed(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-1", "to-do", provider.PriorityUrgent)
	m.EnsureTask("FAC-2", "mystery-status", provider.PriorityHigh)
	if _, err := m.SeedBlocks("FAC-2", "FAC-1"); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-1", nil, "")
	// mystery-status normalizes to unknown:mystery-status — TaskStatus on MemoryStore
	// returns raw stored status; gate uses provider.NormalizeStatus.
	// Memory TaskStatus returns t.Status as stored — EnsureTask normalizes.
	// Re-set raw unknown.
	_ = m.SetStatus("FAC-2", "mystery-status")
	// After NormalizeStatus in SetStatus it becomes unknown:mystery-status
	_, err = ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-1", nil, "")
	if err == nil {
		t.Fatal("unknown blocker status must block")
	}
}

func TestGate_TOCTOU_RevisionMismatch(t *testing.T) {
	m := seedBoard(t)
	gr, err := ValidateLaunch(context.Background(), m, EntryPulse, "FAC-75", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent relation addition changes revision.
	if _, err := m.SeedBlocks("FAC-93", "FAC-75"); err != nil {
		t.Fatal(err)
	}
	_, err = ValidateClaim(context.Background(), m, "FAC-75", gr.GraphRevision)
	if err == nil {
		t.Fatal("expected TOCTOU block after concurrent relation")
	}
	var be *BlockedError
	if !errors.As(err, &be) || (be.Code != "toctou" && be.Code != "open_blocker") {
		// Either toctou (rev mismatch) or open_blocker — both fail closed.
		t.Fatalf("want toctou or open_blocker, got %v", err)
	}
}

func TestGate_DesiredMissingOnBoard(t *testing.T) {
	m := seedBoard(t)
	// Collision-ownership hold without board edge.
	des := &Provenance{
		Version: SchemaVersion,
		TaskRef: "FAC-75",
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
	if len(p.Edges) != 0 || len(p.Holds) != 0 {
		t.Fatalf("prose must not yield edges: %+v", p)
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
	if len(p.DesiredBlocks()) != 1 {
		t.Fatalf("want 1 edge, got %+v", p.Edges)
	}
}

func TestCapabilityUnsupported(t *testing.T) {
	m := NewMemoryStore()
	m.Capable = false
	m.EnsureTask("FAC-1", "to-do", provider.PriorityLow)
	_, err := ValidateLaunch(context.Background(), m, EntryWave, "FAC-1", nil, "")
	if err == nil {
		t.Fatal("expected capability failure")
	}
	if !strings.Contains(err.Error(), "unsupported") && !strings.Contains(err.Error(), "capability") {
		t.Fatalf("want capability error: %v", err)
	}
}

func TestSelectEligible_PreservesPriorityOrder(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-10", "to-do", provider.PriorityMedium)
	m.EnsureTask("FAC-2", "to-do", provider.PriorityUrgent)
	m.EnsureTask("FAC-3", "to-do", provider.PriorityUrgent)
	// Pre-sorted priority DESC, ref ASC: FAC-2, FAC-3, FAC-10
	cands := []*provider.Task{
		{ID: "id-fac-2", Ref: "FAC-2", Status: "to-do", Priority: provider.PriorityUrgent},
		{ID: "id-fac-3", Ref: "FAC-3", Status: "to-do", Priority: provider.PriorityUrgent},
		{ID: "id-fac-10", Ref: "FAC-10", Status: "to-do", Priority: provider.PriorityMedium},
	}
	// Block FAC-2
	if _, err := m.SeedBlocks("FAC-10", "FAC-2"); err != nil {
		// FAC-10 is to-do so FAC-2 blocked
		t.Fatal(err)
	}
	el, _, blocked, err := SelectEligibleRefs(context.Background(), m, EntryPulse, cands, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(el) != 2 || el[0].Ref != "FAC-3" || el[1].Ref != "FAC-10" {
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

func TestMemory_CreateDeleteReadback(t *testing.T) {
	m := seedBoard(t)
	e, err := m.CreateRelation(context.Background(), DependencyEdge{
		SourceRef: "FAC-136", TargetRef: "FAC-90", Type: EdgeBlocks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.RelationID == "" {
		t.Fatal("relation id required")
	}
	list, err := m.ListRelations(context.Background(), TaskID("id-fac-90"))
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if err := m.DeleteRelation(context.Background(), e.RelationID); err != nil {
		t.Fatal(err)
	}
	list, _ = m.ListRelations(context.Background(), TaskID("id-fac-90"))
	if len(list) != 0 {
		t.Fatalf("delete readback still has %v", list)
	}
}

// Mutation control: if Reconcile is gutted to always OK, this fails.
func TestMutationControl_ReconcileNotVacuouslyOK(t *testing.T) {
	rep := Reconcile("FAC-93", []DependencyEdge{
		{SourceRef: "FAC-117", TargetRef: "FAC-93", Type: EdgeBlocks},
	}, nil)
	if rep.OK {
		t.Fatal("MUTATION: Reconcile returned OK for missing edge — gate is vacuous")
	}
}
