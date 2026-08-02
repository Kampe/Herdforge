package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestGitHubProvider_GetTask_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	_, err := gp.GetTask(context.Background(), "1")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

func TestGitHubProvider_ListTasks_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	_, err := gp.ListTasks(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error on bad JSON ListTasks, got nil")
	}
}

func TestAzureProvider_Priority2(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"id": 2,
			"fields": {
				"System.Title": "High Priority",
				"System.Description": "",
				"System.State": "Active",
				"System.WorkItemType": "Issue",
				"Microsoft.VSTS.Common.Priority": 2,
				"System.CreatedDate": "2026-08-01T12:00:00Z"
			}
		}`))
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/myorg", "myproj", "pat123")
	task, err := ap.GetTask(context.Background(), "2")
	if err != nil {
		t.Fatalf("expected task, got err: %v", err)
	}
	if task.Priority != PriorityHigh {
		t.Errorf("expected PriorityHigh, got %v", task.Priority)
	}
}

func TestAzureProvider_Priority0(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"id": 0,
			"fields": {
				"System.Title": "Zero Priority",
				"System.Description": "",
				"System.State": "New",
				"System.WorkItemType": "Issue",
				"Microsoft.VSTS.Common.Priority": 0,
				"System.CreatedDate": "2026-08-01T12:00:00Z"
			}
		}`))
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/myorg", "myproj", "pat123")
	task, err := ap.GetTask(context.Background(), "0")
	if err != nil {
		t.Fatalf("expected task, got err: %v", err)
	}
	if task.Priority != PriorityMedium {
		t.Errorf("expected PriorityMedium (default for 0), got %v", task.Priority)
	}
}

func TestAzureProvider_AddComment_Non200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/myorg", "myproj", "pat123")
	err := ap.AddComment(context.Background(), "42", "comment")
	if err == nil {
		t.Fatal("expected error on 304, got nil")
	}
}

func TestLinearProvider_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	_, err := lp.GetTask(context.Background(), "lin-1")
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestLinearProvider_UnmarshalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	}))
	defer server.Close()

	lp := NewLinearProvider("mock-api-key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	_, err := lp.GetTask(context.Background(), "lin-1")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

func TestHelpers_VerifyProviderContract_NilProvider(t *testing.T) {
	err := VerifyProviderContract(context.Background(), nil, "proj")
	if err == nil {
		t.Fatal("expected error for nil provider, got nil")
	}
}

func TestHelpers_VerifyProviderContract_ListTasksError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	// ListTasks always succeeds for MemoryProvider, so use GitHub with a broken server
	u, _ := url.Parse(ts.URL)
	gp := NewGitHubProvider("", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	err := VerifyProviderContract(context.Background(), gp, "")
	if err == nil {
		t.Fatal("expected error from ListTasks, got nil")
	}
}

func TestAzureProvider_ListTasks_Non200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/myorg", "myproj", "pat123")
	_, err := ap.ListTasks(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error on 500 ListTasks, got nil")
	}
}

func TestAzureProvider_ListTasks_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{invalid}`))
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/myorg", "myproj", "pat123")
	_, err := ap.ListTasks(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error on bad WIQL JSON, got nil")
	}
}

func TestAzureProvider_GetTask_UnmarshalError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/myorg", "myproj", "pat123")
	_, err := ap.GetTask(context.Background(), "1")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}
