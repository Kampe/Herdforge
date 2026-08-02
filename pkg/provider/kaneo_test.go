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

func TestKaneoProvider_ListTasks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/task" || r.URL.Query().Get("projectId") != "proj-1" {
			t.Errorf("unexpected path or query: %s?%s", r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"id":"t-1","ref":"FAC-1","title":"Task 1","status":"to-do","priority":"high","projectId":"proj-1","createdAt":"2026-08-01T20:00:00Z","labels":[]},
			{"id":"t-2","ref":"FAC-2","title":"Task 2","status":"done","priority":"low","projectId":"proj-1","createdAt":"2026-08-01T20:00:00Z","labels":[]}
		]`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	tasks, err := kp.ListTasks(context.Background(), "proj-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	// Filter by status
	tasks, err = kp.ListTasks(context.Background(), "proj-1", "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t-2" {
		t.Fatalf("expected 1 done task, got %d", len(tasks))
	}
}

func TestKaneoProvider_AddComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/task/task-123/comment" {
			t.Errorf("unexpected method or path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	if err := kp.AddComment(context.Background(), "task-123", "reviewed"); err != nil {
		t.Fatalf("expected clean comment add, got err: %v", err)
	}
}

func TestKaneoProvider_ClaimTask(t *testing.T) {
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
	if err := kp.ClaimTask(context.Background(), "task-123", "builder"); err != nil {
		t.Fatalf("expected clean claim, got err: %v", err)
	}
}

func TestKaneoProvider_GetTask_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	_, err := kp.GetTask(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestKaneoProvider_UpdateStatus_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	err := kp.UpdateStatus(context.Background(), "task-123", "done")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestKaneoProvider_ListTasks_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	_, err := kp.ListTasks(context.Background(), "proj-1", "")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestKaneoProvider_AddComment_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	err := kp.AddComment(context.Background(), "task-123", "comment")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestResolveKaneoProjectID(t *testing.T) {
	projID := ResolveKaneoProjectID(".")
	if projID == "" {
		t.Log("no local kaneo link found, which is okay")
	}
}

func TestKaneoProvider_GetTask_BadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	// API returns bad JSON; CLI fallback will also fail since "kaneo" isn't installed
	_, err := kp.GetTask(context.Background(), "task-123")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

func TestKaneoProvider_ListTasks_BadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	_, err := kp.ListTasks(context.Background(), "proj-1", "")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

func TestKaneoProvider_UpdateStatus_NoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1")
	if err := kp.UpdateStatus(context.Background(), "task-123", "done"); err != nil {
		t.Fatalf("expected success on 204, got err: %v", err)
	}
}
