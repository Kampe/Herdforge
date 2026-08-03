package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestLinearProvider_ListTasks_PaginatesServerConstrainedAndSorts(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request linearGraphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !strings.Contains(request.Query, "issues(first: 100, after: $after, filter: { project: { id: { eq: $projectID } } })") {
			t.Fatalf("list query must constrain project server-side: %s", request.Query)
		}
		if request.Variables["projectID"] != "proj-lin" {
			t.Fatalf("projectID=%q", request.Variables["projectID"])
		}
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			if request.Variables["after"] != nil {
				t.Fatalf("first cursor=%#v, want nil", request.Variables["after"])
			}
			_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[
				{"id":"lin-10","identifier":"ENG-10","title":"high","priority":2,"state":{"name":"Todo"},"project":{"id":"proj-lin"},"labels":{"nodes":[]}},
				{"id":"lin-2","identifier":"ENG-2","title":"urgent","priority":1,"state":{"name":"Todo"},"project":{"id":"proj-lin"},"labels":{"nodes":[]}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`))
		case 2:
			if request.Variables["after"] != "cursor-1" {
				t.Fatalf("second cursor=%#v, want cursor-1", request.Variables["after"])
			}
			_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[
				{"id":"lin-1","identifier":"ENG-1","title":"urgent first","priority":1,"state":{"name":"Todo"},"project":{"id":"proj-lin"},"labels":{"nodes":[]}},
				{"id":"lin-5","identifier":"ENG-5","title":"medium","priority":3,"state":{"name":"Todo"},"project":{"id":"proj-lin"},"labels":{"nodes":[]}}
			],"pageInfo":{"hasNextPage":false,"endCursor":"cursor-2"}}}}`))
		default:
			t.Fatalf("unexpected page %d", calls)
		}
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL
	tasks, err := lp.ListTasks(context.Background(), " proj-lin ", "")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if got, want := []string{tasks[0].Ref, tasks[1].Ref, tasks[2].Ref, tasks[3].Ref}, []string{"ENG-1", "ENG-2", "ENG-10", "ENG-5"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v, want=%v", got, want)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestLinearProvider_UpdateStatus(t *testing.T) {
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		n++
		switch n {
		case 1:
			w.Write([]byte(`{"data":{"issue":{"id":"lin-1","team":{"states":{"nodes":[{"id":"progress-state","name":"In Progress","type":"started"}]}}}}}`))
		case 2:
			w.Write([]byte(`{"data": {"issueUpdate": {"success": true}}}`))
		default:
			// readback GetTask
			w.Write([]byte(`{"data":{"issue":{"id":"lin-1","identifier":"LIN-1","title":"t","description":"","priority":2,"state":{"name":"In Progress"},"project":{"id":"p1"},"labels":{"nodes":[]}}}}`))
		}
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

func TestLinearProvider_UpdateStatus_ResolvesWorkflowStateIDAndReadsBack(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request linearGraphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.Query, "ResolveIssueWorkflowStates"):
			w.Write([]byte(`{"data":{"issue":{"id":"lin-1","team":{"states":{"nodes":[{"id":"todo-state","name":"Todo","type":"unstarted"},{"id":"progress-state","name":"In Progress","type":"started"}]}}}}}`))
		case strings.Contains(request.Query, "mutation UpdateIssueState"):
			if got := request.Variables["state"]; got != "progress-state" {
				t.Errorf("mutation state=%q, want resolved workflow state ID", got)
			}
			w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		case strings.Contains(request.Query, "query GetIssue"):
			w.Write([]byte(`{"data":{"issue":{"id":"lin-1","identifier":"LIN-1","title":"t","description":"","priority":2,"state":{"name":"In Progress"},"project":{"id":"p1"},"labels":{"nodes":[]}}}}`))
		default:
			t.Errorf("unexpected GraphQL query: %s", request.Query)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	if err := lp.UpdateStatus(context.Background(), "lin-1", StatusInProgress); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want state resolution + mutation + readback", calls)
	}
}

func TestLinearProvider_ListTasks_RejectsMissingProjectID(t *testing.T) {
	lp := NewLinearProvider("mock-api-key")
	if _, err := lp.ListTasks(context.Background(), " \t ", ""); err == nil {
		t.Fatal("blank project ID must fail before an unscoped query")
	}
}

func TestLinearProvider_ListTasks_RejectsRepeatedCursor(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"issues":{"nodes":[{"id":"lin-%d","identifier":"ENG-%d","priority":3,"state":{"name":"Todo"},"project":{"id":"proj-lin"},"labels":{"nodes":[]}}],"pageInfo":{"hasNextPage":true,"endCursor":"repeat"}}}}`, calls, calls)
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL
	if _, err := lp.ListTasks(context.Background(), "proj-lin", ""); !errors.Is(err, ErrDuplicatePage) || !strings.Contains(err.Error(), "repeated cursor") {
		t.Fatalf("repeated cursor must hard-fail, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestLinearProvider_ListTasks_CapFailsClosed(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"issues":{"nodes":[{"id":"lin-%d","identifier":"ENG-%d","priority":3,"state":{"name":"Todo"},"project":{"id":"proj-lin"},"labels":{"nodes":[]}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-%d"}}}}`, calls, calls, calls)
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL
	_, err := lp.ListTasks(context.Background(), "proj-lin", "")
	if !errors.Is(err, ErrPaginationCap) {
		t.Fatalf("want pagination cap error, got %v", err)
	}
	if calls != DefaultMaxListPages {
		t.Fatalf("calls=%d, want cap %d", calls, DefaultMaxListPages)
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

func TestLinearProvider_AddComment_RejectsSuccessFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":false}}}`))
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL
	if err := lp.AddComment(context.Background(), "lin-1", "Nice work!"); err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("success=false must fail, got %v", err)
	}
}

func TestLinearProvider_ClaimTask(t *testing.T) {
	var capturedBody string
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n++
		w.WriteHeader(http.StatusOK)
		switch n {
		case 1:
			w.Write([]byte(`{"data":{"issue":{"id":"lin-1","team":{"states":{"nodes":[{"id":"progress-state","name":"In Progress","type":"started"}]}}}}}`))
		case 2:
			buf := make([]byte, r.ContentLength)
			r.Body.Read(buf)
			capturedBody = string(buf)
			w.Write([]byte(`{"data": {"issueUpdate": {"success": true}}}`))
		default:
			w.Write([]byte(`{"data":{"issue":{"id":"lin-1","identifier":"LIN-1","title":"t","description":"","priority":2,"state":{"name":"In Progress"},"project":{"id":"p1"},"labels":{"nodes":[]}}}}`))
		}
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

func TestLinearWorkflowStateCanonical_OnlyCompletedIsDone(t *testing.T) {
	if got := linearWorkflowStateCanonical("completed"); got != StatusDone {
		t.Fatalf("completed=%q, want done", got)
	}
	if got := linearWorkflowStateCanonical("canceled"); got == StatusDone {
		t.Fatalf("canceled must never map to done")
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
