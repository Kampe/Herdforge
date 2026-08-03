package provider

import (
	"context"
	"fmt"
)

type MemoryProvider struct {
	tasks map[string]*Task
}

func NewMemoryProvider() *MemoryProvider {
	return &MemoryProvider{
		tasks: make(map[string]*Task),
	}
}

func (m *MemoryProvider) AddTask(t *Task) {
	m.tasks[t.ID] = t
}

func (m *MemoryProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	t, ok := m.tasks[id]
	if !ok {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return t, nil
}

func (m *MemoryProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	var res []*Task
	for _, t := range m.tasks {
		if projectID != "" && t.ProjectID != projectID {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		res = append(res, t)
	}
	return res, nil
}

func (m *MemoryProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return m.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (m *MemoryProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	t, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task not found: %s", taskID)
	}
	canonical := NormalizeStatus(status)
	t.Status = canonical
	// In-memory readback: re-load and verify (fail if map drift / missing).
	got, err := m.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("memory status readback after write: %w", err)
	}
	return VerifyStatusReadback(taskID, canonical, got.Status)
}

func (m *MemoryProvider) AddComment(ctx context.Context, taskID string, body string) error {
	_, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task not found: %s", taskID)
	}
	return nil
}
