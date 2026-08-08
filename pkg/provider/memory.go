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
	comments  map[string][]string
	labels    map[string]TaskLabel
	nextLabel int
	attachIDs []string
	proofed   map[string]bool
	relations map[string]Relation // id → relation
	nextRel   int
}

func (m *MemoryProvider) LabelMutationAuthority() (string, error) {
	return "memory-provider/project", nil
}

func NewMemoryProvider() *MemoryProvider {
	return &MemoryProvider{
		comments:  make(map[string][]string),
		tasks:     make(map[string]*Task),
		labels:    make(map[string]TaskLabel),
		relations: make(map[string]Relation),
		proofed:   make(map[string]bool),
	}
}

func (m *MemoryProvider) ListTaskLabels(_ context.Context, taskID string) ([]TaskLabel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TaskLabel
	for _, l := range m.labels {
		if l.TaskID == taskID {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryProvider) CreateTaskLabel(_ context.Context, taskID, name string) (TaskLabel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[taskID]; !ok {
		return TaskLabel{}, fmt.Errorf("task not found: %s", taskID)
	}
	m.nextLabel++
	// Kaneo creates an unattached row. Ownership is established only by attach.
	l := TaskLabel{ID: fmt.Sprintf("label-%d", m.nextLabel), Name: name}
	m.labels[l.ID] = l
	return l, nil
}

func (m *MemoryProvider) ProveLabelCreation(_ context.Context, created TaskLabel, targetID, name string, opts LabelRepairOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.labels[created.ID]
	if !ok || m.proofed[created.ID] || l.ID != created.ID || l.Name != name || l.TaskID != "" || created.TaskID != "" {
		return fmt.Errorf("label creation proof failed: foreign or attached identity")
	}
	if targetID == "" || (opts.Evidence != nil && (opts.TransactionID == "" || opts.Generation == "")) {
		return fmt.Errorf("label creation proof missing target or generation")
	}
	m.proofed[created.ID] = true
	return nil
}

func (m *MemoryProvider) AttachTaskLabel(_ context.Context, taskID, labelID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.labels[labelID]
	if !ok {
		return fmt.Errorf("label not found: %s", labelID)
	}
	// Kaneo's observed destructive semantics: attaching an already-owned row
	// moves it. Repair must prevent this call for source-owned IDs.
	oldTask := l.TaskID
	l.TaskID = taskID
	m.labels[labelID] = l
	m.attachIDs = append(m.attachIDs, labelID)
	if oldTask != "" {
		m.syncTaskLabelsLocked(oldTask)
	}
	m.syncTaskLabelsLocked(taskID)
	return nil
}

func (m *MemoryProvider) DetachTaskLabel(_ context.Context, labelID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.labels[labelID]
	if !ok {
		return nil
	}
	oldTask := l.TaskID
	l.TaskID = ""
	m.labels[labelID] = l
	if oldTask != "" {
		m.syncTaskLabelsLocked(oldTask)
	}
	return nil
}

func (m *MemoryProvider) DeleteTaskLabel(_ context.Context, labelID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	oldTask := ""
	if l, ok := m.labels[labelID]; ok {
		oldTask = l.TaskID
	}
	delete(m.labels, labelID)
	if oldTask != "" {
		m.syncTaskLabelsLocked(oldTask)
	}
	return nil
}

func (m *MemoryProvider) syncTaskLabelsLocked(taskID string) {
	t := m.tasks[taskID]
	if t == nil {
		return
	}
	labels := make([]string, 0)
	for _, l := range m.labels {
		if l.TaskID == taskID {
			labels = append(labels, l.Name)
		}
	}
	sort.Strings(labels)
	t.Labels = labels
}

func (m *MemoryProvider) AddTask(t *Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
}

// Comments returns recorded comments for tests (FAC-147 fence coverage).
func (m *MemoryProvider) Comments(taskID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.comments[taskID]...)
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
	return m.UpdateStatusAtomic(ctx, taskID, status, "")
}

// UpdateStatusAtomic applies status and optional signed receipt in one step
// (hermetic board model of Kaneo single-PATCH atomicity).
// Resolves taskID by Ref when the map lookup misses (FAC-159 ref-keyed callers).
func (m *MemoryProvider) UpdateStatusAtomic(ctx context.Context, taskID, status, receiptJSON string) error {
	m.mu.Lock()
	t, ok := m.tasks[taskID]
	if !ok {
		// FAC-159: resolve by Ref when the map lookup misses.
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
	t.UpdatedAt = time.Now().UTC()
	if receiptJSON != "" {
		t.StatusReceipt = receiptJSON
		t.Description = EmbedStatusReceiptInDescription(t.Description, receiptJSON)
	}
	m.mu.Unlock()
	got, err := m.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("memory status readback after write: %w", err)
	}
	return VerifyStatusReadback(taskID, canonical, got.Status)
}

func (m *MemoryProvider) AddComment(ctx context.Context, taskID string, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := taskID
	if _, ok := m.tasks[id]; !ok {
		found := false
		for tid, cand := range m.tasks {
			if cand.Ref == taskID {
				id = tid
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("task not found: %s", taskID)
		}
	}
	if m.comments == nil {
		m.comments = map[string][]string{}
	}
	m.comments[id] = append(m.comments[id], body)
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

// ListComments implements CommentReader for exact effect readback.
func (m *MemoryProvider) ListComments(_ context.Context, taskID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[taskID]; !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	out := make([]string, len(m.comments[taskID]))
	copy(out, m.comments[taskID])
	return out, nil
}
