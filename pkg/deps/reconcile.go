package deps

import (
	"fmt"
	"sort"
	"strings"
)

// Reconcile compares desired structured edges against managed board edges for
// a dependent task. Board edges that are blocks where target == taskRef are
// inbound prerequisites; outbound blocks (task blocks others) are also managed
// when present in desired.
//
// Findings are sorted stably by (class, source_ref, target_ref, detail).
func Reconcile(taskRef Ref, desired, board []DependencyEdge) *ReconcileReport {
	taskRef = Ref(strings.TrimSpace(string(taskRef)))
	rep := &ReconcileReport{
		TaskRef:  taskRef,
		Desired:  compactEdges(desired),
		ManagedBoard: compactEdges(filterInvolving(board, taskRef)),
		OK:       true,
	}

	// Index board blocks by key; track duplicates and unresolved.
	boardByKey := map[string][]DependencyEdge{}
	boardBlocks := []DependencyEdge{}
	for _, e := range rep.ManagedBoard {
		if e.Type != EdgeBlocks {
			continue
		}
		if !e.SourceRef.Valid() || !e.TargetRef.Valid() {
			rep.Findings = append(rep.Findings, Finding{
				Class:  DriftUnresolved,
				Edge:   e,
				Detail: "board edge missing source_ref or target_ref",
			})
			continue
		}
		if e.SourceID != "" && !e.SourceID.Valid() {
			rep.Findings = append(rep.Findings, Finding{
				Class:  DriftUnresolved,
				Edge:   e,
				Detail: "board edge has empty source_id",
			})
		}
		boardByKey[e.Key()] = append(boardByKey[e.Key()], e)
		boardBlocks = append(boardBlocks, e)
	}

	for k, group := range boardByKey {
		if len(group) > 1 {
			for _, e := range group[1:] {
				rep.Findings = append(rep.Findings, Finding{
					Class:      DriftDuplicate,
					Edge:       e,
					Detail:     "duplicate board edge " + k,
					RelationID: e.RelationID,
				})
			}
		}
	}

	// Desired blocks must exist on board (exact direction).
	desiredByKey := map[string]DependencyEdge{}
	for _, e := range rep.Desired {
		if e.Type != EdgeBlocks {
			continue
		}
		if !e.SourceRef.Valid() || !e.TargetRef.Valid() {
			rep.Findings = append(rep.Findings, Finding{
				Class:  DriftUnresolved,
				Edge:   e,
				Detail: "desired edge missing source_ref or target_ref",
			})
			continue
		}
		if e.SourceRef == e.TargetRef {
			rep.Findings = append(rep.Findings, Finding{
				Class:  DriftUnresolved,
				Edge:   e,
				Detail: "self-edge rejected",
			})
			continue
		}
		k := e.Key()
		if _, dup := desiredByKey[k]; dup {
			rep.Findings = append(rep.Findings, Finding{
				Class:  DriftDuplicate,
				Edge:   e,
				Detail: "duplicate desired edge " + k,
			})
			continue
		}
		desiredByKey[k] = e
		if _, ok := boardByKey[k]; !ok {
			// Reversed present?
			if rev, rok := boardByKey[e.ReverseKey()]; rok && len(rev) > 0 {
				rep.Findings = append(rep.Findings, Finding{
					Class:      DriftReversed,
					Edge:       e,
					Detail:     fmt.Sprintf("desired %s→%s but board has reverse", e.SourceRef, e.TargetRef),
					RelationID: rev[0].RelationID,
				})
			} else {
				rep.Findings = append(rep.Findings, Finding{
					Class:  DriftMissing,
					Edge:   e,
					Detail: fmt.Sprintf("desired blocks %s→%s absent on board", e.SourceRef, e.TargetRef),
				})
			}
		}
	}

	// Board blocks involving task that are not in desired (when desired non-empty)
	// are "extra" managed edges needing matching provenance.
	if len(desiredByKey) > 0 {
		for _, e := range boardBlocks {
			if _, ok := desiredByKey[e.Key()]; !ok {
				// Skip if it's the reverse of a desired (already reported reversed).
				if _, rev := desiredByKey[e.ReverseKey()]; rev {
					continue
				}
				rep.Findings = append(rep.Findings, Finding{
					Class:      DriftExtra,
					Edge:       e,
					Detail:     fmt.Sprintf("board blocks %s→%s has no structured provenance", e.SourceRef, e.TargetRef),
					RelationID: e.RelationID,
				})
			}
		}
	}

	// Cycles among the union of desired + full board blocks (not only edges
	// involving taskRef — cycles may close through third-party nodes).
	allBoardBlocks := make([]DependencyEdge, 0, len(board))
	for _, e := range board {
		if e.Type == EdgeBlocks {
			allBoardBlocks = append(allBoardBlocks, e)
		}
	}
	if cycle := detectCycle(append(append([]DependencyEdge{}, allBoardBlocks...), edgesFromMap(desiredByKey)...)); cycle != "" {
		rep.Findings = append(rep.Findings, Finding{
			Class:  DriftCyclic,
			Edge:   DependencyEdge{Type: EdgeBlocks, SourceRef: Ref(cycle), TargetRef: taskRef},
			Detail: "cycle: " + cycle,
		})
	}

	sortFindings(rep.Findings)
	rep.OK = len(rep.Findings) == 0
	rep.GraphRevision = GraphRevision(boardBlocks, nil)
	return rep
}

func edgesFromMap(m map[string]DependencyEdge) []DependencyEdge {
	out := make([]DependencyEdge, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	return out
}

func filterInvolving(edges []DependencyEdge, task Ref) []DependencyEdge {
	var out []DependencyEdge
	for _, e := range edges {
		if e.SourceRef == task || e.TargetRef == task {
			out = append(out, e)
		}
	}
	return out
}

func compactEdges(edges []DependencyEdge) []DependencyEdge {
	if len(edges) == 0 {
		return nil
	}
	out := append([]DependencyEdge(nil), edges...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		if out[i].SourceRef != out[j].SourceRef {
			return out[i].SourceRef < out[j].SourceRef
		}
		return out[i].TargetRef < out[j].TargetRef
	})
	return out
}

func sortFindings(f []Finding) {
	rank := map[DriftClass]int{
		DriftMissing: 1, DriftExtra: 2, DriftDuplicate: 3,
		DriftReversed: 4, DriftUnresolved: 5, DriftDeleted: 6, DriftCyclic: 7,
	}
	sort.SliceStable(f, func(i, j int) bool {
		ri, rj := rank[f[i].Class], rank[f[j].Class]
		if ri != rj {
			return ri < rj
		}
		if f[i].Edge.SourceRef != f[j].Edge.SourceRef {
			return f[i].Edge.SourceRef < f[j].Edge.SourceRef
		}
		if f[i].Edge.TargetRef != f[j].Edge.TargetRef {
			return f[i].Edge.TargetRef < f[j].Edge.TargetRef
		}
		return f[i].Detail < f[j].Detail
	})
}

// detectCycle returns a human cycle path string or "" if acyclic.
// Edges are directed Source → Target meaning Source blocks Target
// (edge direction for DFS: Target depends on Source, walk Source→Target).
func detectCycle(edges []DependencyEdge) string {
	adj := map[string][]string{}
	nodes := map[string]bool{}
	for _, e := range edges {
		if e.Type != EdgeBlocks {
			continue
		}
		s, t := string(e.SourceRef), string(e.TargetRef)
		if s == "" || t == "" {
			continue
		}
		adj[s] = append(adj[s], t)
		nodes[s] = true
		nodes[t] = true
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycleStart string
	var dfs func(u string) bool
	dfs = func(u string) bool {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range adj[u] {
			switch color[v] {
			case white:
				if dfs(v) {
					return true
				}
			case gray:
				cycleStart = v
				stack = append(stack, v)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return false
	}
	// Stable node order.
	var order []string
	for n := range nodes {
		order = append(order, n)
	}
	sort.Strings(order)
	for _, n := range order {
		if color[n] == white {
			if dfs(n) {
				// Extract cycle from stack.
				idx := 0
				for i, s := range stack {
					if s == cycleStart {
						idx = i
						break
					}
				}
				return strings.Join(stack[idx:], " → ")
			}
		}
	}
	return ""
}

// InboundBlockers returns source refs of blocks edges targeting dependent.
func InboundBlockers(board []DependencyEdge, dependent Ref) []Ref {
	var out []Ref
	seen := map[Ref]bool{}
	for _, e := range board {
		if e.Type != EdgeBlocks {
			continue
		}
		if e.TargetRef == dependent && e.SourceRef.Valid() && !seen[e.SourceRef] {
			seen[e.SourceRef] = true
			out = append(out, e.SourceRef)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
