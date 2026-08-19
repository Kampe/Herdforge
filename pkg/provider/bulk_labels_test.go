package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestKaneoProvider_ListTaskLabelsBulkReportsCompleteInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/task" || r.URL.Query().Get("projectId") != "project-1" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
            {"id":"task-1","ref":"FAC-1","status":"to-do","labels":[{"name":"lane:alpha"}]},
            {"id":"task-2","ref":"FAC-2","status":"to-do","labels":[{"name":"lane:beta"},{"name":"risk:R1"}]}
        ]`))
	}))
	defer server.Close()

	got, err := NewKaneoProvider(server.URL, "project-1", false).ListTaskLabelsBulk(context.Background(), []string{"task-1", "FAC-2"})
	if err != nil {
		t.Fatalf("ListTaskLabelsBulk: %v", err)
	}
	if !got.Complete || got.Truncated || got.Requested != 2 || got.Retrieved != 2 {
		t.Fatalf("inventory completeness = %+v, want complete 2/2", got)
	}
	want := map[string][]TaskLabel{
		"task-1": {{Name: "lane:alpha", TaskID: "task-1"}},
		"FAC-2":  {{Name: "lane:beta", TaskID: "task-2"}, {Name: "risk:R1", TaskID: "task-2"}},
	}
	if !reflect.DeepEqual(got.Labels, want) {
		t.Fatalf("labels = %#v, want %#v", got.Labels, want)
	}
}

func TestKaneoProvider_ListTaskLabelsBulkMarksMissingTasksTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"task-1","ref":"FAC-1","status":"to-do","labels":[]}]`))
	}))
	defer server.Close()

	got, err := NewKaneoProvider(server.URL, "project-1", false).ListTaskLabelsBulk(context.Background(), []string{"task-1", "missing"})
	if err != nil {
		t.Fatalf("ListTaskLabelsBulk: %v", err)
	}
	if got.Complete || !got.Truncated || got.Requested != 2 || got.Retrieved != 1 {
		t.Fatalf("inventory completeness = %+v, want truncated 1/2", got)
	}
}
