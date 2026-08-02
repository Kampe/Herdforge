package next

import (
	"context"
	"sync"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type testTask struct {
	ref      string
	title    string
	status   string
	priority string
}

type testProvider struct {
	mu    sync.Mutex
	tasks []testTask
}

func newTestProvider(tasks []testTask) *testProvider {
	return &testProvider{tasks: tasks}
}

func (tp *testProvider) GetTask(_ context.Context, id string) (*provider.Task, error) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for _, t := range tp.tasks {
		if t.ref == id {
			return &provider.Task{
				ID:       t.ref,
				Ref:      t.ref,
				Title:    t.title,
				Status:   t.status,
				Priority: provider.Priority(t.priority),
			}, nil
		}
	}
	return nil, nil
}
func (tp *testProvider) ListTasks(_ context.Context, _, statusFilter string) ([]*provider.Task, error) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []*provider.Task
	for _, t := range tp.tasks {
		if statusFilter == "" || t.status == statusFilter {
			out = append(out, &provider.Task{
				ID:       t.ref,
				Ref:      t.ref,
				Title:    t.title,
				Status:   t.status,
				Priority: provider.Priority(t.priority),
			})
		}
	}
	return out, nil
}

func (tp *testProvider) ClaimTask(_ context.Context, _, _ string) error { return nil }
func (tp *testProvider) UpdateStatus(_ context.Context, _, _ string) error {
	return nil
}
func (tp *testProvider) AddComment(_ context.Context, _, _ string) error { return nil }

func testConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{
			Name:          "test",
			DefaultBranch: "main",
		},
		TaskProvider: config.TaskProvider{
			Type:      "memory",
			ProjectID: "test-project",
		},
	}
}
