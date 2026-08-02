package provider

import (
	"context"
	"testing"
	"time"
)

func TestDTOToTask(t *testing.T) {
	now := time.Now()
	task := DTOToTask("id-1", "REF-1", "Test", "Desc", "to-do", PriorityHigh, "proj-1", []string{"label1"}, now)
	if task.ID != "id-1" || task.Ref != "REF-1" || task.Title != "Test" {
		t.Errorf("unexpected DTO mapping: %+v", task)
	}
	if len(task.Labels) != 1 || task.Labels[0] != "label1" {
		t.Errorf("unexpected labels: %v", task.Labels)
	}
}

func TestVerifyProviderContract_Nil(t *testing.T) {
	err := VerifyProviderContract(context.Background(), nil, "proj-1")
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestParsePriorityString_AllCases(t *testing.T) {
	tests := []struct {
		input string
		want  Priority
	}{
		{"urgent", PriorityUrgent},
		{"priority:urgent", PriorityUrgent},
		{"1", PriorityUrgent},
		{"high", PriorityHigh},
		{"priority:high", PriorityHigh},
		{"2", PriorityHigh},
		{"low", PriorityLow},
		{"priority:low", PriorityLow},
		{"4", PriorityLow},
		{"unknown", PriorityMedium},
		{"medium", PriorityMedium},
	}
	for _, tt := range tests {
		got := ParsePriorityString(tt.input)
		if got != tt.want {
			t.Errorf("ParsePriorityString(%q) = %s, want %s", tt.input, got, tt.want)
		}
	}
}

func TestMemoryProvider_ClaimTask(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "t-1", Ref: "MEM-1", Title: "Claim Test", Status: "to-do", ProjectID: "p-1"})

	err := mp.ClaimTask(context.Background(), "t-1", "worker-1")
	if err != nil {
		t.Fatalf("expected clean claim, got err: %v", err)
	}

	// Verify task status changed
	task, _ := mp.GetTask(context.Background(), "t-1")
	if task.Status != "in-progress" {
		t.Errorf("expected status 'in-progress', got %s", task.Status)
	}
}

func TestMemoryProvider_AddComment(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "t-2", Title: "Comment Test", Status: "to-do", ProjectID: "p-1"})

	err := mp.AddComment(context.Background(), "t-2", "looks good")
	if err != nil {
		t.Fatalf("expected clean comment add, got err: %v", err)
	}
}

func TestMemoryProvider_UpdateStatus(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "t-3", Title: "Status Test", Status: "to-do", ProjectID: "p-1"})

	err := mp.UpdateStatus(context.Background(), "t-3", "in-progress")
	if err != nil {
		t.Fatalf("expected clean status update, got err: %v", err)
	}

	task, _ := mp.GetTask(context.Background(), "t-3")
	if task.Status != "in-progress" {
		t.Errorf("expected status 'in-progress', got %s", task.Status)
	}
}

func TestMemoryProvider_GetTask_NotFound(t *testing.T) {
	mp := NewMemoryProvider()
	_, err := mp.GetTask(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing task, got nil")
	}
}

func TestMemoryProvider_UpdateStatus_NotFound(t *testing.T) {
	mp := NewMemoryProvider()
	err := mp.UpdateStatus(context.Background(), "does-not-exist", "done")
	if err == nil {
		t.Fatal("expected error for missing task, got nil")
	}
}

func TestMemoryProvider_AddComment_NotFound(t *testing.T) {
	mp := NewMemoryProvider()
	err := mp.AddComment(context.Background(), "does-not-exist", "comment")
	if err == nil {
		t.Fatal("expected error for missing task, got nil")
	}
}

func TestMemoryProvider_ClaimTask_NotFound(t *testing.T) {
	mp := NewMemoryProvider()
	err := mp.ClaimTask(context.Background(), "does-not-exist", "worker")
	if err == nil {
		t.Fatal("expected error for missing task, got nil")
	}
}

func TestMemoryProvider_ListTasks_Empty(t *testing.T) {
	mp := NewMemoryProvider()
	tasks, err := mp.ListTasks(context.Background(), "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestMemoryProvider_ListTasks_FilterByProject(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "a-1", Title: "A", Status: "to-do", ProjectID: "p-1"})
	mp.AddTask(&Task{ID: "b-1", Title: "B", Status: "to-do", ProjectID: "p-2"})

	tasks, err := mp.ListTasks(context.Background(), "p-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "a-1" {
		t.Fatalf("expected 1 task for p-1, got %d", len(tasks))
	}
}

func TestMemoryProvider_ListTasks_FilterByStatus(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "a-2", Title: "A", Status: "to-do", ProjectID: "p-1"})
	mp.AddTask(&Task{ID: "b-2", Title: "B", Status: "done", ProjectID: "p-1"})

	tasks, err := mp.ListTasks(context.Background(), "", "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "b-2" {
		t.Fatalf("expected 1 done task, got %d", len(tasks))
	}
}

func TestMemoryProvider_ListTasks_ProjectAndStatus(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "a-3", Title: "A", Status: "to-do", ProjectID: "p-1"})
	mp.AddTask(&Task{ID: "b-3", Title: "B", Status: "done", ProjectID: "p-1"})
	mp.AddTask(&Task{ID: "c-3", Title: "C", Status: "done", ProjectID: "p-2"})

	tasks, err := mp.ListTasks(context.Background(), "p-1", "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "b-3" {
		t.Fatalf("expected 1 task (p-1 + done), got %d", len(tasks))
	}
}
