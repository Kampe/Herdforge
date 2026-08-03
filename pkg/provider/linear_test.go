package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLinearProvider_GetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"data": {
				"issue": {
					"id": "lin-100",
					"identifier": "ENG-100",
					"title": "Linear Test Ticket",
					"description": "Linear Desc",
					"priority": 1,
					"state": {"name": "Todo"},
					"project": {"id": "proj-lin"},
					"labels": {"nodes": [{"name": "backend"}]}
				}
			}
		}`))
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	task, err := lp.GetTask(context.Background(), "lin-100")
	if err != nil {
		t.Fatalf("expected task, got error: %v", err)
	}
	if task.ID != "lin-100" || task.Title != "Linear Test Ticket" || task.Ref != "ENG-100" {
		t.Fatalf("unexpected task: %+v", task)
	}
	if task.Priority != PriorityUrgent {
		t.Fatalf("expected urgent priority for priority=1, got %v", task.Priority)
	}
	if len(task.Labels) != 1 || task.Labels[0] != "backend" {
		t.Fatalf("unexpected labels: %v", task.Labels)
	}
}

func TestLinearProvider_ListTasks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"data": {
				"issues": {
					"nodes": [
						{
							"id": "lin-1",
							"identifier": "ENG-1",
							"title": "Task 1",
							"description": "Desc 1",
							"priority": 2,
							"state": {"name": "In Progress"},
							"project": {"id": "proj-lin"},
							"labels": {"nodes": []}
						},
						{
							"id": "lin-2",
							"identifier": "ENG-2",
							"title": "Task 2",
							"description": "Desc 2",
							"priority": 4,
							"state": {"name": "Todo"},
							"project": {"id": "other"},
							"labels": {"nodes": [{"name": "frontend"}]}
						}
					]
				}
			}
		}`))
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	// List all
	tasks, err := lp.ListTasks(context.Background(), "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	// Filter by project
	tasks, err = lp.ListTasks(context.Background(), "proj-lin", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "lin-1" {
		t.Fatalf("expected 1 project-filtered task, got %d", len(tasks))
	}
	if tasks[0].Priority != PriorityHigh {
		t.Fatalf("expected high priority for priority=2, got %v", tasks[0].Priority)
	}

	// Filter by status
	tasks, err = lp.ListTasks(context.Background(), "", "Todo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "lin-2" {
		t.Fatalf("expected 1 status-filtered task, got %d", len(tasks))
	}
	if tasks[0].Priority != PriorityLow {
		t.Fatalf("expected low priority for priority=4, got %v", tasks[0].Priority)
	}
}

func TestLinearProvider_UpdateStatus(t *testing.T) {
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		n++
		if n == 1 {
			// mutation
			w.Write([]byte(`{"data": {"issueUpdate": {"success": true}}}`))
			return
		}
		// readback GetTask
		w.Write([]byte(`{"data":{"issue":{"id":"lin-1","identifier":"LIN-1","title":"t","description":"","priority":2,"state":{"name":"In Progress"},"project":{"id":"p1"},"labels":{"nodes":[]}}}}`))
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	err := lp.UpdateStatus(context.Background(), "lin-1", "In Progress")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLinearProvider_AddComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data": {"commentCreate": {"success": true}}}`))
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	err := lp.AddComment(context.Background(), "lin-1", "Nice work!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLinearProvider_ClaimTask(t *testing.T) {
	var capturedBody string
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n++
		if n == 1 {
			buf := make([]byte, r.ContentLength)
			r.Body.Read(buf)
			capturedBody = string(buf)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data": {"issueUpdate": {"success": true}}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"issue":{"id":"lin-1","identifier":"LIN-1","title":"t","description":"","priority":2,"state":{"name":"In Progress"},"project":{"id":"p1"},"labels":{"nodes":[]}}}}`))
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	err := lp.ClaimTask(context.Background(), "lin-1", "developer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedBody == "" {
		t.Fatalf("expected claim to call UpdateStatus which sends a request")
	}
}

func TestLinearProvider_GetTask_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	_, err := lp.GetTask(context.Background(), "lin-404")
	if err == nil {
		t.Fatalf("expected error on 404, got nil")
	}
}

func TestLinearProvider_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	err := lp.UpdateStatus(context.Background(), "lin-1", "In Progress")
	if err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
}
