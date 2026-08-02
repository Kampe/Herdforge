package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestEngine_SelectNextTask_DeterministicSort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"id":"1", "ref":"FAC-10", "title":"Medium 1", "priority":"medium", "status":"to-do", "labels":[{"name":"herd-smith"}]},
			{"id":"2", "ref":"FAC-2", "title":"Urgent 1", "priority":"urgent", "status":"to-do", "labels":[{"name":"herd-smith"}]},
			{"id":"3", "ref":"FAC-1", "title":"High 1", "priority":"high", "status":"to-do", "labels":[{"name":"herd-smith"}]}
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
		t.Fatalf("expected task selection, got err: %v", err)
	}

	if task == nil {
		t.Fatalf("expected non-nil task selected")
	}

	if task.Ref != "FAC-2" {
		t.Errorf("expected urgent task FAC-2 selected first, got %s", task.Ref)
	}
}
