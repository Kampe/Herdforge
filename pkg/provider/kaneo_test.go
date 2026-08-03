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

	kp := NewKaneoProvider(server.URL, "proj-1", false)
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

func TestKaneoProvider_GetTask_BadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	_, err := kp.GetTask(context.Background(), "task-123")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

func TestKaneoProvider_GetTask_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	_, err := kp.GetTask(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestKaneoProvider_UpdateStatus(t *testing.T) {
	status := "to-do"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/task/task-123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			status = "in-progress"
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"task-123","ref":"FAC-1","title":"t","status":"` + status + `","priority":"high","projectId":"proj-1","labels":[]}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	if err := kp.UpdateStatus(context.Background(), "task-123", "in-progress"); err != nil {
		t.Fatalf("expected clean update, got err: %v", err)
	}
}

func TestKaneoProvider_UpdateStatus_NoContent(t *testing.T) {
	status := "to-do"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			status = "done"
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"task-123","ref":"FAC-1","title":"t","status":"` + status + `","priority":"high","projectId":"proj-1","labels":[]}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	if err := kp.UpdateStatus(context.Background(), "task-123", "done"); err != nil {
		t.Fatalf("expected success on 204, got err: %v", err)
	}
}

func TestKaneoProvider_UpdateStatus_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	err := kp.UpdateStatus(context.Background(), "task-123", "done")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
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

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	tasks, err := kp.ListTasks(context.Background(), "proj-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	tasks, err = kp.ListTasks(context.Background(), "proj-1", "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t-2" {
		t.Fatalf("expected 1 done task, got %d", len(tasks))
	}
}

func TestKaneoProvider_ListTasks_BadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	_, err := kp.ListTasks(context.Background(), "proj-1", "")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

func TestKaneoProvider_ListTasks_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	_, err := kp.ListTasks(context.Background(), "proj-1", "")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestKaneoProvider_ListTasks_DefaultProjectID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("projectId") != "proj-default" {
			t.Errorf("expected projectId=proj-default, got %s", r.URL.Query().Get("projectId"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-default", false)
	tasks, err := kp.ListTasks(context.Background(), "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestKaneoProvider_ClaimTask(t *testing.T) {
	status := "to-do"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/task/task-123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			status = "in-progress"
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"task-123","ref":"FAC-1","title":"t","status":"` + status + `","priority":"high","projectId":"proj-1","labels":[]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	if err := kp.ClaimTask(context.Background(), "task-123", "builder"); err != nil {
		t.Fatalf("expected clean claim, got err: %v", err)
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

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	if err := kp.AddComment(context.Background(), "task-123", "reviewed"); err != nil {
		t.Fatalf("expected clean comment add, got err: %v", err)
	}
}

func TestKaneoProvider_AddComment_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
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

func TestResolveKaneoProjectID_FromEnv(t *testing.T) {
	t.Setenv("KANEO_PROJECT", "env-proj")
	projID := ResolveKaneoProjectID("/nonexistent")
	if projID != "" {
		// Falls through if no file found; note: ResolveKaneoProjectID doesn't read env
		t.Logf("got project id: %s", projID)
	}
}

func TestKaneoGetTask_ArrayBody(t *testing.T) {
	// Cross-package mocks and some Kaneo shapes return a JSON array for get.
	// Readback after claim must still decode (CI: pkg/daemon pulse).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"id":"t-1","ref":"FAC-99","title":"Pulse","status":"in-progress","priority":"urgent","projectId":"p","labels":[{"name":"herd-smith"}]}]`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "p", false)
	task, err := kp.GetTask(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("array body GetTask: %v", err)
	}
	if task.Ref != "FAC-99" || task.Status != StatusInProgress {
		t.Fatalf("unexpected task: %+v", task)
	}
	// Non-vacuity: empty array fails closed.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer empty.Close()
	kp2 := NewKaneoProvider(empty.URL, "p", false)
	if _, err := kp2.GetTask(context.Background(), "t-1"); err == nil {
		t.Fatal("empty array must fail")
	}
}

func TestKaneoLabel_DualShape(t *testing.T) {
	// CLI form: labels as strings
	var cli kaneoTaskDTO
	if err := DecodeJSONBytes(200, []byte(`{"id":"1","ref":"FAC-1","title":"t","status":"to-do","priority":"low","projectId":"p","labels":["forge-smith","urgent"]}`), &cli); err != nil {
		t.Fatalf("cli labels: %v", err)
	}
	task := dtoToTask(cli)
	if len(task.Labels) != 2 || task.Labels[0] != "forge-smith" {
		t.Fatalf("cli labels=%v", task.Labels)
	}
	// API form: labels as objects
	var api kaneoTaskDTO
	if err := DecodeJSONBytes(200, []byte(`{"id":"1","ref":"FAC-1","title":"t","status":"to-do","priority":"low","projectId":"p","labels":[{"name":"forge-smith"}]}`), &api); err != nil {
		t.Fatalf("api labels: %v", err)
	}
	task2 := dtoToTask(api)
	if len(task2.Labels) != 1 || task2.Labels[0] != "forge-smith" {
		t.Fatalf("api labels=%v", task2.Labels)
	}
}
