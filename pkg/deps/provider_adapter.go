package deps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// ProviderStore adapts a TaskProvider (+ optional RelationProvider) to RelationStore.
//
// Live-path rules (FAC-159 audit cruhqfqizmzmmaa51i9cmigi):
//   - SnapshotGraph prefers BulkRelationProvider.ListProjectRelations (bounded
//     concurrent fan-out / single bulk RPC) — never sequential per-task CLI.
//   - One ListTasks hydration per SnapshotFence (not per TaskStatus).
//   - Immutable snapshot reuse within a fence; Invalidate for final TOCTOU.
//   - No long-lived unsafe cache across fences.
type ProviderStore struct {
	mu        sync.Mutex
	TP        provider.TaskProvider
	ProjectID string
	refCache  map[string]*provider.Task
	idCache   map[string]*provider.Task

	// Test counters (optional observability for bounded-call tests).
	ListTasksCalls     atomic.Int64
	ListRelCalls       atomic.Int64 // per-task ListRelations
	BulkRelCalls       atomic.Int64 // ListProjectRelations
	SnapshotGraphCalls atomic.Int64
}

// NewProviderStore wraps a TaskProvider. Relation capability is explicit.
func NewProviderStore(tp provider.TaskProvider, projectID string) *ProviderStore {
	return &ProviderStore{
		TP:        tp,
		ProjectID: projectID,
		refCache:  map[string]*provider.Task{},
		idCache:   map[string]*provider.Task{},
	}
}

func (s *ProviderStore) AsDepsStore() RelationStore { return s }

func (s *ProviderStore) SupportsRelations(ctx context.Context) (bool, error) {
	if s == nil || s.TP == nil {
		return false, ErrCapabilityUnknown
	}
	if _, ok := s.TP.(provider.RelationProvider); ok {
		return true, nil
	}
	// BoundClient may wrap a RelationProvider — try type assert only.
	return false, nil
}

func (s *ProviderStore) rel() (provider.RelationProvider, error) {
	rp, ok := s.TP.(provider.RelationProvider)
	if !ok || rp == nil {
		return nil, ErrCapabilityUnsupported
	}
	return rp, nil
}

func (s *ProviderStore) bulk() (provider.BulkRelationProvider, bool) {
	bp, ok := s.TP.(provider.BulkRelationProvider)
	return bp, ok && bp != nil
}

// hydrateFresh always re-lists the full project and rebuilds maps.
// Duplicate ref→id or id→ref mappings fail the snapshot (stale/capability).
// When a SnapshotFence is present and already hydrated, reuses fence maps
// (one hydration per fence). Thread-safe: takes s.mu for map swaps.
// Caller must NOT hold s.mu.
func (s *ProviderStore) hydrateFresh(ctx context.Context) error {
	if fence := FenceFrom(ctx); fence != nil {
		fence.mu.Lock()
		if fence.hydrated && len(fence.tasksByID) > 0 {
			ref := cloneTaskMap(fence.tasksByRef)
			id := cloneTaskMap(fence.tasksByID)
			fence.mu.Unlock()
			s.mu.Lock()
			s.refCache = ref
			s.idCache = id
			s.mu.Unlock()
			return nil
		}
		fence.mu.Unlock()
	}

	s.ListTasksCalls.Add(1)
	tasks, err := s.TP.ListTasks(ctx, s.ProjectID, "")
	if err != nil {
		return err
	}
	refCache := map[string]*provider.Task{}
	idCache := map[string]*provider.Task{}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if t.ID == "" {
			return fmt.Errorf("deps: task missing immutable id (ref=%q)", t.Ref)
		}
		if t.Ref == "" {
			return fmt.Errorf("deps: task missing ref (id=%q)", t.ID)
		}
		if prev, ok := idCache[t.ID]; ok && prev.Ref != t.Ref {
			return fmt.Errorf("deps: duplicate task id %q maps to %q and %q", t.ID, prev.Ref, t.Ref)
		}
		if prev, ok := refCache[t.Ref]; ok && prev.ID != t.ID {
			return fmt.Errorf("deps: duplicate task ref %q maps to %q and %q", t.Ref, prev.ID, t.ID)
		}
		cp := *t
		refCache[t.Ref] = &cp
		idCache[t.ID] = &cp
	}
	s.mu.Lock()
	s.refCache = refCache
	s.idCache = idCache
	s.mu.Unlock()

	if fence := FenceFrom(ctx); fence != nil {
		fence.mu.Lock()
		fence.tasksByRef = cloneTaskMap(refCache)
		fence.tasksByID = cloneTaskMap(idCache)
		fence.hydrated = true
		fence.mu.Unlock()
	}
	return nil
}

func cloneTaskMap(in map[string]*provider.Task) map[string]*provider.Task {
	out := make(map[string]*provider.Task, len(in))
	for k, v := range in {
		if v == nil {
			continue
		}
		cp := *v
		out[k] = &cp
	}
	return out
}

func (s *ProviderStore) ResolveRef(ctx context.Context, ref Ref) (TaskID, error) {
	if err := s.hydrateFresh(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	t, ok := s.refCache[string(ref)]
	s.mu.Unlock()
	if ok {
		return TaskID(t.ID), nil
	}
	got, err := s.TP.GetTask(ctx, string(ref))
	if err != nil || got == nil {
		return "", fmt.Errorf("%w: %s", ErrUnresolvedRef, ref)
	}
	return TaskID(got.ID), nil
}

func (s *ProviderStore) TaskStatus(ctx context.Context, ref Ref) (string, TaskID, error) {
	if err := s.hydrateFresh(ctx); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	id := string(ref)
	if t, ok := s.refCache[string(ref)]; ok {
		id = t.ID
	}
	s.mu.Unlock()

	// Fresh Get outside lock — live status for Done check (not a full re-list).
	fresh, err := s.TP.GetTask(ctx, id)
	if err != nil || fresh == nil {
		return "", "", fmt.Errorf("%w: %s", ErrDeletedTask, ref)
	}
	st := provider.NormalizeStatus(fresh.Status)
	if st == provider.StatusUnknown || strings.HasPrefix(st, "unknown:") {
		return "", TaskID(fresh.ID), fmt.Errorf("deps: task %s has unreadable status %q", ref, fresh.Status)
	}
	s.mu.Lock()
	s.refCache[fresh.Ref] = fresh
	s.idCache[fresh.ID] = fresh
	s.mu.Unlock()
	if fence := FenceFrom(ctx); fence != nil {
		fence.mu.Lock()
		fence.tasksByRef[fresh.Ref] = fresh
		fence.tasksByID[fresh.ID] = fresh
		fence.mu.Unlock()
	}
	return st, TaskID(fresh.ID), nil
}

func (s *ProviderStore) mapEdgeStrict(ctx context.Context, r provider.Relation) (DependencyEdge, error) {
	if r.ID == "" {
		return DependencyEdge{}, fmt.Errorf("deps: relation missing id")
	}
	if r.SourceTaskID == "" || r.TargetTaskID == "" {
		return DependencyEdge{}, fmt.Errorf("deps: relation %s missing endpoint id", r.ID)
	}
	typ := mapRelTypeStrict(r.Type)
	if !ValidEdgeType(typ) {
		return DependencyEdge{}, fmt.Errorf("%w: relation %s type %q", ErrUnknownEdgeType, r.ID, r.Type)
	}
	e := DependencyEdge{
		RelationID: r.ID,
		SourceID:   TaskID(r.SourceTaskID),
		TargetID:   TaskID(r.TargetTaskID),
		Type:       typ,
	}
	s.mu.Lock()
	srcT := s.idCache[r.SourceTaskID]
	tgtT := s.idCache[r.TargetTaskID]
	s.mu.Unlock()
	if srcT == nil {
		t, gerr := s.TP.GetTask(ctx, r.SourceTaskID)
		if gerr != nil || t == nil {
			return DependencyEdge{}, fmt.Errorf("deps: relation %s source %s unreadable: %w", r.ID, r.SourceTaskID, ErrDeletedTask)
		}
		srcT = t
		s.mu.Lock()
		s.idCache[t.ID] = t
		s.refCache[t.Ref] = t
		s.mu.Unlock()
	}
	if tgtT == nil {
		t, gerr := s.TP.GetTask(ctx, r.TargetTaskID)
		if gerr != nil || t == nil {
			return DependencyEdge{}, fmt.Errorf("deps: relation %s target %s unreadable: %w", r.ID, r.TargetTaskID, ErrDeletedTask)
		}
		tgtT = t
		s.mu.Lock()
		s.idCache[t.ID] = t
		s.refCache[t.Ref] = t
		s.mu.Unlock()
	}
	e.SourceRef = Ref(srcT.Ref)
	e.TargetRef = Ref(tgtT.Ref)
	if !e.SourceRef.Valid() || !e.TargetRef.Valid() {
		return DependencyEdge{}, fmt.Errorf("deps: relation %s missing endpoint refs", r.ID)
	}
	return e, nil
}

func (s *ProviderStore) ListRelations(ctx context.Context, taskID TaskID) ([]DependencyEdge, error) {
	rp, err := s.rel()
	if err != nil {
		return nil, err
	}
	if err := s.hydrateFresh(ctx); err != nil {
		return nil, err
	}
	s.ListRelCalls.Add(1)
	rels, err := rp.ListRelations(ctx, string(taskID))
	if err != nil {
		return nil, err
	}
	out := make([]DependencyEdge, 0, len(rels))
	for _, r := range rels {
		e, merr := s.mapEdgeStrict(ctx, r)
		if merr != nil {
			return nil, merr
		}
		out = append(out, e)
	}
	return out, nil
}

// SnapshotGraph returns the authoritative full relation closure.
// Prefers BulkRelationProvider; reuses an immutable fence snapshot when present
// and not invalidated. Never sequential per-task ListRelations over the board.
func (s *ProviderStore) SnapshotGraph(ctx context.Context) (*GraphSnapshot, error) {
	s.SnapshotGraphCalls.Add(1)
	if fence := FenceFrom(ctx); fence != nil {
		if snap := fence.GetSnap(); snap != nil {
			return snap, nil
		}
	}

	rp, err := s.rel()
	if err != nil {
		return nil, err
	}
	if err := s.hydrateFresh(ctx); err != nil {
		return nil, err
	}
	// Copy id set under lock for non-bulk fallback.
	s.mu.Lock()
	ids := make([]string, 0, len(s.idCache))
	for id := range s.idCache {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	var rels []provider.Relation
	if bp, ok := s.bulk(); ok {
		s.BulkRelCalls.Add(1)
		rels, err = bp.ListProjectRelations(ctx, s.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("deps: bulk project relations: %w", err)
		}
	} else {
		// Fallback for partial adapters: still dual-end assemble. Live Kaneo
		// implements BulkRelationProvider — do not use this for production boards.
		sort.Strings(ids)
		byTask := map[string][]provider.Relation{}
		for _, id := range ids {
			s.ListRelCalls.Add(1)
			lr, lerr := rp.ListRelations(ctx, id)
			if lerr != nil {
				return nil, fmt.Errorf("deps: snapshot list %s: %w", id, lerr)
			}
			byTask[id] = lr
		}
		rels, err = assembleDualEnd(byTask)
		if err != nil {
			return nil, err
		}
	}

	out := make([]DependencyEdge, 0, len(rels))
	for _, r := range rels {
		e, merr := s.mapEdgeStrict(ctx, r)
		if merr != nil {
			return nil, merr
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RelationID != out[j].RelationID {
			return out[i].RelationID < out[j].RelationID
		}
		return out[i].CanonicalKey() < out[j].CanonicalKey()
	})
	h := sha256.New()
	for _, e := range out {
		_, _ = h.Write([]byte(e.RelationID + "|" + e.CanonicalKey()))
		_, _ = h.Write([]byte{0})
	}
	if out == nil {
		out = []DependencyEdge{}
	}
	snap := &GraphSnapshot{
		Edges:            out,
		ProviderRevision: hex.EncodeToString(h.Sum(nil)),
	}
	if fence := FenceFrom(ctx); fence != nil {
		fence.setSnap(snap)
	}
	return cloneSnapshot(snap), nil
}

// RelationRevision returns the fence snapshot's provider revision when present.
// It does NOT re-run O(board) project fan-out. Post-side-effect TOCTOU uses
// AssertPrerequisiteClosureFresh (closure-scoped re-read, not full board).
func (s *ProviderStore) RelationRevision(ctx context.Context) (string, error) {
	if fence := FenceFrom(ctx); fence != nil {
		if snap := fence.GetSnap(); snap != nil && snap.ProviderRevision != "" {
			return snap.ProviderRevision, nil
		}
	}
	snap, err := s.SnapshotGraph(ctx)
	if err != nil {
		return "", err
	}
	return snap.ProviderRevision, nil
}

// Closure refresh budget: re-read only the launch task's prerequisite closure
// (not unrelated board components). Live Kaneo has no project-relation RPC;
// cold SnapshotGraph remains honest O(board) concurrent HTTP under list deadline.
const (
	// DefaultClosureRefreshMaxRequests caps ListRelations during post-fence TOCTOU.
	DefaultClosureRefreshMaxRequests = 64
	// DefaultClosureRefreshTimeout bounds wall time for closure re-read.
	DefaultClosureRefreshTimeout = 8 * time.Second
)

// AssertPrerequisiteClosureFresh re-lists relations for the frozen prerequisite
// closure of the launch task (transitive inbound blocks ancestors + the task),
// expanding to a fixed point when live listings reveal new incident nodes.
// Compares the closure edge multiset to the fenced snapshot. Unrelated board
// components are not re-read. Fails closed on budget exhaustion or drift.
//
// AssertIncidentEdgesFresh is an alias kept for older call sites.
func (s *ProviderStore) AssertPrerequisiteClosureFresh(ctx context.Context, taskRef Ref, taskID TaskID, snap *GraphSnapshot) error {
	if snap == nil {
		return fmt.Errorf("deps: AssertPrerequisiteClosureFresh: nil snapshot")
	}
	rp, err := s.rel()
	if err != nil {
		return err
	}
	if !taskID.Valid() && !taskRef.Valid() {
		return fmt.Errorf("deps: AssertPrerequisiteClosureFresh: task identity required")
	}

	// Frozen prerequisite node set from snapshot (fixed point on inbound blocks).
	frozenNodes := prerequisiteClosureNodes(snap.Edges, taskRef, taskID)
	if len(frozenNodes) == 0 {
		return fmt.Errorf("deps: AssertPrerequisiteClosureFresh: empty closure")
	}
	frozenEdges := closureRelevantEdges(snap.Edges, frozenNodes)

	// Budgeted live re-read + expand when new prereq nodes appear.
	deadline := DefaultClosureRefreshTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < deadline {
			deadline = rem
		}
	}
	rctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// Work queue: task IDs to ListRelations (prefer immutable ids).
	type node struct {
		id  TaskID
		ref Ref
	}
	var queue []node
	seenID := map[TaskID]bool{}
	seenRef := map[Ref]bool{}
	enqueue := func(id TaskID, ref Ref) {
		if id.Valid() {
			if seenID[id] {
				return
			}
			seenID[id] = true
		} else if ref.Valid() {
			if seenRef[ref] {
				return
			}
			seenRef[ref] = true
		} else {
			return
		}
		queue = append(queue, node{id: id, ref: ref})
	}
	for _, nk := range frozenNodes {
		enqueue(nk.id, nk.ref)
	}

	liveByKey := map[string]DependencyEdge{}
	listedID := map[TaskID]bool{}
	requests := 0
	maxReq := DefaultClosureRefreshMaxRequests

	for len(queue) > 0 {
		if err := rctx.Err(); err != nil {
			return fmt.Errorf("%w: prerequisite closure refresh budget exceeded: %v", ErrPostClaimDrift, err)
		}
		if requests >= maxReq {
			return fmt.Errorf("%w: prerequisite closure refresh exceeded max requests %d", ErrPostClaimDrift, maxReq)
		}
		cur := queue[0]
		queue = queue[1:]
		listID := cur.id
		if !listID.Valid() {
			// Resolve ref → id via fence/store cache when possible.
			if cur.ref.Valid() {
				if id, rerr := s.ResolveRef(rctx, cur.ref); rerr == nil {
					listID = id
				}
			}
		}
		if !listID.Valid() {
			return fmt.Errorf("%w: cannot list closure node %s", ErrPostClaimDrift, cur.ref)
		}
		if listedID[listID] {
			continue
		}
		listedID[listID] = true
		requests++
		s.ListRelCalls.Add(1)
		rels, lerr := rp.ListRelations(rctx, string(listID))
		if lerr != nil {
			return fmt.Errorf("%w: list relations %s: %v", ErrPostClaimDrift, listID, lerr)
		}
		for _, r := range rels {
			e, merr := s.mapEdgeStrict(rctx, r)
			if merr != nil {
				return merr
			}
			if e.Type != EdgeBlocks {
				continue
			}
			// Record all blocks edges seen; filter to closure-relevant below.
			liveByKey[edgeAuthKey(e)] = e
			// Fixed-point expand: new source that blocks a node already in the
			// live/frozen closure becomes a prerequisite that must be re-read.
			if e.TargetID == listID || (cur.ref.Valid() && e.TargetRef == cur.ref) ||
				nodeInClosure(frozenNodes, e.TargetID, e.TargetRef) || listedID[e.TargetID] {
				if e.SourceID.Valid() && !listedID[e.SourceID] {
					enqueue(e.SourceID, e.SourceRef)
					// Grow frozenNodes identity set for relevance filtering.
					frozenNodes = append(frozenNodes, closureNode{id: e.SourceID, ref: e.SourceRef})
				}
			}
		}
	}

	// Live relevant edges: blocks edges whose target is in the (possibly expanded) closure.
	liveRelevant := make([]DependencyEdge, 0, len(liveByKey))
	for _, e := range liveByKey {
		if nodeInClosure(frozenNodes, e.TargetID, e.TargetRef) ||
			nodeInClosure(frozenNodes, e.SourceID, e.SourceRef) && nodeInClosure(frozenNodes, e.TargetID, e.TargetRef) {
			// Inbound to any closure node, or both ends in closure.
			if nodeInClosure(frozenNodes, e.TargetID, e.TargetRef) {
				liveRelevant = append(liveRelevant, e)
			}
		}
	}

	if !edgeMultisetEqualKeys(liveRelevant, frozenEdges) {
		return fmt.Errorf("%w: prerequisite closure edges for %s changed since fence snapshot (requests=%d)", ErrPostClaimDrift, taskRef, requests)
	}
	return nil
}

// AssertIncidentEdgesFresh is the historical name; it now validates the full
// prerequisite closure (not only the launch task's incident multiset).
func (s *ProviderStore) AssertIncidentEdgesFresh(ctx context.Context, taskRef Ref, taskID TaskID, snap *GraphSnapshot) error {
	return s.AssertPrerequisiteClosureFresh(ctx, taskRef, taskID, snap)
}

type closureNode struct {
	id  TaskID
	ref Ref
}

func nodeKey(id TaskID, ref Ref) string {
	if id.Valid() {
		return "id:" + string(id)
	}
	return "ref:" + string(ref)
}

func nodeInClosure(nodes []closureNode, id TaskID, ref Ref) bool {
	for _, n := range nodes {
		if id.Valid() && n.id.Valid() && n.id == id {
			return true
		}
		if ref.Valid() && n.ref.Valid() && n.ref == ref {
			return true
		}
	}
	return false
}

// prerequisiteClosureNodes returns launch + transitive inbound blockers (sources
// of blocks edges targeting the growing set) from a frozen snapshot.
func prerequisiteClosureNodes(edges []DependencyEdge, taskRef Ref, taskID TaskID) []closureNode {
	var nodes []closureNode
	seen := map[string]bool{}
	add := func(id TaskID, ref Ref) {
		k := nodeKey(id, ref)
		if k == "id:" || k == "ref:" || seen[k] {
			return
		}
		// Also de-dupe when both id and ref present under either key.
		if id.Valid() && seen["id:"+string(id)] {
			return
		}
		if ref.Valid() && seen["ref:"+string(ref)] {
			return
		}
		seen[k] = true
		if id.Valid() {
			seen["id:"+string(id)] = true
		}
		if ref.Valid() {
			seen["ref:"+string(ref)] = true
		}
		nodes = append(nodes, closureNode{id: id, ref: ref})
	}
	add(taskID, taskRef)
	changed := true
	for changed {
		changed = false
		for _, e := range edges {
			if e.Type != EdgeBlocks {
				continue
			}
			if !nodeInClosure(nodes, e.TargetID, e.TargetRef) {
				continue
			}
			before := len(nodes)
			add(e.SourceID, e.SourceRef)
			if len(nodes) > before {
				changed = true
			}
		}
	}
	return nodes
}

// closureRelevantEdges are blocks edges whose target is in the prerequisite
// closure (inbound edges that determine reachability to the launch task).
func closureRelevantEdges(edges []DependencyEdge, nodes []closureNode) []DependencyEdge {
	var out []DependencyEdge
	for _, e := range edges {
		if e.Type != EdgeBlocks {
			continue
		}
		if nodeInClosure(nodes, e.TargetID, e.TargetRef) {
			out = append(out, e)
		}
	}
	return out
}

func edgeAuthKey(e DependencyEdge) string {
	if e.RelationID != "" {
		return e.RelationID + "|" + e.Key()
	}
	return e.Key()
}

func edgeMultisetEqualKeys(a, b []DependencyEdge) bool {
	ca := map[string]int{}
	cb := map[string]int{}
	for _, e := range a {
		ca[edgeAuthKey(e)]++
	}
	for _, e := range b {
		cb[edgeAuthKey(e)]++
	}
	if len(ca) != len(cb) {
		return false
	}
	for k, n := range ca {
		if cb[k] != n {
			return false
		}
	}
	return true
}

func assembleDualEnd(byTask map[string][]provider.Relation) ([]provider.Relation, error) {
	type acc struct {
		src *provider.Relation
		tgt *provider.Relation
	}
	accs := map[string]*acc{}
	for taskID, rels := range byTask {
		for i := range rels {
			r := rels[i]
			if r.ID == "" {
				return nil, fmt.Errorf("deps: snapshot: empty relation id on task %s", taskID)
			}
			a := accs[r.ID]
			if a == nil {
				a = &acc{}
				accs[r.ID] = a
			}
			if r.SourceTaskID == taskID {
				if a.src != nil && !relationEqual(*a.src, r) {
					return nil, fmt.Errorf("deps: snapshot: relation %s inconsistent on source listings", r.ID)
				}
				cp := r
				a.src = &cp
			}
			if r.TargetTaskID == taskID {
				if a.tgt != nil && !relationEqual(*a.tgt, r) {
					return nil, fmt.Errorf("deps: snapshot: relation %s inconsistent on target listings", r.ID)
				}
				cp := r
				a.tgt = &cp
			}
			if r.SourceTaskID != taskID && r.TargetTaskID != taskID {
				return nil, fmt.Errorf("deps: snapshot: relation %s listed on unrelated task %s", r.ID, taskID)
			}
		}
	}
	out := make([]provider.Relation, 0, len(accs))
	var keys []string
	for id := range accs {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		a := accs[id]
		if a.src == nil || a.tgt == nil {
			return nil, fmt.Errorf("deps: snapshot: relation %s not visible on both endpoints (one-sided)", id)
		}
		if !relationEqual(*a.src, *a.tgt) {
			return nil, fmt.Errorf("deps: snapshot: relation %s field disagreement between source and target listings", id)
		}
		out = append(out, *a.src)
	}
	return out, nil
}

func relationEqual(a, b provider.Relation) bool {
	return a.ID == b.ID && a.SourceTaskID == b.SourceTaskID &&
		a.TargetTaskID == b.TargetTaskID && a.Type == b.Type
}

func (s *ProviderStore) CreateRelation(ctx context.Context, edge DependencyEdge) (DependencyEdge, error) {
	rp, err := s.rel()
	if err != nil {
		return DependencyEdge{}, err
	}
	if !ValidEdgeType(edge.Type) {
		if edge.Type == "" {
			return DependencyEdge{}, fmt.Errorf("%w: empty type", ErrUnknownEdgeType)
		}
		return DependencyEdge{}, fmt.Errorf("%w: %q", ErrUnknownEdgeType, edge.Type)
	}
	src := string(edge.SourceID)
	tgt := string(edge.TargetID)
	if src == "" && edge.SourceRef.Valid() {
		id, rerr := s.ResolveRef(ctx, edge.SourceRef)
		if rerr != nil {
			return DependencyEdge{}, rerr
		}
		src = string(id)
	}
	if tgt == "" && edge.TargetRef.Valid() {
		id, rerr := s.ResolveRef(ctx, edge.TargetRef)
		if rerr != nil {
			return DependencyEdge{}, rerr
		}
		tgt = string(id)
	}
	if src == "" || tgt == "" {
		return DependencyEdge{}, fmt.Errorf("deps: create requires resolved source and target IDs")
	}
	if src == tgt {
		return DependencyEdge{}, ErrSelfEdge
	}
	typ := provider.RelationBlocks
	if edge.Type == EdgeRelated {
		typ = provider.RelationRelated
	}
	// Mutations invalidate fence so next snapshot is fresh.
	if fence := FenceFrom(ctx); fence != nil {
		fence.Invalidate(false)
	}
	rel, err := rp.CreateRelation(ctx, src, tgt, typ)
	if err != nil {
		if provider.IsAmbiguous(err) || provider.IsTimeout(err) {
			if existing, ferr := s.findEdgeBothEnds(ctx, src, tgt, typ); ferr == nil && existing != nil {
				return *existing, nil
			}
			return DependencyEdge{}, fmt.Errorf("%w: %v", ErrAmbiguousMutation, err)
		}
		if existing, ferr := s.findEdgeBothEnds(ctx, src, tgt, typ); ferr == nil && existing != nil {
			return *existing, nil
		}
		return DependencyEdge{}, err
	}
	if rel == nil {
		return DependencyEdge{}, fmt.Errorf("deps: create returned nil relation")
	}
	out, merr := s.mapEdgeStrict(ctx, *rel)
	if merr != nil {
		return DependencyEdge{}, merr
	}
	return out, nil
}

func (s *ProviderStore) findEdgeBothEnds(ctx context.Context, src, tgt string, typ provider.RelationType) (*DependencyEdge, error) {
	rp, err := s.rel()
	if err != nil {
		return nil, err
	}
	s.ListRelCalls.Add(1)
	srcRels, err := rp.ListRelations(ctx, src)
	if err != nil {
		return nil, err
	}
	s.ListRelCalls.Add(1)
	tgtRels, err := rp.ListRelations(ctx, tgt)
	if err != nil {
		return nil, err
	}
	var fromSrc *provider.Relation
	for i := range srcRels {
		r := &srcRels[i]
		if r.SourceTaskID == src && r.TargetTaskID == tgt && r.Type == typ {
			fromSrc = r
			break
		}
	}
	if fromSrc == nil {
		return nil, nil
	}
	foundTgt := false
	for i := range tgtRels {
		r := &tgtRels[i]
		if r.ID == fromSrc.ID && relationEqual(*r, *fromSrc) {
			foundTgt = true
			break
		}
	}
	if !foundTgt {
		return nil, fmt.Errorf("deps: edge visible on source but not target (or fields disagree)")
	}
	e, merr := s.mapEdgeStrict(ctx, *fromSrc)
	if merr != nil {
		return nil, merr
	}
	return &e, nil
}

func (s *ProviderStore) DeleteRelation(ctx context.Context, relationID string, sourceID, targetID TaskID) error {
	rp, err := s.rel()
	if err != nil {
		return err
	}
	if !sourceID.Valid() || !targetID.Valid() {
		snap, serr := s.SnapshotGraph(ctx)
		if serr != nil {
			return fmt.Errorf("deps: delete cannot capture endpoints: %w", serr)
		}
		found := false
		for _, e := range snap.Edges {
			if e.RelationID == relationID {
				sourceID, targetID = e.SourceID, e.TargetID
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("deps: delete relation %s not found for endpoint capture", relationID)
		}
	}
	if fence := FenceFrom(ctx); fence != nil {
		fence.Invalidate(false)
	}
	if err := rp.DeleteRelation(ctx, relationID, string(sourceID), string(targetID)); err != nil {
		if provider.IsAmbiguous(err) || provider.IsTimeout(err) {
			return fmt.Errorf("%w: %v", ErrAmbiguousMutation, err)
		}
		return err
	}
	return nil
}

func mapRelTypeStrict(t provider.RelationType) EdgeType {
	switch t {
	case provider.RelationRelated:
		return EdgeRelated
	case provider.RelationSubtask:
		// Kaneo exposes parent/child structure as `subtask`. It is useful
		// context, but it must not become an eligibility blocker in the
		// provider-neutral graph.
		return EdgeRelated
	case provider.RelationBlocks:
		return EdgeBlocks
	default:
		return EdgeType(strings.ToLower(string(t)))
	}
}

// StoreFor returns a RelationStore for a TaskProvider.
func StoreFor(tp provider.TaskProvider, projectID string) RelationStore {
	return NewProviderStore(tp, projectID)
}
