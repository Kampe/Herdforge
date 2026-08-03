package deps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// ProviderStore adapts a TaskProvider (+ optional RelationProvider) to RelationStore.
type ProviderStore struct {
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

func (s *ProviderStore) hydrate(ctx context.Context) error {
	if len(s.refCache) > 0 {
		return nil
	}
	tasks, err := s.TP.ListTasks(ctx, s.ProjectID, "")
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		cp := *t
		s.refCache[t.Ref] = &cp
		s.idCache[t.ID] = &cp
	}
	return nil
}

func (s *ProviderStore) ResolveRef(ctx context.Context, ref Ref) (TaskID, error) {
	if err := s.hydrate(ctx); err != nil {
		return "", err
	}
	if t, ok := s.refCache[string(ref)]; ok {
		return TaskID(t.ID), nil
	}
	t, err := s.TP.GetTask(ctx, string(ref))
	if err != nil || t == nil {
		return "", fmt.Errorf("%w: %s", ErrUnresolvedRef, ref)
	}
	s.refCache[t.Ref] = t
	s.idCache[t.ID] = t
	return TaskID(t.ID), nil
}

func (s *ProviderStore) TaskStatus(ctx context.Context, ref Ref) (string, TaskID, error) {
	if err := s.hydrate(ctx); err != nil {
		return "", "", err
	}
	id := string(ref)
	if t, ok := s.refCache[string(ref)]; ok {
		id = t.ID
	}
	fresh, err := s.TP.GetTask(ctx, id)
	if err != nil || fresh == nil {
		return "", "", fmt.Errorf("%w: %s", ErrDeletedTask, ref)
	}
	st := provider.NormalizeStatus(fresh.Status)
	if st == provider.StatusUnknown || strings.HasPrefix(st, "unknown:") {
		return "", TaskID(fresh.ID), fmt.Errorf("deps: task %s has unreadable status %q", ref, fresh.Status)
	}
	s.refCache[fresh.Ref] = fresh
	s.idCache[fresh.ID] = fresh
	return st, TaskID(fresh.ID), nil
}

func (s *ProviderStore) mapEdge(ctx context.Context, r provider.Relation) (DependencyEdge, error) {
	e := DependencyEdge{
		RelationID: r.ID,
		SourceID:   TaskID(r.SourceTaskID),
		TargetID:   TaskID(r.TargetTaskID),
		Type:       mapRelType(r.Type),
	}
	// Listing accepts provider types (subtask etc.); mutations still reject unknowns.
	if t := s.idCache[r.SourceTaskID]; t != nil {
		e.SourceRef = Ref(t.Ref)
	} else if t, gerr := s.TP.GetTask(ctx, r.SourceTaskID); gerr == nil && t != nil {
		e.SourceRef = Ref(t.Ref)
		s.idCache[t.ID] = t
		s.refCache[t.Ref] = t
	}
	if t := s.idCache[r.TargetTaskID]; t != nil {
		e.TargetRef = Ref(t.Ref)
	} else if t, gerr := s.TP.GetTask(ctx, r.TargetTaskID); gerr == nil && t != nil {
		e.TargetRef = Ref(t.Ref)
		s.idCache[t.ID] = t
		s.refCache[t.Ref] = t
	}
	return e, nil
}

func (s *ProviderStore) ListRelations(ctx context.Context, taskID TaskID) ([]DependencyEdge, error) {
	rp, err := s.rel()
	if err != nil {
		return nil, err
	}
	if err := s.hydrate(ctx); err != nil {
		return nil, err
	}
	rels, err := rp.ListRelations(ctx, string(taskID))
	if err != nil {
		return nil, err
	}
	out := make([]DependencyEdge, 0, len(rels))
	for _, r := range rels {
		e, merr := s.mapEdge(ctx, r)
		if merr != nil {
			return nil, merr
		}
		out = append(out, e)
	}
	return out, nil
}

// SnapshotGraph walks every project task and unions relation listings.
// This is the authoritative full closure — not a single-task listing.
func (s *ProviderStore) SnapshotGraph(ctx context.Context) (*GraphSnapshot, error) {
	rp, err := s.rel()
	if err != nil {
		return nil, err
	}
	if err := s.hydrate(ctx); err != nil {
		return nil, err
	}
	seen := map[string]DependencyEdge{}
	var ids []string
	for id := range s.idCache {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rels, lerr := rp.ListRelations(ctx, id)
		if lerr != nil {
			return nil, fmt.Errorf("deps: snapshot list %s: %w", id, lerr)
		}
		for _, r := range rels {
			e, merr := s.mapEdge(ctx, r)
			if merr != nil {
				return nil, merr
			}
			key := e.RelationID
			if key == "" {
				key = e.CanonicalKey()
			}
			seen[key] = e
		}
	}
	out := make([]DependencyEdge, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RelationID != out[j].RelationID {
			return out[i].RelationID < out[j].RelationID
		}
		return out[i].CanonicalKey() < out[j].CanonicalKey()
	})
	// Provider revision: SHA-256 of sorted relation IDs + canonical keys.
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
			// Reconcile: if edge already exists, return it (no duplicate).
			if existing, ferr := s.findEdge(ctx, src, tgt, typ); ferr == nil && existing != nil {
				return *existing, nil
			}
			return DependencyEdge{}, fmt.Errorf("%w: %v", ErrAmbiguousMutation, err)
		}
		// Non-timeout create failure: still try reconcile for already-exists races.
		if existing, ferr := s.findEdge(ctx, src, tgt, typ); ferr == nil && existing != nil {
			return *existing, nil
		}
		return DependencyEdge{}, err
	}
	if rel == nil {
		return DependencyEdge{}, fmt.Errorf("deps: create returned nil relation")
	}
	out, merr := s.mapEdge(ctx, *rel)
	if merr != nil {
		return DependencyEdge{}, merr
	}
	out.SourceRef = edge.SourceRef
	out.TargetRef = edge.TargetRef
	return out, nil
}

func (s *ProviderStore) findEdge(ctx context.Context, src, tgt string, typ provider.RelationType) (*DependencyEdge, error) {
	rp, err := s.rel()
	if err != nil {
		return nil, err
	}
	rels, err := rp.ListRelations(ctx, src)
	if err != nil {
		return nil, err
	}
	for _, r := range rels {
		if r.SourceTaskID == src && r.TargetTaskID == tgt && r.Type == typ {
			e, merr := s.mapEdge(ctx, r)
			if merr != nil {
				return nil, merr
			}
			return &e, nil
		}
	}
	return nil, nil
}

func (s *ProviderStore) DeleteRelation(ctx context.Context, relationID string, sourceID, targetID TaskID) error {
	rp, err := s.rel()
	if err != nil {
		return err
	}
	if !sourceID.Valid() || !targetID.Valid() {
		// Capture endpoints from snapshot if caller omitted them.
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

func mapRelType(t provider.RelationType) EdgeType {
	switch t {
	case provider.RelationRelated:
		return EdgeRelated
	case provider.RelationBlocks:
		return EdgeBlocks
	case provider.RelationSubtask:
		// Subtask is not eligibility-authoritative; map fails ValidEdgeType for deps mutations.
		return EdgeType("subtask")
	default:
		return EdgeType(strings.ToLower(string(t)))
	}
}

// StoreFor returns a RelationStore for a TaskProvider.
func StoreFor(tp provider.TaskProvider, projectID string) RelationStore {
	return NewProviderStore(tp, projectID)
}
