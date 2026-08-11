// Package criticalpath computes the longest dependency chain and serial
// fraction of a task graph, yielding Amdahl speedup estimates before any
// agent is dispatched.
//
// The "and then" test from graph engineering: every EdgeBlocks is a real
// edge (the dependent reads the blocker's output); everything else is
// parallelizable. The longest chain of real edges is the critical path —
// the fastest this work can ever finish. The serial fraction is the ratio
// of sequential tasks to total, and Amdahl's law gives the speedup ceiling
// before a single agent runs.
package criticalpath

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/deps"
)

// Analysis is the complete critical-path + Amdahl assessment of a task graph.
type Analysis struct {
	// CriticalPath is the ordered list of refs forming the longest
	// EdgeBlocks chain. Empty when there are no edges.
	CriticalPath []string `json:"critical_path"`
	// CriticalPathLength is len(CriticalPath). When zero, there are no
	// real edges and every task is independent.
	CriticalPathLength int `json:"critical_path_length"`
	// TotalTasks is the count of distinct refs appearing in edges.
	TotalTasks int `json:"total_tasks"`
	// IndependentTasks is the count of tasks with no incoming EdgeBlocks
	// (no prerequisites). These can all fan out simultaneously.
	IndependentTasks int `json:"independent_tasks"`
	// SerialFraction is the ratio of the critical path length to the
	// total task count, clamped to [0, 1]. This is Amdahl's serial
	// fraction s: speedup = 1 / (s + (1-s)/n).
	SerialFraction float64 `json:"serial_fraction"`
}

// SpeedupEstimate is the Amdahl-predicted parallelism ceiling for a given
// agent count.
type SpeedupEstimate struct {
	Agents         int     `json:"agents"`
	SerialFraction float64 `json:"serial_fraction"`
	// Speedup is the Amdahl speedup factor: 1 / (s + (1-s)/n).
	// A value of 5.4 means the fleet finishes 5.4x faster than a single
	// agent on the same work.
	Speedup float64 `json:"speedup"`
	// Efficiency is Speedup / Agents, clamped to [0, 1]. Diminishing
	// returns from the serial tail are visible here.
	Efficiency float64 `json:"efficiency"`
}

// Analyze computes the critical path and serial fraction of a task graph
// from a set of dependency edges. Only EdgeBlocks edges are real edges;
// EdgeRelated edges are not dependencies and do not constrain ordering.
//
// The graph is treated as a DAG. If a cycle is detected, Analyze returns
// an error — a cyclic graph has no well-defined critical path.
func Analyze(edges []deps.DependencyEdge) (*Analysis, error) {
	blocks, err := filterBlocks(edges)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return &Analysis{
			CriticalPath:        []string{},
			CriticalPathLength:  0,
			TotalTasks:          0,
			IndependentTasks:    0,
			SerialFraction:      0,
		}, nil
	}

	refs := collectRefs(blocks)
	adj, inDeg := buildAdjacency(blocks, refs)

	if cycle := detectCycle(refs, adj, inDeg); cycle != nil {
		return nil, fmt.Errorf("criticalpath: cyclic dependency detected: %s", strings.Join(cycle, " -> "))
	}

	cp, cpLen := longestPath(refs, adj, inDeg)
	total := len(refs)
	independent := countIndependent(refs, inDeg)

	var serialFraction float64
	if total > 0 {
		serialFraction = float64(cpLen) / float64(total)
	}
	if serialFraction > 1 {
		serialFraction = 1
	}

	return &Analysis{
		CriticalPath:        cp,
		CriticalPathLength:  cpLen,
		TotalTasks:          total,
		IndependentTasks:    independent,
		SerialFraction:      serialFraction,
	}, nil
}

// EstimateSpeedup computes the Amdahl speedup for a given agent count and
// serial fraction. The formula: speedup = 1 / (s + (1-s)/n).
//
// Amdahl's law is the ceiling: even 256 agents at 95% serial fraction
// only reach ~18.6x. The critical path is the floor; the serial fraction
// is the cap.
func EstimateSpeedup(serialFraction float64, agents int) SpeedupEstimate {
	if agents <= 0 {
		agents = 1
	}
	s := serialFraction
	if s < 0 {
		s = 0
	}
	if s > 1 {
		s = 1
	}
	speedup := 1.0 / (s + (1.0-s)/float64(agents))
	efficiency := speedup / float64(agents)
	if efficiency > 1 {
		efficiency = 1
	}
	return SpeedupEstimate{
		Agents:         agents,
		SerialFraction: s,
		Speedup:        roundToTwo(speedup),
		Efficiency:     roundToTwo(efficiency),
	}
}

// EstimateSpeedupTable computes Amdahl speedup for a range of agent counts,
// letting the operator see the diminishing-returns curve before deploying.
func EstimateSpeedupTable(serialFraction float64, agentCounts []int) []SpeedupEstimate {
	out := make([]SpeedupEstimate, 0, len(agentCounts))
	for _, n := range agentCounts {
		out = append(out, EstimateSpeedup(serialFraction, n))
	}
	return out
}

// filterBlocks returns only EdgeBlocks edges — real dependencies where the
// dependent reads the blocker's output. EdgeRelated edges are not real
// edges and do not constrain ordering. Self-edges (source == target) are
// rejected as errors — a self-cycle is malformed dependency data, not a
// valid empty graph.
func filterBlocks(edges []deps.DependencyEdge) ([]deps.DependencyEdge, error) {
	var out []deps.DependencyEdge
	for _, e := range edges {
		if e.Type == deps.EdgeBlocks {
			src := deps.Ref(strings.TrimSpace(string(e.SourceRef)))
			tgt := deps.Ref(strings.TrimSpace(string(e.TargetRef)))
			if !src.Valid() || !tgt.Valid() {
				return nil, fmt.Errorf("criticalpath: invalid edge with empty source or target ref: %s -> %s", src, tgt)
			}
			if src == tgt {
				return nil, fmt.Errorf("criticalpath: self-cycle rejected: %s -> %s", src, tgt)
			}
			out = append(out, deps.DependencyEdge{
				SourceRef: src,
				TargetRef: tgt,
				Type:      deps.EdgeBlocks,
			})
		}
	}
	return out, nil
}

// collectRefs returns the sorted, deduplicated set of refs from edges.
func collectRefs(edges []deps.DependencyEdge) []string {
	seen := map[string]bool{}
	for _, e := range edges {
		seen[string(e.SourceRef)] = true
		seen[string(e.TargetRef)] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// buildAdjacency builds the adjacency list (blocker -> dependents) and
// in-degree map (dependent -> count of blockers).
func buildAdjacency(edges []deps.DependencyEdge, refs []string) (map[string][]string, map[string]int) {
	adj := make(map[string][]string)
	inDeg := make(map[string]int)
	for _, r := range refs {
		adj[r] = nil
		inDeg[r] = 0
	}
	for _, e := range edges {
		adj[string(e.SourceRef)] = append(adj[string(e.SourceRef)], string(e.TargetRef))
		inDeg[string(e.TargetRef)]++
	}
	for _, deps := range adj {
		sort.Strings(deps)
	}
	return adj, inDeg
}

// detectCycle returns a cycle path if the graph has one, nil otherwise.
// Uses Kahn's algorithm: if not all nodes are processed, a cycle exists.
func detectCycle(refs []string, adj map[string][]string, inDeg map[string]int) []string {
	deg := make(map[string]int, len(inDeg))
	for k, v := range inDeg {
		deg[k] = v
	}
	queue := []string{}
	for _, r := range refs {
		if deg[r] == 0 {
			queue = append(queue, r)
		}
	}
	processed := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		processed++
		for _, dep := range adj[n] {
			deg[dep]--
			if deg[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	if processed == len(refs) {
		return nil
	}

	remaining := map[string]bool{}
	for _, r := range refs {
		if deg[r] > 0 {
			remaining[r] = true
		}
	}
	var cycle []string
	for _, r := range refs {
		if remaining[r] {
			cycle = append(cycle, r)
		}
	}
	return cycle
}

// longestPath computes the longest chain in the DAG using topological
// order + dynamic programming. Returns the path (ordered refs) and its
// length (node count, not edge count).
func longestPath(refs []string, adj map[string][]string, inDeg map[string]int) ([]string, int) {
	deg := make(map[string]int, len(inDeg))
	for k, v := range inDeg {
		deg[k] = v
	}
	queue := []string{}
	for _, r := range refs {
		if deg[r] == 0 {
			queue = append(queue, r)
		}
	}

	dist := make(map[string]int)
	prev := make(map[string]string)
	for _, r := range refs {
		dist[r] = 1
		prev[r] = ""
	}

	var topoOrder []string
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		topoOrder = append(topoOrder, n)
		for _, dep := range adj[n] {
			if dist[n]+1 > dist[dep] {
				dist[dep] = dist[n] + 1
				prev[dep] = n
			}
			deg[dep]--
			if deg[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	maxDist := 0
	maxNode := ""
	for _, r := range refs {
		if dist[r] > maxDist {
			maxDist = dist[r]
			maxNode = r
		}
	}
	if maxNode == "" {
		return []string{}, 0
	}

	path := []string{}
	cur := maxNode
	for cur != "" {
		path = append([]string{cur}, path...)
		cur = prev[cur]
	}
	return path, len(path)
}

// countIndependent returns the number of tasks with no incoming edges
// (no prerequisites). These can all fan out simultaneously.
func countIndependent(refs []string, inDeg map[string]int) int {
	count := 0
	for _, r := range refs {
		if inDeg[r] == 0 {
			count++
		}
	}
	return count
}

func roundToTwo(n float64) float64 {
	return math.Round(n*100) / 100
}
