package provider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type MemoryProvider struct {
	mu        sync.Mutex
	tasks     map[string]*Task
	relations map[string]Relation // id → relation
	nextRel   int
}

func NewMemoryProvider() *MemoryProvider {
	return &MemoryProvider{
		tasks:     make(map[string]*Task),
		relations: make(map[string]Relation),
	}
}

func (m *MemoryProvider) AddTask(t *Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
}

func (m *MemoryProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		// Also allow lookup by ref.
		for _, cand := range m.tasks {
			if cand.Ref == id {
				cp := *cand
				return &cp, nil
			}
		}
		return nil, fmt.Errorf("task not found: %s", id)
	}
	cp := *t
	return &cp, nil
}

func (m *MemoryProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var res []*Task
	for _, t := range m.tasks {
		if projectID != "" && t.ProjectID != projectID {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		cp := *t
		res = append(res, &cp)
	}
	return res, nil
}

func (m *MemoryProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return m.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (m *MemoryProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	m.mu.Lock()
	t, ok := m.tasks[taskID]
	if !ok {
		// Resolve by ref.
		for id, cand := range m.tasks {
			if cand.Ref == taskID {
				t = cand
				taskID = id
				ok = true
				break
			}
		}
	}
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", taskID)
	}
	canonical := NormalizeStatus(status)
	t.Status = canonical
	m.mu.Unlock()
	// In-memory readback: re-load and verify (fail if map drift / missing).
	got, err := m.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("memory status readback after write: %w", err)
	}
	return VerifyStatusReadback(taskID, canonical, got.Status)
}

func (m *MemoryProvider) AddComment(ctx context.Context, taskID string, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[taskID]; !ok {
		for _, cand := range m.tasks {
			if cand.Ref == taskID {
				return nil
			}
		}
		return fmt.Errorf("task not found: %s", taskID)
	}
	return nil
}

// ListRelations implements RelationProvider (FAC-159).
func (m *MemoryProvider) ListRelations(ctx context.Context, taskID string) ([]Relation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Resolve ref → id.
	id := taskID
	if t, ok := m.tasks[taskID]; ok {
		id = t.ID
	} else {
		for _, cand := range m.tasks {
			if cand.Ref == taskID {
				id = cand.ID
				break
			}
		}
	}
	var out []Relation
	for _, r := range m.relations {
		if r.SourceTaskID == id || r.TargetTaskID == id || r.SourceTaskID == taskID || r.TargetTaskID == taskID {
			out = append(out, r)
		}
	}
	return out, nil
}

// CreateRelation implements RelationProvider with readback.
func (m *MemoryProvider) CreateRelation(ctx context.Context, sourceID, targetID string, typ RelationType) (*Relation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.resolveIDLocked(sourceID)
	tgt := m.resolveIDLocked(targetID)
	if src == "" {
		return nil, fmt.Errorf("task not found: %s", sourceID)
	}
	if tgt == "" {
		return nil, fmt.Errorf("task not found: %s", targetID)
	}
	if typ == "" {
		typ = RelationBlocks
	}
	m.nextRel++
	r := Relation{
		ID:           fmt.Sprintf("mem-rel-%d", m.nextRel),
		SourceTaskID: src,
		TargetTaskID: tgt,
		Type:         typ,
		CreatedAt:    time.Now().UTC(),
	}
	m.relations[r.ID] = r
	got, ok := m.relations[r.ID]
	if !ok {
		return nil, fmt.Errorf("memory CreateRelation readback missing")
	}
	return &got, nil
}

// DeleteRelation implements RelationProvider with absence verification.
func (m *MemoryProvider) DeleteRelation(ctx context.Context, relationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.relations[relationID]; !ok {
		return fmt.Errorf("relation not found: %s", relationID)
	}
	delete(m.relations, relationID)
	if _, ok := m.relations[relationID]; ok {
		return fmt.Errorf("memory DeleteRelation readback still present")
	}
	return nil
}

func (m *MemoryProvider) resolveIDLocked(idOrRef string) string {
	if t, ok := m.tasks[idOrRef]; ok {
		return t.ID
	}
	for _, cand := range m.tasks {
		if cand.Ref == idOrRef {
			return cand.ID
		}
	}
	return ""
}

