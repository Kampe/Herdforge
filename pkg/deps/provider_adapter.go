package deps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// ProviderStore adapts a TaskProvider (+ optional RelationProvider) to RelationStore.
// Cache is mutex-protected and refreshed on every SnapshotGraph / hydrateFresh
// so concurrent gates cannot race and deleted/new tasks are not frozen forever.
type ProviderStore struct {
	mu        sync.Mutex
	TP        provider.TaskProvider
	ProjectID string
	refCache  map[string]*provider.Task
	idCache   map[string]*provider.Task
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
	return false, nil
}

func (s *ProviderStore) rel() (provider.RelationProvider, error) {
	rp, ok := s.TP.(provider.RelationProvider)
	if !ok || rp == nil {
		return nil, ErrCapabilityUnsupported
	}
	return rp, nil
}

// hydrateFresh always re-lists the full project and rebuilds maps.
// Duplicate ref→id or id→ref mappings fail the snapshot (stale/capability).
func (s *ProviderStore) hydrateFresh(ctx context.Context) error {
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
	s.refCache = refCache
	s.idCache = idCache
	return nil
}

func (s *ProviderStore) ResolveRef(ctx context.Context, ref Ref) (TaskID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.hydrateFresh(ctx); err != nil {
		return "", err
	}
	if t, ok := s.refCache[string(ref)]; ok {
		return TaskID(t.ID), nil
	}
	t, err := s.TP.GetTask(ctx, string(ref))
	if err != nil || t == nil {
		return "", fmt.Errorf("%w: %s", ErrUnresolvedRef, ref)
	}
	return TaskID(t.ID), nil
}

func (s *ProviderStore) TaskStatus(ctx context.Context, ref Ref) (string, TaskID, error) {
	s.mu.Lock()
	if err := s.hydrateFresh(ctx); err != nil {
		s.mu.Unlock()
		return "", "", err
	}
	id := string(ref)
	if t, ok := s.refCache[string(ref)]; ok {
		id = t.ID
	}
	s.mu.Unlock()

	// Fresh Get outside lock (provider I/O).
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
	s.mu.Lock()
	if err := s.hydrateFresh(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
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

// SnapshotGraph rebuilds the full task set, lists relations per task, and
// requires every relation to be visible with identical fields on BOTH endpoints.
func (s *ProviderStore) SnapshotGraph(ctx context.Context) (*GraphSnapshot, error) {
	rp, err := s.rel()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if err := s.hydrateFresh(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	ids := make([]string, 0, len(s.idCache))
	for id := range s.idCache {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)

	// Collect listings per task.
	byTask := map[string][]provider.Relation{}
	for _, id := range ids {
		rels, lerr := rp.ListRelations(ctx, id)
		if lerr != nil {
			return nil, fmt.Errorf("deps: snapshot list %s: %w", id, lerr)
		}
		byTask[id] = rels
	}

	// Index by relation ID with dual-end agreement.
	type acc struct {
		srcSide *provider.Relation
		tgtSide *provider.Relation
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
				if a.srcSide != nil && !relationEqual(*a.srcSide, r) {
					return nil, fmt.Errorf("deps: snapshot: relation %s inconsistent on source listings", r.ID)
				}
				cp := r
				a.srcSide = &cp
			}
			if r.TargetTaskID == taskID {
				if a.tgtSide != nil && !relationEqual(*a.tgtSide, r) {
					return nil, fmt.Errorf("deps: snapshot: relation %s inconsistent on target listings", r.ID)
				}
				cp := r
				a.tgtSide = &cp
			}
			// If listed on a third task id, still require field agreement with first.
			if r.SourceTaskID != taskID && r.TargetTaskID != taskID {
				return nil, fmt.Errorf("deps: snapshot: relation %s listed on unrelated task %s", r.ID, taskID)
			}
		}
	}

	out := make([]DependencyEdge, 0, len(accs))
	var keys []string
	for id := range accs {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		a := accs[id]
		if a.srcSide == nil || a.tgtSide == nil {
			return nil, fmt.Errorf("deps: snapshot: relation %s not visible on both endpoints (one-sided)", id)
		}
		if !relationEqual(*a.srcSide, *a.tgtSide) {
			return nil, fmt.Errorf("deps: snapshot: relation %s field disagreement between source and target listings", id)
		}
		e, merr := s.mapEdgeStrict(ctx, *a.srcSide)
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
	return &GraphSnapshot{
		Edges:            out,
		ProviderRevision: hex.EncodeToString(h.Sum(nil)),
	}, nil
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
	srcRels, err := rp.ListRelations(ctx, src)
	if err != nil {
		return nil, err
	}
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
	// Prefer capture from snapshot when endpoints incomplete.
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
	case provider.RelationBlocks:
		return EdgeBlocks
	default:
		// Unknown/subtask fail ValidEdgeType — snapshot fails closed.
		return EdgeType(strings.ToLower(string(t)))
	}
}

// StoreFor returns a RelationStore for a TaskProvider.
func StoreFor(tp provider.TaskProvider, projectID string) RelationStore {
	return NewProviderStore(tp, projectID)
}
