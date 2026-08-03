package deps

import (
	"context"
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
		// Allow id-as-ref for fixtures.
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
	want := string(taskID)
	// Resolve ref → id if needed.
	if id, ok := m.byRef[want]; ok {
		want = id
	}
	var out []DependencyEdge
	for _, e := range m.relations {
		if string(e.SourceID) == want || string(e.TargetID) == want ||
			string(e.SourceRef) == string(taskID) || string(e.TargetRef) == string(taskID) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelationID < out[j].RelationID })
	return out, nil
}

func (m *MemoryStore) CreateRelation(ctx context.Context, edge DependencyEdge) (DependencyEdge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if edge.Type == "" {
		edge.Type = EdgeBlocks
	}
	// Resolve IDs from refs when missing.
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
	// Fill refs from tasks when only IDs given.
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
	m.nextRel++
	edge.RelationID = fmt.Sprintf("mem-rel-%d", m.nextRel)
	m.relations[edge.RelationID] = edge

	// Readback
	got, ok := m.relations[edge.RelationID]
	if !ok {
		return DependencyEdge{}, fmt.Errorf("deps: create readback missing relation %s", edge.RelationID)
	}
	if got.Key() != edge.Key() {
		return DependencyEdge{}, fmt.Errorf("deps: create readback mismatch want %s got %s", edge.Key(), got.Key())
	}
	return got, nil
}

func (m *MemoryStore) DeleteRelation(_ context.Context, relationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.relations[relationID]; !ok {
		return fmt.Errorf("deps: relation not found: %s", relationID)
	}
	delete(m.relations, relationID)
	if _, ok := m.relations[relationID]; ok {
		return fmt.Errorf("deps: delete readback still present: %s", relationID)
	}
	return nil
}

// SnapshotEdges returns all relations (test helper).
func (m *MemoryStore) SnapshotEdges() []DependencyEdge {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DependencyEdge, 0, len(m.relations))
	for _, e := range m.relations {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelationID < out[j].RelationID })
	return out
}

// SeedBlocks is a test helper: add A blocks B by ref.
func (m *MemoryStore) SeedBlocks(sourceRef, targetRef string) (DependencyEdge, error) {
	return m.CreateRelation(context.Background(), DependencyEdge{
		SourceRef: Ref(sourceRef),
		TargetRef: Ref(targetRef),
		Type:      EdgeBlocks,
	})
}

// Ensure tasks exist for refs used in fixtures.
func (m *MemoryStore) EnsureTask(ref, status string, priority provider.Priority) {
	id := "id-" + strings.ToLower(ref)
	m.AddTask(&provider.Task{
		ID: id, Ref: ref, Title: ref, Status: provider.NormalizeStatus(status),
		Priority: priority, ProjectID: "fixture",
	})
}
