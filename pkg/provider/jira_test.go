package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJiraProvider_GetTaskAndListTasks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/JIRA-100"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"id": "100",
				"key": "JIRA-100",
				"fields": {
					"summary": "Fix auth vulnerability",
					"description": "Fix OAuth token validation",
					"status": {"name": "In Progress"},
					"priority": {"name": "high"},
					"labels": ["security"],
					"created": "2026-08-01T12:00:00Z",
					"project": {"key": "PROJ"}
				}
			}`))
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"issues": [
					{
						"id": "100",
						"key": "JIRA-100",
						"fields": {
							"summary": "Fix auth vulnerability",
							"description": "Fix OAuth token validation",
							"status": {"name": "In Progress"},
							"priority": {"name": "high"},
							"labels": ["security"],
							"created": "2026-08-01T12:00:00Z",
							"project": {"key": "PROJ"}
						}
					}
				]
			}`))
		case strings.Contains(r.URL.Path, "/transitions"), strings.Contains(r.URL.Path, "/comment"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	defer ts.Close()

	jp := NewJiraProvider(ts.URL, "user@example.com", "token123")
	task, err := jp.GetTask(context.Background(), "JIRA-100")
	if err != nil || task == nil {
		t.Fatalf("expected clean GetTask, got err: %v", err)
	}

	if task.Ref != "JIRA-100" || task.Priority != PriorityHigh {
		t.Errorf("unexpected task fields: %+v", task)
	}

	tasks, err := jp.ListTasks(context.Background(), "PROJ", "In Progress")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected 1 task listed, got %d (err: %v)", len(tasks), err)
	}

	if err := jp.ClaimTask(context.Background(), "100", "smith"); err != nil {
		t.Errorf("expected clean ClaimTask, got err: %v", err)
	}

	if err := jp.UpdateStatus(context.Background(), "100", "Done"); err != nil {
		t.Errorf("expected clean UpdateStatus, got err: %v", err)
	}
}

func TestJiraProvider_AddComment(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	jp := NewJiraProvider(ts.URL, "user@example.com", "token123")
	if err := jp.AddComment(context.Background(), "100", "done"); err != nil {
		t.Errorf("expected clean AddComment, got err: %v", err)
	}
}

func TestJiraProvider_DoRequest_Non200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	jp := NewJiraProvider(ts.URL, "user@example.com", "token123")
	_, err := jp.doRequest(context.Background(), "GET", "/rest/api/3/issue/999", nil)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestJiraProvider_GetTask_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	}))
	defer ts.Close()

	jp := NewJiraProvider(ts.URL, "user@example.com", "token123")
	_, err := jp.GetTask(context.Background(), "999")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

func TestJiraProvider_UpdateStatus_CancelledContext(t *testing.T) {
	jp := NewJiraProvider("http://localhost:1", "user@example.com", "token123")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := jp.UpdateStatus(ctx, "100", "Done")
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
}
