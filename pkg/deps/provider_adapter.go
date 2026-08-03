package deps

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// ProviderStore adapts a TaskProvider (+ optional RelationProvider) to RelationStore.
type ProviderStore struct {
	TP provider.TaskProvider
	// ProjectID scopes ListTasks when resolving refs.
	ProjectID string
	// refCache is filled lazily from ListTasks.
	refCache map[string]*provider.Task // ref → task
	idCache  map[string]*provider.Task // id → task
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

// AsDepsStore satisfies the optional adapter used by FromProvider when
// TaskProvider implementations embed *ProviderStore — not used directly.
func (s *ProviderStore) AsDepsStore() RelationStore { return s }

func (s *ProviderStore) SupportsRelations(ctx context.Context) (bool, error) {
	if s == nil || s.TP == nil {
		return false, ErrCapabilityUnknown
	}
	if _, ok := s.TP.(provider.RelationProvider); ok {
		return true, nil
	}
	// Explicit unsupported — never silent empty success.
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
	// Direct GetTask by ref (some providers accept refs).
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
	if t, ok := s.refCache[string(ref)]; ok {
		// Re-read for freshness (TOCTOU).
		fresh, err := s.TP.GetTask(ctx, t.ID)
		if err != nil {
			return "", "", fmt.Errorf("deps: task status read %s: %w", ref, err)
		}
		if fresh == nil {
			return "", "", fmt.Errorf("%w: %s", ErrDeletedTask, ref)
		}
		st := provider.NormalizeStatus(fresh.Status)
		if st == provider.StatusUnknown || strings.HasPrefix(st, "unknown:") {
			return "", TaskID(fresh.ID), fmt.Errorf("deps: blocker %s has unreadable status %q", ref, fresh.Status)
		}
		s.refCache[fresh.Ref] = fresh
		s.idCache[fresh.ID] = fresh
		return st, TaskID(fresh.ID), nil
	}
	t, err := s.TP.GetTask(ctx, string(ref))
	if err != nil || t == nil {
		return "", "", fmt.Errorf("%w: %s", ErrDeletedTask, ref)
	}
	st := provider.NormalizeStatus(t.Status)
	if st == provider.StatusUnknown || strings.HasPrefix(st, "unknown:") {
		return "", TaskID(t.ID), fmt.Errorf("deps: task %s has unreadable status %q", ref, t.Status)
	}
	return st, TaskID(t.ID), nil
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
		e := DependencyEdge{
			RelationID: r.ID,
			SourceID:   TaskID(r.SourceTaskID),
			TargetID:   TaskID(r.TargetTaskID),
			Type:       mapRelType(r.Type),
		}
		if t := s.idCache[r.SourceTaskID]; t != nil {
			e.SourceRef = Ref(t.Ref)
		}
		if t := s.idCache[r.TargetTaskID]; t != nil {
			e.TargetRef = Ref(t.Ref)
		}
		// If cache miss, try GetTask for each end.
		if !e.SourceRef.Valid() && r.SourceTaskID != "" {
			if t, gerr := s.TP.GetTask(ctx, r.SourceTaskID); gerr == nil && t != nil {
				e.SourceRef = Ref(t.Ref)
				s.idCache[t.ID] = t
				s.refCache[t.Ref] = t
			}
		}
		if !e.TargetRef.Valid() && r.TargetTaskID != "" {
			if t, gerr := s.TP.GetTask(ctx, r.TargetTaskID); gerr == nil && t != nil {
				e.TargetRef = Ref(t.Ref)
				s.idCache[t.ID] = t
				s.refCache[t.Ref] = t
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *ProviderStore) CreateRelation(ctx context.Context, edge DependencyEdge) (DependencyEdge, error) {
	rp, err := s.rel()
	if err != nil {
		return DependencyEdge{}, err
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
	typ := provider.RelationBlocks
	if edge.Type == EdgeRelated {
		typ = provider.RelationRelated
	}
	rel, err := rp.CreateRelation(ctx, src, tgt, typ)
	if err != nil {
		return DependencyEdge{}, err
	}
	if rel == nil {
		return DependencyEdge{}, fmt.Errorf("deps: create returned nil relation")
	}
	out := DependencyEdge{
		RelationID: rel.ID,
		SourceID:   TaskID(rel.SourceTaskID),
		TargetID:   TaskID(rel.TargetTaskID),
		SourceRef:  edge.SourceRef,
		TargetRef:  edge.TargetRef,
		Type:       mapRelType(rel.Type),
	}
	return out, nil
}

func (s *ProviderStore) DeleteRelation(ctx context.Context, relationID string) error {
	rp, err := s.rel()
	if err != nil {
		return err
	}
	return rp.DeleteRelation(ctx, relationID)
}

func mapRelType(t provider.RelationType) EdgeType {
	switch t {
	case provider.RelationRelated:
		return EdgeRelated
	default:
		return EdgeBlocks
	}
}

// StoreFor returns a RelationStore for a TaskProvider.
// Kaneo / Memory (with relations) succeed; others return capability unsupported
// wrapped store that fails SupportsRelations explicitly.
func StoreFor(tp provider.TaskProvider, projectID string) RelationStore {
	return NewProviderStore(tp, projectID)
}
