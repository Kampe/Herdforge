package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type customTripper struct {
	targetURL *url.URL
}

func (c *customTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = c.targetURL.Scheme
	req.URL.Host = c.targetURL.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestGitHubProvider_GetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testowner/testrepo/issues/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"number": 42,
			"title": "GitHub Integration Issue",
			"body": "Issue details",
			"state": "open",
			"created_at": "2026-08-01T20:00:00Z",
			"labels": [{"name": "priority:urgent"}, {"name": "herd-smith"}]
		}`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	task, err := gp.GetTask(context.Background(), "42")
	if err != nil {
		t.Fatalf("expected task, got err: %v", err)
	}

	if task.Ref != "#42" || task.Priority != PriorityUrgent {
		t.Errorf("unexpected task output: %+v", task)
	}
}

func TestGitHubProvider_ListTasks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testowner/testrepo/issues" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("expected state=open, got %s", r.URL.Query().Get("state"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Empty termination required: only page 1 has items.
		if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(`[
			{"number":1,"title":"Issue 1","body":"Body 1","state":"open","created_at":"2026-08-01T20:00:00Z","labels":[{"name":"priority:high"}]},
			{"number":2,"title":"Issue 2","body":"Body 2","state":"closed","created_at":"2026-08-01T20:00:00Z","labels":[]}
		]`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	// List open (default)
	tasks, err := gp.ListTasks(context.Background(), "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	if tasks[0].Priority != PriorityHigh {
		t.Fatalf("expected high priority, got %v", tasks[0].Priority)
	}
}

func TestGitHubProvider_ListTasks_Closed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "closed" {
			t.Errorf("expected state=closed, got %s", r.URL.Query().Get("state"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	tasks, err := gp.ListTasks(context.Background(), "", "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestGitHubProvider_UpdateStatus(t *testing.T) {
	state := "open"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testowner/testrepo/issues/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			state = "closed"
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"number":42,"title":"t","body":"","state":"` + state + `","labels":[],"created_at":"2026-08-01T12:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	if err := gp.UpdateStatus(context.Background(), "42", "closed"); err != nil {
		t.Fatalf("expected clean update, got err: %v", err)
	}
}

func TestGitHubProvider_UpdateStatus_Open(t *testing.T) {
	var capturedBody string
	state := "closed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			buf := make([]byte, r.ContentLength)
			r.Body.Read(buf)
			capturedBody = string(buf)
			state = "open"
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"number":42,"title":"t","body":"","state":"` + state + `","labels":[],"created_at":"2026-08-01T12:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	if err := gp.UpdateStatus(context.Background(), "42", "open"); err != nil {
		t.Fatalf("expected clean update, got err: %v", err)
	}
	if capturedBody == "" {
		t.Fatal("expected a request body")
	}
}

func TestGitHubProvider_AddComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/testowner/testrepo/issues/42/comments" {
			t.Errorf("unexpected method or path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	if err := gp.AddComment(context.Background(), "42", "lgtm"); err != nil {
		t.Fatalf("expected clean comment add, got err: %v", err)
	}
}

func TestGitHubProvider_ClaimTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/testowner/testrepo/issues/42/comments" {
			t.Errorf("unexpected method or path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	if err := gp.ClaimTask(context.Background(), "42", "builder"); err != nil {
		t.Fatalf("expected clean claim, got err: %v", err)
	}
}

func TestGitHubProvider_GetTask_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	_, err := gp.GetTask(context.Background(), "999")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestGitHubProvider_UpdateStatus_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	err := gp.UpdateStatus(context.Background(), "42", "closed")
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestGitHubProvider_ListTasks_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	_, err := gp.ListTasks(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestGitHubProvider_AddComment_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	err := gp.AddComment(context.Background(), "42", "comment")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestGitHubProvider_GetTask_WithLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"number": 43,
			"title": "Label Test",
			"body": "",
			"state": "open",
			"created_at": "2026-08-01T20:00:00Z",
			"labels": [
				{"name": "priority:high"},
				{"name": "bug"},
				{"name": "low"}
			]
		}`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	task, err := gp.GetTask(context.Background(), "43")
	if err != nil {
		t.Fatalf("expected task, got err: %v", err)
	}
	if task.Priority != PriorityLow {
		t.Fatalf("expected low priority (last priority label wins), got %v", task.Priority)
	}
	if len(task.Labels) != 3 {
		t.Fatalf("expected 3 labels, got %d", len(task.Labels))
	}
}

func TestGitHubProvider_NoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"number":44,"title":"No Auth","body":"","state":"open","created_at":"2026-08-01T20:00:00Z","labels":[]}`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	task, err := gp.GetTask(context.Background(), "44")
	if err != nil {
		t.Fatalf("expected task, got err: %v", err)
	}
	if task.ID != "44" {
		t.Fatalf("expected task id 44, got %s", task.ID)
	}
}
