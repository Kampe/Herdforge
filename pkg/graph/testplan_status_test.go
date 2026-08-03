package graph

import (
	"strings"
	"testing"
)

func TestParseGraphStatusJSON(t *testing.T) {
	raw := []byte(`{"nodes":10,"edges":20,"files":5,"built_at_commit":"abc123","extra":true}`)
	r, err := ParseGraphStatusJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.BuiltAtCommit != "abc123" || r.Nodes != 10 || r.Edges != 20 || r.Files != 5 {
		t.Fatalf("unexpected report: %+v", r)
	}
}

func TestParseGraphStatusJSON_EmptyAndInvalid(t *testing.T) {
	if _, err := ParseGraphStatusJSON(nil); err == nil {
		t.Fatal("empty must fail")
	}
	if _, err := ParseGraphStatusJSON([]byte(`{`)); err == nil {
		t.Fatal("invalid JSON must fail")
	}
}

func TestEvidenceFromStatus_UnbuiltIsMissing(t *testing.T) {
	ev := EvidenceFromStatus(GraphStatusReport{}, nil)
	state, _ := classifyGraph(ev, "base")
	if state != GraphMissing {
		t.Fatalf("state = %s, want unavailable", state)
	}
}

func TestEvidenceFromStatus_RecordsRevision(t *testing.T) {
	hits := []GraphHit{{Kind: "tests_for", Target: "a", FilePath: "pkg/a/a_test.go"}}
	ev := EvidenceFromStatus(GraphStatusReport{BuiltAtCommit: "base1"}, hits)
	if ev.BuiltAtCommit != "base1" {
		t.Fatal("built_at not recorded")
	}
	if len(ev.Hits) != 1 {
		t.Fatal("hits not carried")
	}
	// Defensive copy: mutating caller's slice must not affect evidence after build
	// if we re-call — EvidenceFromStatus copies; mutate original.
	hits[0].FilePath = "mutated"
	if ev.Hits[0].FilePath == "mutated" {
		t.Fatal("EvidenceFromStatus must copy hits")
	}

	in := baseInput("pkg/a/a.go")
	in.BaseSHA = "base1"
	in.Graph = ev
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GraphBuiltAt != "base1" {
		t.Fatalf("plan must record graph revision, got %q", plan.GraphBuiltAt)
	}
	if plan.GraphState != GraphAvailable {
		t.Fatalf("state = %s", plan.GraphState)
	}
}

func TestPlan_RequireFreshGraphMutationGuard(t *testing.T) {
	// Non-vacuous: RequireFreshGraph must actually reject stale evidence.
	// If the guard is removed, this test fails.
	in := baseInput("pkg/graph/graph.go")
	in.Graph.BuiltAtCommit = "not-the-base"
	in.Profile.RequireFreshGraph = true
	_, err := Plan(in)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("RequireFreshGraph guard not enforced: err=%v", err)
	}
}
