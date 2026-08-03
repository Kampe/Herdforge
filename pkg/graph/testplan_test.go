package graph

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func baseInput(paths ...string) PlanInput {
	return PlanInput{
		BaseSHA:      "baseaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CandidateSHA: "candbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ChangedPaths: paths,
		Graph: GraphEvidence{
			BuiltAtCommit: "baseaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Profile: DefaultGoProfile(),
	}
}

func TestPlan_DeterministicSameInputSameJSON(t *testing.T) {
	// Same change set in different path/symbol/hit order must yield byte-identical plans.
	mk := func() PlanInput {
		in := baseInput(
			"pkg/graph/testplan.go",
			"pkg/config/config.go",
			"pkg/graph/graph.go",
		)
		in.ChangedSymbols = []string{"Plan", "DefaultGoProfile", "WorkspaceGraph"}
		in.Graph.Hits = []GraphHit{
			{Kind: "tests_for", Target: "pkg/graph/graph.go", FilePath: "pkg/graph/graph_test.go"},
			{Kind: "importers_of", Target: "pkg/graph/graph.go", FilePath: "pkg/daemon/daemon.go"},
			{Kind: "tests_for", Target: "pkg/config/config.go", FilePath: "pkg/config/config_test.go"},
			{Kind: "callers_of", Target: "Plan", FilePath: "pkg/dispatch/dispatch.go"},
		}
		return in
	}

	// Permute inputs.
	a := mk()
	b := mk()
	b.ChangedPaths = []string{"pkg/graph/graph.go", "pkg/graph/testplan.go", "pkg/config/config.go"}
	b.ChangedSymbols = []string{"WorkspaceGraph", "Plan", "DefaultGoProfile"}
	b.Graph.Hits = []GraphHit{
		b.Graph.Hits[3], b.Graph.Hits[1], b.Graph.Hits[0], b.Graph.Hits[2],
	}

	pa, err := Plan(a)
	if err != nil {
		t.Fatalf("plan a: %v", err)
	}
	pb, err := Plan(b)
	if err != nil {
		t.Fatalf("plan b: %v", err)
	}

	ja, err := json.Marshal(pa)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(pb)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ja, jb) {
		t.Fatalf("plans not byte-identical:\nA: %s\nB: %s", ja, jb)
	}

	// Second call with identical input also stable.
	pa2, err := Plan(a)
	if err != nil {
		t.Fatal(err)
	}
	ja2, _ := json.Marshal(pa2)
	if !bytes.Equal(ja, ja2) {
		t.Fatal("repeated Plan() on same input is not stable")
	}
}

func TestPlan_OwnerPackagesTargeted(t *testing.T) {
	in := baseInput("pkg/graph/graph.go", "pkg/config/config.go")
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GraphState != GraphAvailable {
		t.Fatalf("graph state = %s, want available", plan.GraphState)
	}
	wantPkgs := []string{"./pkg/config", "./pkg/graph"}
	if !reflect.DeepEqual(plan.ChangedPackages, wantPkgs) {
		t.Fatalf("packages = %v, want %v", plan.ChangedPackages, wantPkgs)
	}

	// Must include exact package test argv (paths with no shell split).
	foundGraphTest := false
	foundConfigTest := false
	for _, c := range plan.Commands {
		if c.Stage != StageTest || c.Source != "owner" {
			continue
		}
		if reflect.DeepEqual(c.Argv, []string{"go", "test", "-count=1", "./pkg/graph"}) {
			foundGraphTest = true
		}
		if reflect.DeepEqual(c.Argv, []string{"go", "test", "-count=1", "./pkg/config"}) {
			foundConfigTest = true
		}
	}
	if !foundGraphTest || !foundConfigTest {
		t.Fatalf("missing owner package tests: commands=%+v", plan.Commands)
	}
}

func TestPlan_QuotedArgvPreserved(t *testing.T) {
	// Paths with spaces must remain single argv elements.
	in := baseInput("pkg/graph/graph.go")
	in.Profile.PackageTest = []string{"go", "test", "-count=1", "{package}", "-run", "Test Plan Space"}
	in.Profile.Blackbox = [][]string{{"bin/check surface", "--path", "dir with spaces/file"}}
	in.ChangedSymbols = []string{"Plan"} // public → blackbox when configured
	// Avoid escalation noise from public symbols path alone by keeping graph fresh
	// and only one non-auth path — still hasPublicBehavior true.

	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	var sawPackage bool
	var sawBlackbox bool
	for _, c := range plan.Commands {
		if c.Stage == StageTest && c.Source == "owner" {
			if len(c.Argv) != 6 {
				t.Fatalf("package test argv split incorrectly: %#v", c.Argv)
			}
			if c.Argv[5] != "Test Plan Space" {
				t.Fatalf("spaced test name not preserved: %#v", c.Argv)
			}
			sawPackage = true
		}
		if c.Stage == StageBlackbox {
			if c.Argv[0] != "bin/check surface" {
				t.Fatalf("spaced binary name not preserved: %#v", c.Argv)
			}
			if c.Argv[2] != "dir with spaces/file" {
				t.Fatalf("spaced path not preserved: %#v", c.Argv)
			}
			sawBlackbox = true
		}
	}
	if !sawPackage {
		t.Fatal("expected owner package test with spaced -run arg")
	}
	if !sawBlackbox {
		t.Fatal("expected blackbox command with spaced argv elements")
	}
}

func TestPlan_GraphTestsForAndConsumers(t *testing.T) {
	in := baseInput("pkg/graph/graph.go")
	in.Graph.Hits = []GraphHit{
		// tests_for in a different package (e.g. integration coverage) must be kept
		{Kind: "tests_for", Target: "pkg/graph/graph.go", FilePath: "pkg/overlap/overlap_test.go"},
		// same-package tests_for is covered by owner and deduped (not double-emitted)
		{Kind: "tests_for", Target: "pkg/graph/graph.go", FilePath: "pkg/graph/graph_test.go"},
		// importer outside owner package
		{Kind: "importers_of", Target: "pkg/graph/graph.go", FilePath: "pkg/daemon/x.go"},
		// same-package importer must not become a consumer suite
		{Kind: "importers_of", Target: "pkg/graph/graph.go", FilePath: "pkg/graph/other.go"},
	}
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	var graphTest, consumer, ownerTest bool
	graphTestCount := 0
	for _, c := range plan.Commands {
		if c.Source == "graph-tests_for" && reflect.DeepEqual(c.Argv, []string{"go", "test", "-count=1", "./pkg/overlap"}) {
			graphTest = true
		}
		if c.Source == "graph-consumer" && reflect.DeepEqual(c.Argv, []string{"go", "test", "-count=1", "./pkg/daemon"}) {
			consumer = true
		}
		if c.Source == "owner" && reflect.DeepEqual(c.Argv, []string{"go", "test", "-count=1", "./pkg/graph"}) {
			ownerTest = true
		}
		if reflect.DeepEqual(c.Argv, []string{"go", "test", "-count=1", "./pkg/graph"}) {
			graphTestCount++
		}
	}
	if !graphTest {
		t.Fatalf("expected graph-tests_for for overlap, got %+v", plan.Commands)
	}
	if !consumer {
		t.Fatalf("expected graph-consumer for daemon, got %+v", plan.Commands)
	}
	if !ownerTest {
		t.Fatalf("expected owner package test for graph, got %+v", plan.Commands)
	}
	if graphTestCount != 1 {
		t.Fatalf("owner+same-package tests_for must dedupe to 1, got %d", graphTestCount)
	}
}

func TestPlan_MissingGraphDoesNotFabricateHits(t *testing.T) {
	in := baseInput("pkg/graph/graph.go")
	in.Graph = GraphEvidence{
		// Hits present but BuiltAtCommit empty → unavailable; hits must be ignored.
		Hits: []GraphHit{
			{Kind: "tests_for", Target: "x", FilePath: "pkg/other/other_test.go"},
		},
	}
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GraphState != GraphMissing {
		t.Fatalf("state = %s, want unavailable", plan.GraphState)
	}
	for _, c := range plan.Commands {
		if strings.HasPrefix(c.Source, "graph-") {
			t.Fatalf("must not use graph sources when unavailable: %+v", c)
		}
	}
	// Production code without graph → escalated.
	if !plan.Escalated {
		t.Fatal("expected escalation when production code changes without graph")
	}
}

func TestPlan_StaleGraphFailClosedWhenRequired(t *testing.T) {
	in := baseInput("pkg/graph/graph.go")
	in.Graph.BuiltAtCommit = "stalezzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	in.Profile.RequireFreshGraph = true
	_, err := Plan(in)
	if err == nil {
		t.Fatal("expected error for stale graph with RequireFreshGraph")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error should mention stale: %v", err)
	}
}

func TestPlan_MissingGraphFailClosedWhenRequired(t *testing.T) {
	in := baseInput("pkg/graph/graph.go")
	in.Graph = GraphEvidence{}
	in.Profile.RequireFreshGraph = true
	_, err := Plan(in)
	if err == nil {
		t.Fatal("expected error for missing graph with RequireFreshGraph")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error should mention unavailable: %v", err)
	}
}

func TestPlan_UnsupportedLanguageFailClosed(t *testing.T) {
	in := baseInput("pkg/graph/graph.go")
	in.Profile.Language = Language("cobol")
	_, err := Plan(in)
	if err == nil {
		t.Fatal("expected error for unsupported language")
	}
}

func TestPlan_EmptyProfileTestsFailClosed(t *testing.T) {
	in := baseInput("pkg/graph/graph.go")
	in.Profile = VerificationProfile{Language: LangGo}
	_, err := Plan(in)
	if err == nil {
		t.Fatal("expected error when profile has no test commands")
	}
}

func TestPlan_MissingSHAFailClosed(t *testing.T) {
	_, err := Plan(PlanInput{CandidateSHA: "x", Profile: DefaultGoProfile()})
	if err == nil {
		t.Fatal("expected error for empty base_sha")
	}
	_, err = Plan(PlanInput{BaseSHA: "x", Profile: DefaultGoProfile()})
	if err == nil {
		t.Fatal("expected error for empty candidate_sha")
	}
}

func TestPlan_AuthPathEscalatesWithBlackbox(t *testing.T) {
	in := baseInput("pkg/security/auth.go")
	in.Profile.Blackbox = [][]string{{"go", "test", "-count=1", "./pkg/selftest/"}}
	in.Profile.Full = [][]string{{"go", "test", "-count=1", "./..."}}
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Escalated {
		t.Fatal("auth path must escalate")
	}
	var full, blackbox bool
	for _, c := range plan.Commands {
		if c.Stage == StageFull && c.Source == "escalation" {
			full = true
		}
		if c.Stage == StageBlackbox && c.Source == "escalation" {
			blackbox = true
		}
	}
	if !full {
		t.Fatal("expected escalated full suite")
	}
	if !blackbox {
		t.Fatal("expected escalated blackbox for auth")
	}
}

func TestPlan_DocsOnlyNoPackageNoEscalate(t *testing.T) {
	in := baseInput("docs/architecture/TARGET-WORKFLOW.md", "README.md")
	in.Profile = VerificationProfile{
		Language: LangDocs,
		// docs profiles may have empty test; allow empty
	}
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ChangedPackages) != 0 {
		t.Fatalf("docs-only should have no packages: %v", plan.ChangedPackages)
	}
	if plan.Escalated {
		t.Fatalf("docs-only must not escalate: %v", plan.EscalationReasons)
	}
}

func TestPlan_DedupCommands(t *testing.T) {
	in := baseInput("pkg/graph/graph.go", "pkg/graph/graph_test.go")
	// Hits that resolve to the same package test as owner.
	in.Graph.Hits = []GraphHit{
		{Kind: "tests_for", Target: "pkg/graph/graph.go", FilePath: "pkg/graph/graph_test.go"},
		{Kind: "tests_for", Target: "pkg/graph/graph.go", FilePath: "pkg/graph/graph_extended_test.go"},
	}
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, c := range plan.Commands {
		if reflect.DeepEqual(c.Argv, []string{"go", "test", "-count=1", "./pkg/graph"}) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 deduped go test ./pkg/graph, got %d in %+v", count, plan.Commands)
	}
}

func TestPlan_AbsolutePathsDropped(t *testing.T) {
	in := baseInput("/Users/someone/pkg/graph/graph.go", "pkg/config/config.go")
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plan.ChangedPaths {
		if strings.HasPrefix(p, "/") {
			t.Fatalf("absolute path leaked into plan: %s", p)
		}
	}
	if !reflect.DeepEqual(plan.ChangedPackages, []string{"./pkg/config"}) {
		t.Fatalf("packages = %v", plan.ChangedPackages)
	}
}

func TestPlan_ValidFor(t *testing.T) {
	in := baseInput("pkg/graph/graph.go")
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ValidFor(in.BaseSHA, in.CandidateSHA) {
		t.Fatal("ValidFor should accept original SHAs")
	}
	if plan.ValidFor(in.BaseSHA, "other") {
		t.Fatal("ValidFor must reject candidate mismatch")
	}
	plan.PlannerVersion = "0.0.0"
	if plan.ValidFor(in.BaseSHA, in.CandidateSHA) {
		t.Fatal("ValidFor must reject planner version mismatch")
	}
}

func TestPackageForPath_Go(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"pkg/graph/graph.go", "./pkg/graph"},
		{"pkg/graph/graph_test.go", "./pkg/graph"},
		{"cmd/herd/main.go", "./cmd/herd"},
		{"README.md", ""},
		{"pkg/graph/testdata/x.go", ""},
		{"./pkg/config/config.go", "./pkg/config"},
	}
	for _, tc := range cases {
		got := packageForPath(tc.path, LangGo)
		if got != tc.want {
			t.Errorf("packageForPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestPlan_MutationNonVacuity_DropSortBreaksDeterminism(t *testing.T) {
	// Non-vacuity guard: if ChangedPaths were not sorted, permuted inputs
	// would produce different JSON. We assert the invariant holds; a
	// regression that removes sorting fails this test.
	mk := func(paths []string) []byte {
		in := baseInput(paths...)
		p, err := Plan(in)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(p.ChangedPackages)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a := mk([]string{"pkg/z/z.go", "pkg/a/a.go", "pkg/m/m.go"})
	b := mk([]string{"pkg/m/m.go", "pkg/z/z.go", "pkg/a/a.go"})
	if !bytes.Equal(a, b) {
		t.Fatalf("package order not deterministic: %s vs %s", a, b)
	}
	// Explicit order expectation.
	if string(a) != `["./pkg/a","./pkg/m","./pkg/z"]` {
		t.Fatalf("unexpected package order: %s", a)
	}
}

func TestPlan_StageOrderStable(t *testing.T) {
	in := baseInput("pkg/security/token.go")
	in.Profile.Lint = [][]string{{"go", "vet", "./..."}}
	in.Profile.Mutation = [][]string{{"go", "test", "-count=1", "./pkg/security/"}}
	in.Profile.Blackbox = [][]string{{"make", "self-test"}}
	in.Profile.Full = [][]string{{"go", "test", "-count=1", "./..."}}
	plan, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	var ranks []int
	for _, c := range plan.Commands {
		ranks = append(ranks, stageRank(c.Stage))
	}
	for i := 1; i < len(ranks); i++ {
		if ranks[i] < ranks[i-1] {
			t.Fatalf("stages not non-decreasing: ranks=%v commands=%+v", ranks, plan.Commands)
		}
	}
}
