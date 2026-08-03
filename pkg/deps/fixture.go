package deps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// DriftFixture is a hermetic board snapshot for FAC-75/90/93/105 regression.
type DriftFixture struct {
	Name     string            `json:"name"`
	Tasks    []FixtureTask     `json:"tasks"`
	Board    []DependencyEdge  `json:"board"`
	Desired  map[string][]DependencyEdge `json:"desired"` // task ref → desired blocks
	// ExpectBlocked refs that must fail the launch gate.
	ExpectBlocked []string `json:"expect_blocked"`
	// ExpectOK refs that must pass (all blockers done, no drift).
	ExpectOK []string `json:"expect_ok"`
}

// FixtureTask is one card in a drift fixture.
type FixtureTask struct {
	ID       string            `json:"id"`
	Ref      string            `json:"ref"`
	Status   string            `json:"status"`
	Priority provider.Priority `json:"priority"`
}

// FAC759093105Fixture reproduces the live audit drift class for FAC-75/90/93/105.
func FAC759093105Fixture() DriftFixture {
	return DriftFixture{
		Name: "fac-75-90-93-105-relation-drift",
		Tasks: []FixtureTask{
			{ID: "id-75", Ref: "FAC-75", Status: "to-do", Priority: provider.PriorityHigh},
			{ID: "id-90", Ref: "FAC-90", Status: "to-do", Priority: provider.PriorityHigh},
			{ID: "id-93", Ref: "FAC-93", Status: "to-do", Priority: provider.PriorityUrgent},
			{ID: "id-105", Ref: "FAC-105", Status: "to-do", Priority: provider.PriorityHigh},
			{ID: "id-136", Ref: "FAC-136", Status: "done", Priority: provider.PriorityHigh},
			{ID: "id-69", Ref: "FAC-69", Status: "to-do", Priority: provider.PriorityHigh},
			{ID: "id-73", Ref: "FAC-73", Status: "to-do", Priority: provider.PriorityMedium},
			{ID: "id-117", Ref: "FAC-117", Status: "done", Priority: provider.PriorityMedium},
		},
		// Board intentionally missing the audit-repaired edges for FAC-75/90
		// and missing FAC-73→FAC-105; FAC-117→FAC-93 present and done (OK).
		Board: []DependencyEdge{
			{RelationID: "b1", SourceRef: "FAC-117", TargetRef: "FAC-93", Type: EdgeBlocks,
				SourceID: "id-117", TargetID: "id-93"},
		},
		Desired: map[string][]DependencyEdge{
			"FAC-75":  {{SourceRef: "FAC-136", TargetRef: "FAC-75", Type: EdgeBlocks}},
			"FAC-90":  {{SourceRef: "FAC-136", TargetRef: "FAC-90", Type: EdgeBlocks}},
			"FAC-105": {{SourceRef: "FAC-73", TargetRef: "FAC-105", Type: EdgeBlocks}},
			"FAC-93":  {{SourceRef: "FAC-117", TargetRef: "FAC-93", Type: EdgeBlocks}},
		},
		ExpectBlocked: []string{"FAC-75", "FAC-90", "FAC-105"},
		ExpectOK:      []string{"FAC-93"},
	}
}

// LoadStore builds a MemoryStore from a fixture.
func (f DriftFixture) LoadStore() *MemoryStore {
	m := NewMemoryStore()
	for _, t := range f.Tasks {
		id := t.ID
		if id == "" {
			id = "id-" + t.Ref
		}
		m.AddTask(&provider.Task{
			ID: id, Ref: t.Ref, Title: t.Ref,
			Status: provider.NormalizeStatus(t.Status),
			Priority: t.Priority, ProjectID: "fixture",
		})
	}
	for _, e := range f.Board {
		_, _ = m.CreateRelation(context.Background(), e)
	}
	return m
}

// RunFixture executes reconcile + gate checks. Returns non-nil error on failure
// (nonzero exit for CLI / binary tests).
func RunFixture(f DriftFixture) error {
	store := f.LoadStore()
	var failures []string

	for ref, desired := range f.Desired {
		rep := Reconcile(Ref(ref), desired, f.Board)
		// Gate path also checks open blockers with store.
		des := &Provenance{Version: SchemaVersion, TaskRef: Ref(ref), Edges: desired}
		_, gerr := ValidateLaunch(context.Background(), store, EntryDispatch, Ref(ref), des, "")

		wantBlock := false
		for _, b := range f.ExpectBlocked {
			if b == ref {
				wantBlock = true
				break
			}
		}
		wantOK := false
		for _, o := range f.ExpectOK {
			if o == ref {
				wantOK = true
				break
			}
		}

		if wantBlock {
			if gerr == nil && rep.OK {
				failures = append(failures, fmt.Sprintf("%s: expected BLOCKED drift/open, got OK", ref))
			}
		}
		if wantOK {
			if gerr != nil {
				failures = append(failures, fmt.Sprintf("%s: expected OK, got %v", ref, gerr))
			}
		}
		_ = rep
	}

	if len(failures) > 0 {
		return fmt.Errorf("fixture %s failed:\n  %s", f.Name, joinNL(failures))
	}
	return nil
}

func joinNL(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}

// WriteFixtureJSON writes a fixture to path (relative or absolute — caller owns).
func WriteFixtureJSON(path string, f DriftFixture) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

// MutationControl_ReconcileRemoved is called by binary tests: if Reconcile is
// replaced with a vacuous OK, this returns an error the harness must see.
func MutationControl_ReconcileRemoved() error {
	rep := Reconcile("FAC-75", []DependencyEdge{
		{SourceRef: "FAC-136", TargetRef: "FAC-75", Type: EdgeBlocks},
	}, nil)
	if rep.OK {
		return fmt.Errorf("MUTATION CONTROL FAILED: Reconcile is vacuous (OK on missing edge)")
	}
	return nil
}

// MutationControl_GateBypassed ensures ValidateLaunch fails on open blockers.
func MutationControl_GateBypassed() error {
	m := NewMemoryStore()
	m.EnsureTask("FAC-75", "to-do", provider.PriorityHigh)
	m.EnsureTask("FAC-136", "to-do", provider.PriorityHigh) // not done
	if _, err := m.SeedBlocks("FAC-136", "FAC-75"); err != nil {
		return err
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-75", nil, "")
	if err == nil {
		return fmt.Errorf("MUTATION CONTROL FAILED: gate allowed open blocker")
	}
	return nil
}
