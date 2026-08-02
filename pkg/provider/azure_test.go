package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAzureDevOpsProvider_GetTaskAndListTasks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/myorg/myproj/_apis/wit/workitems/42":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"id": 42,
				"fields": {
					"System.Title": "Deploy K8s Cluster",
					"System.Description": "Provision production cluster",
					"System.State": "Active",
					"System.WorkItemType": "Task",
					"Microsoft.VSTS.Common.Priority": 1,
					"System.CreatedDate": "2026-08-01T12:00:00Z"
				}
			}`))
		case "/myorg/myproj/_apis/wit/wiql":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"workItems": [
					{"id": 42}
				]
			}`))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/myorg", "myproj", "pat123")
	task, err := ap.GetTask(context.Background(), "42")
	if err != nil || task == nil {
		t.Fatalf("expected clean GetTask, got err: %v", err)
	}

	if task.Ref != "AZ-42" || task.Priority != PriorityUrgent {
		t.Errorf("unexpected task fields: %+v", task)
	}

	tasks, err := ap.ListTasks(context.Background(), "myproj", "Active")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected 1 task listed, got %d (err: %v)", len(tasks), err)
	}

	if err := ap.ClaimTask(context.Background(), "42", "smith"); err != nil {
		t.Errorf("expected clean ClaimTask, got err: %v", err)
	}

	if err := ap.UpdateStatus(context.Background(), "42", "Closed"); err != nil {
		t.Errorf("expected clean UpdateStatus, got err: %v", err)
	}
}
