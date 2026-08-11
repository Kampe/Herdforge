package criticalpath

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/deps"
)

func edge(src, tgt string) deps.DependencyEdge {
	return deps.DependencyEdge{SourceRef: deps.Ref(src), TargetRef: deps.Ref(tgt), Type: deps.EdgeBlocks}
}

func relatedEdge(src, tgt string) deps.DependencyEdge {
	return deps.DependencyEdge{SourceRef: deps.Ref(src), TargetRef: deps.Ref(tgt), Type: deps.EdgeRelated}
}

func TestAnalyze_NoEdges(t *testing.T) {
	a, err := Analyze(nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.CriticalPathLength != 0 {
		t.Fatalf("expected 0 critical path, got %d", a.CriticalPathLength)
	}
	if a.TotalTasks != 0 {
		t.Fatalf("expected 0 total tasks, got %d", a.TotalTasks)
	}
	if a.SerialFraction != 0 {
		t.Fatalf("expected 0 serial fraction, got %f", a.SerialFraction)
	}
}

func TestAnalyze_AllIndependent(t *testing.T) {
	edges := []deps.DependencyEdge{
		relatedEdge("FAC-1", "FAC-2"),
		relatedEdge("FAC-3", "FAC-4"),
	}
	a, err := Analyze(edges)
	if err != nil {
		t.Fatal(err)
	}
	if a.CriticalPathLength != 0 {
		t.Fatalf("related edges should not form critical path; got length %d", a.CriticalPathLength)
	}
	if a.TotalTasks != 0 {
		t.Fatalf("no blocks edges means 0 total tasks; got %d", a.TotalTasks)
	}
}

func TestAnalyze_LinearChain(t *testing.T) {
	edges := []deps.DependencyEdge{
		edge("FAC-1", "FAC-2"),
		edge("FAC-2", "FAC-3"),
		edge("FAC-3", "FAC-4"),
	}
	a, err := Analyze(edges)
	if err != nil {
		t.Fatal(err)
	}
	if a.CriticalPathLength != 4 {
		t.Fatalf("expected critical path length 4, got %d", a.CriticalPathLength)
	}
	expected := []string{"FAC-1", "FAC-2", "FAC-3", "FAC-4"}
	for i, ref := range expected {
		if i >= len(a.CriticalPath) || a.CriticalPath[i] != ref {
			t.Fatalf("critical path[%d]: expected %s, got %v", i, ref, a.CriticalPath)
		}
	}
	if a.TotalTasks != 4 {
		t.Fatalf("expected 4 total tasks, got %d", a.TotalTasks)
	}
	if a.IndependentTasks != 1 {
		t.Fatalf("expected 1 independent task (FAC-1), got %d", a.IndependentTasks)
	}
	if a.SerialFraction != 1.0 {
		t.Fatalf("fully serial chain should have serial fraction 1.0, got %f", a.SerialFraction)
	}
}

func TestAnalyze_FanOutGraph(t *testing.T) {
	//    FAC-1
	//   /  |  \
	// FAC-2 FAC-3 FAC-4
	//   \  |  /
	//    FAC-5
	edges := []deps.DependencyEdge{
		edge("FAC-1", "FAC-2"),
		edge("FAC-1", "FAC-3"),
		edge("FAC-1", "FAC-4"),
		edge("FAC-2", "FAC-5"),
		edge("FAC-3", "FAC-5"),
		edge("FAC-4", "FAC-5"),
	}
	a, err := Analyze(edges)
	if err != nil {
		t.Fatal(err)
	}
	if a.CriticalPathLength != 3 {
		t.Fatalf("expected critical path length 3 (A->B->E), got %d", a.CriticalPathLength)
	}
	if a.TotalTasks != 5 {
		t.Fatalf("expected 5 total tasks, got %d", a.TotalTasks)
	}
	if a.IndependentTasks != 1 {
		t.Fatalf("expected 1 independent task (FAC-1), got %d", a.IndependentTasks)
	}
	expectedFraction := 3.0 / 5.0
	if a.SerialFraction != expectedFraction {
		t.Fatalf("expected serial fraction %f, got %f", expectedFraction, a.SerialFraction)
	}
}

func TestAnalyze_MixedBlocksAndRelated(t *testing.T) {
	edges := []deps.DependencyEdge{
		edge("FAC-1", "FAC-2"),
		relatedEdge("FAC-3", "FAC-4"),
		edge("FAC-2", "FAC-3"),
	}
	a, err := Analyze(edges)
	if err != nil {
		t.Fatal(err)
	}
	if a.CriticalPathLength != 3 {
		t.Fatalf("expected critical path 3 (FAC-1->FAC-2->FAC-3), got %d", a.CriticalPathLength)
	}
	if a.TotalTasks != 3 {
		t.Fatalf("related edges should not add to total; got %d", a.TotalTasks)
	}
}

func TestAnalyze_CycleDetected(t *testing.T) {
	edges := []deps.DependencyEdge{
		edge("FAC-1", "FAC-2"),
		edge("FAC-2", "FAC-3"),
		edge("FAC-3", "FAC-1"),
	}
	_, err := Analyze(edges)
	if err == nil {
		t.Fatal("expected cycle detection error, got nil")
	}
}

func TestAnalyze_SelfEdgeRejected(t *testing.T) {
	edges := []deps.DependencyEdge{
		edge("FAC-1", "FAC-1"),
	}
	_, err := Analyze(edges)
	if err == nil {
		t.Fatal("self-edges should be rejected with error, not silently discarded")
	}
}

func TestAnalyze_InvalidEdgeRejected(t *testing.T) {
	edges := []deps.DependencyEdge{
		{SourceRef: deps.Ref(""), TargetRef: deps.Ref("FAC-2"), Type: deps.EdgeBlocks},
	}
	_, err := Analyze(edges)
	if err == nil {
		t.Fatal("edges with empty source should be rejected with error")
	}
}

func TestEstimateSpeedup_FullyParallel(t *testing.T) {
	est := EstimateSpeedup(0, 16)
	if est.Speedup != 16 {
		t.Fatalf("0 serial fraction + 16 agents should give 16x, got %f", est.Speedup)
	}
	if est.Efficiency != 1.0 {
		t.Fatalf("fully parallel should have 100%% efficiency, got %f", est.Efficiency)
	}
}

func TestEstimateSpeedup_FullySerial(t *testing.T) {
	est := EstimateSpeedup(1.0, 16)
	if est.Speedup != 1.0 {
		t.Fatalf("fully serial should give 1x regardless of agents, got %f", est.Speedup)
	}
}

func TestEstimateSpeedup_AmdahlExample(t *testing.T) {
	// Article example: 95% parallel (5% serial), 16 agents
	// speedup = 1 / (0.05 + 0.95/16) = 1 / (0.05 + 0.059375) = 1 / 0.109375 ≈ 9.14
	est := EstimateSpeedup(0.05, 16)
	if est.Speedup < 9.0 || est.Speedup > 9.3 {
		t.Fatalf("expected ~9.14x for s=0.05 n=16, got %f", est.Speedup)
	}
}

func TestEstimateSpeedup_DiminishingReturns(t *testing.T) {
	s := 0.05
	est16 := EstimateSpeedup(s, 16)
	est256 := EstimateSpeedup(s, 256)
	if est256.Speedup <= est16.Speedup {
		t.Fatalf("more agents should not reduce speedup: 16=%f 256=%f", est16.Speedup, est256.Speedup)
	}
	if est256.Efficiency >= est16.Efficiency {
		t.Fatalf("efficiency should drop with more agents: 16=%f 256=%f", est16.Efficiency, est256.Efficiency)
	}
	// Article: 256 agents at 95% parallel only reaches ~18.6x
	if est256.Speedup < 18 || est256.Speedup > 19 {
		t.Fatalf("expected ~18.6x for s=0.05 n=256, got %f", est256.Speedup)
	}
}

func TestEstimateSpeedup_ZeroAgents(t *testing.T) {
	est := EstimateSpeedup(0.5, 0)
	if est.Agents != 1 {
		t.Fatalf("0 agents should clamp to 1, got %d", est.Agents)
	}
	if est.Speedup != 1.0 {
		t.Fatalf("1 agent should give 1x, got %f", est.Speedup)
	}
}

func TestEstimateSpeedupTable(t *testing.T) {
	table := EstimateSpeedupTable(0.1, []int{1, 4, 16, 64})
	if len(table) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(table))
	}
	for i, est := range table {
		if est.Speedup < 1.0 {
			t.Fatalf("entry %d: speedup should never be < 1, got %f", i, est.Speedup)
		}
	}
	if table[0].Speedup != 1.0 {
		t.Fatalf("1 agent should give 1x, got %f", table[0].Speedup)
	}
}

func TestAnalyze_DeterministicOnShuffledInput(t *testing.T) {
	edges := []deps.DependencyEdge{
		edge("FAC-3", "FAC-4"),
		edge("FAC-1", "FAC-2"),
		edge("FAC-2", "FAC-3"),
		edge("FAC-1", "FAC-3"),
	}
	a1, err := Analyze(edges)
	if err != nil {
		t.Fatal(err)
	}

	shuffled := []deps.DependencyEdge{
		edge("FAC-1", "FAC-3"),
		edge("FAC-2", "FAC-3"),
		edge("FAC-1", "FAC-2"),
		edge("FAC-3", "FAC-4"),
	}
	a2, err := Analyze(shuffled)
	if err != nil {
		t.Fatal(err)
	}

	if a1.CriticalPathLength != a2.CriticalPathLength {
		t.Fatalf("critical path length should be deterministic: %d vs %d", a1.CriticalPathLength, a2.CriticalPathLength)
	}
	if len(a1.CriticalPath) != len(a2.CriticalPath) {
		t.Fatalf("critical path should be deterministic in length")
	}
	for i := range a1.CriticalPath {
		if a1.CriticalPath[i] != a2.CriticalPath[i] {
			t.Fatalf("critical path[%d]: %s vs %s — not deterministic", i, a1.CriticalPath[i], a2.CriticalPath[i])
		}
	}
}
