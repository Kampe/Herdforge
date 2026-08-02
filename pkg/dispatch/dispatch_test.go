package dispatch

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type mockTaskProvider struct {
	tasks []*provider.Task
}

func (m *mockTaskProvider) GetTask(_ context.Context, id string) (*provider.Task, error) {
	for _, t := range m.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, nil
}
func (m *mockTaskProvider) ListTasks(_ context.Context, _, _ string) ([]*provider.Task, error) {
	return m.tasks, nil
}

func (m *mockTaskProvider) ClaimTask(_ context.Context, _, _ string) error { return nil }
func (m *mockTaskProvider) UpdateStatus(_ context.Context, _, _ string) error {
	return nil
}
func (m *mockTaskProvider) AddComment(_ context.Context, _, _ string) error { return nil }

func TestFindTicket(t *testing.T) {
	tp := &mockTaskProvider{
		tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Title: "First task", Status: "to-do", Priority: "high"},
			{ID: "2", Ref: "FAC-2", Title: "Second task", Status: "to-do", Priority: "medium"},
		},
	}
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "deepseek-v4-flash", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
	}
	wm := worktree.NewWorktreeManager(".")
	d := NewDispatcher(cfg, tp, wm)

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})
	if err == nil || !os.IsNotExist(err) {
		if err != nil && strings.Contains(err.Error(), "ticket FAC-1 not found") {
			t.Fatalf("ticket should have been found: %v", err)
		}
	}

	_, err = d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-NONEXIST", NoLaunch: true})
	if err == nil {
		t.Fatal("expected error for non-existent ticket")
	}
}

func TestSlugForTask(t *testing.T) {
	cases := []struct {
		ref   string
		title string
		want  string
	}{
		{"FAC-33", "Port herd-next priority-ordered action picker", "fac-33-port-herd-next-priority-ordered-action-picker"},
		{"FAC-1", "Hello World", "fac-1-hello-world"},
	}
	for _, c := range cases {
		got := slugForTask(c.ref, c.title)
		if got != c.want {
			t.Errorf("slugForTask(%q, %q) = %q, want %q", c.ref, c.title, got, c.want)
		}
	}
}

func TestBuildTaskPacket(t *testing.T) {
	task := &provider.Task{
		Ref:         "FAC-33",
		Title:       "Test task",
		Description: "Do the thing",
		Status:      "to-do",
		Priority:    "high",
		Labels:      []string{"go", "core"},
	}
	lane := &config.LaneDef{
		Name: "worker", Role: "worker", AgentKind: "opencode",
		Model: "deepseek-v4-flash", Prompt: ".herd/prompts/worker.md",
	}
	packet := buildTaskPacket(task, "/tmp/wt", "task/foo", ".herd/prompts/worker.md", lane)
	if !strings.Contains(packet, "FAC-33") {
		t.Error("packet should contain ticket ref")
	}
	if !strings.Contains(packet, "/tmp/wt") {
		t.Error("packet should contain worktree path")
	}
	// FAC-115: reference-based, NOT an inline spec dump — the agent reads the
	// card itself, and the packet must be tight (context-budget fix).
	if !strings.Contains(packet, "kaneo task get FAC-33") {
		t.Error("packet must tell the agent to read the card by reference")
	}
	if strings.Contains(packet, "Do the thing") {
		t.Error("packet must NOT dump the card description inline (burns agent context)")
	}
	if !strings.Contains(packet, "herd verify") {
		t.Error("packet must include the self-verify completion contract")
	}
	if lines := strings.Count(packet, "\n"); lines > 25 {
		t.Errorf("packet must stay tight (<25 lines), got %d", lines)
	}
}
