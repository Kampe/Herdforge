package router

import "testing"

func TestSelectHelpRouteUsesNarrowCapableHelper(t *testing.T) {
	route, err := SelectHelpRoute(HelpKindReview, []HelpCandidate{
		{Identity: "fleet", Capabilities: []string{"review", "merge"}, Available: true},
		{Identity: "review-supervisor", Family: "google", Capabilities: []string{"review"}, Available: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Target != "review-supervisor" || route.Escalated {
		t.Fatalf("route=%+v, want narrow non-escalated review helper", route)
	}
}

func TestSelectHelpRouteEscalatesOnlyWithoutCapableHelper(t *testing.T) {
	route, err := SelectHelpRoute(HelpKindImplementation, []HelpCandidate{
		{Identity: "review-supervisor", Capabilities: []string{"review"}, Available: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Target != "fleet" || !route.Escalated {
		t.Fatalf("route=%+v, want bounded fleet escalation", route)
	}
}

func TestSelectHelpRouteIsDeterministic(t *testing.T) {
	route, err := SelectHelpRoute(HelpKindMerge, []HelpCandidate{
		{Identity: "z-coordinator", Capabilities: []string{"merge"}, Available: true},
		{Identity: "a-coordinator", Capabilities: []string{"merge"}, Available: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Target != "a-coordinator" {
		t.Fatalf("route target=%q, want deterministic lexical winner", route.Target)
	}
}
