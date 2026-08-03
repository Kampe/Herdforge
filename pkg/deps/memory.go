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

// MemoryStore is an in-process RelationStore for tests and fixtures.
// It is concurrency-safe and supports full list/create/delete/readback.
type MemoryStore struct {
	mu        sync.Mutex
	tasks     map[string]*provider.Task // id → task
	byRef     map[string]string         // ref → id
	relations map[string]DependencyEdge // relationID → edge
	nextRel   int
	// Capable when true SupportsRelations returns true. Default true.
	Capable bool
	// CapabilityErr forces SupportsRelations to fail unknown.
	CapabilityErr error
	// ProviderRevisionToken is returned from SnapshotGraph (tests mutate for TOCTOU).
	ProviderRevisionToken string
	// mutateGen bumps on every create/delete for revision.
	mutateGen int
}

// NewMemoryStore returns a capable empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks:     map[string]*provider.Task{},
		byRef:     map[string]string{},
		relations: map[string]DependencyEdge{},
		Capable:   true,
	}
}

// AddTask registers a task for ResolveRef / TaskStatus.
func (m *MemoryStore) AddTask(t *provider.Task) {
	if t == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.tasks[t.ID] = &cp
	if t.Ref != "" {
		m.byRef[t.Ref] = t.ID
	}
}

// SetStatus updates a task status by ref or id.
func (m *MemoryStore) SetStatus(refOrID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := refOrID
	if x, ok := m.byRef[refOrID]; ok {
		id = x
	}
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrDeletedTask, refOrID)
	}
	t.Status = provider.NormalizeStatus(status)
	m.mutateGen++
	return nil
}

func (m *MemoryStore) SupportsRelations(context.Context) (bool, error) {
	if m.CapabilityErr != nil {
		return false, m.CapabilityErr
	}
	return m.Capable, nil
}

func (m *MemoryStore) ResolveRef(_ context.Context, ref Ref) (TaskID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byRef[string(ref)]
	if !ok {
		if t, ok := m.tasks[string(ref)]; ok {
			return TaskID(t.ID), nil
		}
		return "", fmt.Errorf("%w: %s", ErrUnresolvedRef, ref)
	}
	return TaskID(id), nil
}

func (m *MemoryStore) TaskStatus(_ context.Context, ref Ref) (string, TaskID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byRef[string(ref)]
	if !ok {
		if t, ok := m.tasks[string(ref)]; ok {
			return t.Status, TaskID(t.ID), nil
		}
		return "", "", fmt.Errorf("%w: %s", ErrDeletedTask, ref)
	}
	t := m.tasks[id]
	return t.Status, TaskID(id), nil
}

func (m *MemoryStore) ListRelations(_ context.Context, taskID TaskID) ([]DependencyEdge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked(string(taskID)), nil
}

func (m *MemoryStore) listLocked(want string) []DependencyEdge {
	if id, ok := m.byRef[want]; ok {
		want = id
	}
	var out []DependencyEdge
	for _, e := range m.relations {
		if string(e.SourceID) == want || string(e.TargetID) == want ||
			string(e.SourceRef) == want || string(e.TargetRef) == want {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelationID < out[j].RelationID })
	return out
}

func (m *MemoryStore) SnapshotGraph(context.Context) (*GraphSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DependencyEdge, 0, len(m.relations))
	for _, e := range m.relations {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelationID < out[j].RelationID })
	rev := m.ProviderRevisionToken
	if rev == "" {
		h := sha256.Sum256([]byte(fmt.Sprintf("mem|%d|%d", m.mutateGen, len(m.relations))))
		rev = hex.EncodeToString(h[:])
	}
	return &GraphSnapshot{Edges: out, ProviderRevision: rev}, nil
}

func (m *MemoryStore) CreateRelation(_ context.Context, edge DependencyEdge) (DependencyEdge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !ValidEdgeType(edge.Type) {
		if edge.Type == "" {
			return DependencyEdge{}, fmt.Errorf("%w: empty type", ErrUnknownEdgeType)
		}
		return DependencyEdge{}, fmt.Errorf("%w: %q", ErrUnknownEdgeType, edge.Type)
	}
	edge.Type = EdgeType(strings.ToLower(string(edge.Type)))

	if !edge.SourceID.Valid() && edge.SourceRef.Valid() {
		if id, ok := m.byRef[string(edge.SourceRef)]; ok {
			edge.SourceID = TaskID(id)
		} else {
			return DependencyEdge{}, fmt.Errorf("%w: source %s", ErrUnresolvedRef, edge.SourceRef)
		}
	}
	if !edge.TargetID.Valid() && edge.TargetRef.Valid() {
		if id, ok := m.byRef[string(edge.TargetRef)]; ok {
			edge.TargetID = TaskID(id)
		} else {
			return DependencyEdge{}, fmt.Errorf("%w: target %s", ErrUnresolvedRef, edge.TargetRef)
		}
	}
	if !edge.SourceRef.Valid() && edge.SourceID.Valid() {
		if t := m.tasks[string(edge.SourceID)]; t != nil {
			edge.SourceRef = Ref(t.Ref)
		}
	}
	if !edge.TargetRef.Valid() && edge.TargetID.Valid() {
		if t := m.tasks[string(edge.TargetID)]; t != nil {
			edge.TargetRef = Ref(t.Ref)
		}
	}
	if edge.SourceID == edge.TargetID || edge.SourceRef == edge.TargetRef {
		return DependencyEdge{}, ErrSelfEdge
	}

	// Idempotent reconcile: existing identical edge returned (no duplicate).
	for _, e := range m.relations {
		if e.SourceID == edge.SourceID && e.TargetID == edge.TargetID && e.Type == edge.Type {
			return e, nil
		}
	}

	m.nextRel++
	m.mutateGen++
	edge.RelationID = fmt.Sprintf("mem-rel-%d", m.nextRel)
	m.relations[edge.RelationID] = edge

	// Dual-end readback.
	srcList := m.listLocked(string(edge.SourceID))
	tgtList := m.listLocked(string(edge.TargetID))
	if !containsRel(srcList, edge.RelationID) || !containsRel(tgtList, edge.RelationID) {
		return DependencyEdge{}, fmt.Errorf("deps: create readback missing on source or target")
	}
	got, ok := m.relations[edge.RelationID]
	if !ok || got.CanonicalKey() != edge.CanonicalKey() {
		return DependencyEdge{}, fmt.Errorf("deps: create readback mismatch")
	}
	return got, nil
}

func containsRel(edges []DependencyEdge, id string) bool {
	for _, e := range edges {
		if e.RelationID == id {
			return true
		}
	}
	return false
}

func (m *MemoryStore) DeleteRelation(_ context.Context, relationID string, sourceID, targetID TaskID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.relations[relationID]
	if !ok {
		return fmt.Errorf("deps: relation not found: %s", relationID)
	}
	// Capture endpoints (authoritative from store, not caller trust alone).
	src, tgt := e.SourceID, e.TargetID
	if sourceID.Valid() && sourceID != src {
		return fmt.Errorf("deps: delete endpoint mismatch source want %s got %s", src, sourceID)
	}
	if targetID.Valid() && targetID != tgt {
		return fmt.Errorf("deps: delete endpoint mismatch target want %s got %s", tgt, targetID)
	}
	delete(m.relations, relationID)
	m.mutateGen++
	// Verify absence on BOTH ends.
	if containsRel(m.listLocked(string(src)), relationID) {
		return fmt.Errorf("%w: relation %s still on source after delete", ErrAmbiguousMutation, relationID)
	}
	if containsRel(m.listLocked(string(tgt)), relationID) {
		return fmt.Errorf("%w: relation %s still on target after delete", ErrAmbiguousMutation, relationID)
	}
	return nil
}

// SnapshotEdges returns all relations (test helper).
func (m *MemoryStore) SnapshotEdges() []DependencyEdge {
	snap, _ := m.SnapshotGraph(context.Background())
	if snap == nil {
		return nil
	}
	return snap.Edges
}

// SeedBlocks is a test helper: add A blocks B by ref.
func (m *MemoryStore) SeedBlocks(sourceRef, targetRef string) (DependencyEdge, error) {
	return m.CreateRelation(context.Background(), DependencyEdge{
		SourceRef: Ref(sourceRef),
		TargetRef: Ref(targetRef),
		Type:      EdgeBlocks,
	})
}

// EnsureTask creates a fixture task.
func (m *MemoryStore) EnsureTask(ref, status string, priority provider.Priority) {
	id := "id-" + strings.ToLower(ref)
	m.AddTask(&provider.Task{
		ID: id, Ref: ref, Title: ref, Status: provider.NormalizeStatus(status),
		Priority: priority, ProjectID: "fixture",
	})
}
