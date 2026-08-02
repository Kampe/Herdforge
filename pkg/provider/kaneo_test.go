package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKaneoProvider_GetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/task/task-123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"id": "task-123",
			"ref": "FAC-123",
			"title": "Test Task",
			"description": "Task Description",
			"status": "to-do",
			"priority": "high",
			"projectId": "proj-1",
			"createdAt": "2026-08-01T20:00:00Z",
			"labels": [{"name": "herd-smith"}]
		}`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	task, err := kp.GetTask(context.Background(), "task-123")
	if err != nil {
		t.Fatalf("expected task, got err: %v", err)
	}

	if task.Title != "Test Task" || task.Priority != PriorityHigh {
		t.Errorf("unexpected task fields: %+v", task)
	}
	if len(task.Labels) != 1 || task.Labels[0] != "herd-smith" {
		t.Errorf("unexpected labels: %v", task.Labels)
	}
}

func TestKaneoProvider_UpdateStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/task/task-123" {
			t.Errorf("unexpected method or path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	if err := kp.UpdateStatus(context.Background(), "task-123", "in-progress"); err != nil {
		t.Fatalf("expected clean update, got err: %v", err)
	}
}
