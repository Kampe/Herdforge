package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestSelectNextTask_NoRoleMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"id":"1", "ref":"FAC-1", "title":"No Match", "priority":"high", "status":"to-do", "labels":[{"name":"herd-someone-else"}]}
		]`))
	}))
	defer server.Close()

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	kp := provider.NewKaneoProvider(server.URL, "proj-1")
	engine := NewEngine(cfg, kp, nil, nil, nil)

	task, err := engine.SelectNextTask(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("expected no error for no role match, got: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task for no role match, got %+v", task)
	}
}

func TestRunPulse_SelectsAndClaimsTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/task" && r.Method == "POST" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"id":"t-1", "ref":"FAC-99", "title":"Pulse Task", "priority":"urgent", "status":"to-do", "labels":[{"name":"herd-smith"}]}
		]`))
	}))
	defer server.Close()

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	kp := provider.NewKaneoProvider(server.URL, "proj-1")
	engine := NewEngine(cfg, kp, nil, nil, nil)

	task, err := engine.RunPulse(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("expected clean pulse, got err: %v", err)
	}
	if task == nil {
		t.Fatal("expected non-nil task from pulse")
	}
	if task.Ref != "FAC-99" {
		t.Errorf("expected FAC-99, got %s", task.Ref)
	}
}

func TestRunPulse_NoTasks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	kp := provider.NewKaneoProvider(server.URL, "proj-1")
	engine := NewEngine(cfg, kp, nil, nil, nil)

	task, err := engine.RunPulse(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("expected no error for empty queue, got: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task for empty queue, got %+v", task)
	}
}
