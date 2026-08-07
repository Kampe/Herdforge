package graph

import (
	"strings"
	"testing"
)

func planReasons(t *testing.T, in PlanInput) (*TestPlan, string) {
	t.Helper()
	p, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	return p, strings.Join(p.EscalationReasons, "; ")
}

func hasStage(p *TestPlan, s Stage) bool {
	for _, c := range p.Commands {
		if c.Stage == s {
			return true
		}
	}
	return false
}

// TestPlan_MissingTestsForEdgesEscalates is the FAC-160 rule: a complete,
// revision-bound index that still shows no tests_for edge for a changed
// exported production symbol is uncovered surface, so the plan must broaden
// instead of shipping a targeted plan that exercises nothing.
//
// The paired sub-test with a tests_for hit present is what makes this
// non-vacuous: removing the rule leaves the "covered" case unchanged and the
// "uncovered" case failing.
func TestPlan_MissingTestsForEdgesEscalates(t *testing.T) {
	t.Parallel()
	const reason = "no graph tests_for edges for changed exported production symbols"

	mk := func() PlanInput {
		in := baseInput("pkg/store/store.go")
		in.ChangedSymbols = []string{"PersistReceipt"}
		return in
	}

	t.Run("no tests_for edge escalates", func(t *testing.T) {
		t.Parallel()
		in := mk()
		in.Graph.Hits = []GraphHit{
			{Kind: "callers_of", Target: "PersistReceipt", FilePath: "pkg/verifier/admission.go"},
		}
		p, reasons := planReasons(t, in)
		if !strings.Contains(reasons, reason) {
			t.Fatalf("expected tests_for-gap escalation, got %q", reasons)
		}
		if !p.Escalated || !hasStage(p, StageFull) {
			t.Fatalf("uncovered exported change must reach the full profile: %+v", p.Commands)
		}
	})

	t.Run("tests_for edge present does not raise the gap reason", func(t *testing.T) {
		t.Parallel()
		in := mk()
		in.Graph.Hits = []GraphHit{
			{Kind: "tests_for", Target: "PersistReceipt", FilePath: "pkg/store/store_test.go"},
		}
		_, reasons := planReasons(t, in)
		if strings.Contains(reasons, reason) {
			t.Fatalf("a present tests_for edge must not raise the gap reason, got %q", reasons)
		}
	})

	t.Run("gap reason needs a usable index", func(t *testing.T) {
		t.Parallel()
		in := mk()
		in.Graph.BuiltAtCommit = "" // unavailable: absence proves nothing at all
		_, reasons := planReasons(t, in)
		if strings.Contains(reasons, reason) {
			t.Fatalf("an unavailable index must not be reported as a tests_for gap, got %q", reasons)
		}
	})
}

// TestPlan_ForceEscalateBroadensRatherThanNarrows covers the BLOCKED path:
// when graph integrity cannot be proven the caller must still get broadened
// verification, never an empty or narrowed targeted plan.
func TestPlan_ForceEscalateBroadensRatherThanNarrows(t *testing.T) {
	t.Parallel()
	// Docs-only change: nothing in the change set would escalate on its own.
	in := baseInput("docs/rfcs/RFC-001.md")
	plain, reasons := planReasons(t, in)
	if plain.Escalated {
		t.Fatalf("docs-only change should not escalate on its own, got %q", reasons)
	}

	in.ForceEscalate = "graph integrity unproven: index incomplete"
	forced, freasons := planReasons(t, in)
	if !forced.Escalated {
		t.Fatal("ForceEscalate must escalate")
	}
	if !strings.Contains(freasons, "index incomplete") {
		t.Fatalf("forced reason must be carried through, got %q", freasons)
	}
	if !hasStage(forced, StageFull) {
		t.Fatalf("forced escalation must emit the full profile: %+v", forced.Commands)
	}
	if len(forced.Commands) <= len(plain.Commands) {
		t.Fatalf("forced escalation must broaden (%d) beyond the targeted plan (%d)",
			len(forced.Commands), len(plain.Commands))
	}
}

// TestPlan_GraphAnchorSHADefaultsToBase pins that the new anchor field is
// opt-in: FAC-94 callers keep base-relative freshness, herd tests-for anchors
// on the candidate because candidate-only symbols cannot exist in a base index.
func TestPlan_GraphAnchorSHADefaultsToBase(t *testing.T) {
	t.Parallel()
	in := baseInput("pkg/store/store.go")

	if p, _ := planReasons(t, in); p.GraphState != GraphAvailable {
		t.Fatalf("default anchor must be BaseSHA, got %s", p.GraphState)
	}

	in.GraphAnchorSHA = in.CandidateSHA
	p, _ := planReasons(t, in)
	if p.GraphState != GraphStale {
		t.Fatalf("candidate anchor must mark a base-built index stale, got %s", p.GraphState)
	}

	in.Graph.BuiltAtCommit = in.CandidateSHA
	p, _ = planReasons(t, in)
	if p.GraphState != GraphAvailable {
		t.Fatalf("candidate-built index must be available under a candidate anchor, got %s", p.GraphState)
	}
}

// TestPlan_PathsWithSpacesStayOneArgvElement pins that a package directory
// containing a space is never tokenized into separate argv elements.
func TestPlan_PathsWithSpacesStayOneArgvElement(t *testing.T) {
	t.Parallel()
	p, _ := planReasons(t, baseInput("pkg/with space/thing.go"))
	found := false
	for _, c := range p.Commands {
		for _, a := range c.Argv {
			if a == "./pkg/with space" {
				found = true
			}
			if a == "./pkg/with" || a == "space" {
				t.Fatalf("argv was tokenized on the space: %q", c.Argv)
			}
		}
	}
	if !found {
		t.Fatalf("expected an exact ./pkg/with space element: %+v", p.Commands)
	}
}
