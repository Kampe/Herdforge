package provider

import (
	"context"
	"fmt"
	"sort"
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
	// Board status is not the cross-process fence (pkg/claim SQLite lease is).
	// FAC-147 will add provider CAS; until then this remains a plain transition.
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

// ListProjectRelations implements BulkRelationProvider — O(edges) in-memory.
func (m *MemoryProvider) ListProjectRelations(ctx context.Context, projectID string) ([]Relation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Relation, 0, len(m.relations))
	for _, r := range m.relations {
		// Optional project filter when tasks carry ProjectID.
		if projectID != "" {
			src := m.tasks[r.SourceTaskID]
			tgt := m.tasks[r.TargetTaskID]
			if src != nil && src.ProjectID != "" && src.ProjectID != projectID {
				continue
			}
			if tgt != nil && tgt.ProjectID != "" && tgt.ProjectID != projectID {
				continue
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
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

// CreateRelation implements RelationProvider with dual-end readback.
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
	if src == tgt {
		return nil, fmt.Errorf("memory CreateRelation: self-edge rejected")
	}
	if typ == "" {
		return nil, fmt.Errorf("memory CreateRelation: type required")
	}
	if !ValidRelationType(typ) {
		return nil, fmt.Errorf("memory CreateRelation: unknown type %q", typ)
	}
	// Idempotent: return existing.
	for _, r := range m.relations {
		if r.SourceTaskID == src && r.TargetTaskID == tgt && r.Type == typ {
			cp := r
			return &cp, nil
		}
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
	// Dual-end presence check.
	if !m.relOnTaskLocked(src, r.ID) || !m.relOnTaskLocked(tgt, r.ID) {
		return nil, fmt.Errorf("memory CreateRelation dual readback failed")
	}
	got := m.relations[r.ID]
	return &got, nil
}

// DeleteRelation implements RelationProvider with dual-end absence verification.
func (m *MemoryProvider) DeleteRelation(ctx context.Context, relationID, sourceID, targetID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.relations[relationID]
	if !ok {
		// Already gone: verify both ends absent.
		src := m.resolveIDLocked(sourceID)
		tgt := m.resolveIDLocked(targetID)
		if src != "" && m.relOnTaskLocked(src, relationID) {
			return fmt.Errorf("memory DeleteRelation: missing map but present on source")
		}
		if tgt != "" && m.relOnTaskLocked(tgt, relationID) {
			return fmt.Errorf("memory DeleteRelation: missing map but present on target")
		}
		return nil
	}
	src, tgt := r.SourceTaskID, r.TargetTaskID
	if sourceID != "" {
		if s := m.resolveIDLocked(sourceID); s != "" && s != src {
			return fmt.Errorf("memory DeleteRelation: source endpoint mismatch")
		}
	}
	if targetID != "" {
		if t := m.resolveIDLocked(targetID); t != "" && t != tgt {
			return fmt.Errorf("memory DeleteRelation: target endpoint mismatch")
		}
	}
	delete(m.relations, relationID)
	if m.relOnTaskLocked(src, relationID) || m.relOnTaskLocked(tgt, relationID) {
		return fmt.Errorf("memory DeleteRelation: still present after delete")
	}
	return nil
}

func (m *MemoryProvider) relOnTaskLocked(taskID, relationID string) bool {
	for _, r := range m.relations {
		if r.ID == relationID && (r.SourceTaskID == taskID || r.TargetTaskID == taskID) {
			return true
		}
	}
	return false
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
