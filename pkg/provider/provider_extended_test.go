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
