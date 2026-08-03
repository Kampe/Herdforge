package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/store"
)

func dependencyFence(ref, id string, edges ...deps.DependencyEdge) string {
	b, _ := json.Marshal(struct {
		Version int                   `json:"version"`
		TaskRef deps.Ref              `json:"task_ref"`
		TaskID  deps.TaskID           `json:"task_id"`
		Edges   []deps.DependencyEdge `json:"edges"`
	}{1, deps.Ref(ref), deps.TaskID(id), edges})
	return "```herd-deps-v1\n" + string(b) + "\n```\n"
}

func TestSelectNextTask_ProductionPathSurfacesMixedBlockedResults(t *testing.T) {
	const project = "proj-159"
	mp := provider.NewMemoryProvider()
	graph := deps.NewMemoryStore()
	local, err := store.New(filepath.Join(t.TempDir(), "blocked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	add := func(ref, id, description string) {
		task := &provider.Task{ID: id, Ref: ref, Title: ref, Priority: provider.PriorityHigh, Status: provider.StatusToDo, ProjectID: project, Labels: []string{"herd-smith"}, Description: description}
		mp.AddTask(task)
		graph.AddTask(task)
	}
	add("FAC-1", "id-1", dependencyFence("FAC-1", "id-1"))                                                                                     // eligible
	add("FAC-2", "id-2", "missing structured provenance")                                                                                      // missing provenance
	add("FAC-3", "id-3", dependencyFence("FAC-3", "id-3"))                                                                                     // drift
	add("FAC-4", "id-4", dependencyFence("FAC-4", "id-4", deps.DependencyEdge{SourceRef: "FAC-5", TargetRef: "FAC-4", Type: deps.EdgeBlocks})) // cycle/drift
	add("FAC-5", "id-5", dependencyFence("FAC-5", "id-5", deps.DependencyEdge{SourceRef: "FAC-4", TargetRef: "FAC-5", Type: deps.EdgeBlocks}))
	graph.EnsureTask("FAC-9", provider.StatusToDo, provider.PriorityLow)
	if _, err := graph.SeedBlocks("FAC-9", "FAC-3"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.SeedBlocks("FAC-4", "FAC-5"); err != nil {
		t.Fatal(err)
	}

	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: project}}, mp, nil, local, nil, nil)
	e.Deps = graph
	got, err := e.SelectNextTask(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("mixed board should preserve eligible work: %v", err)
	}
	if got == nil || got.Ref != "FAC-1" {
		t.Fatalf("eligible task was discarded: %+v", got)
	}
	history, err := local.BlockedSelectionHistory(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 {
		t.Fatalf("want every blocked card recorded, got %d: %+v", len(history), history)
	}
	seen := map[string]store.BlockedRecord{}
	codes := map[string]int{}
	for _, record := range history {
		seen[record.Ref] = record
		codes[record.Code]++
		if record.TaskID == "" || record.Code == "" || record.Reason == "" {
			t.Fatalf("blocked evidence must have stable identity and reason: %+v", record)
		}
	}
	for _, ref := range []string{"FAC-2", "FAC-3", "FAC-4", "FAC-5"} {
		if _, ok := seen[ref]; !ok {
			t.Fatalf("missing durable BLOCKED evidence for %s: %+v", ref, history)
		}
	}
	if codes["missing_provenance"] != 1 || codes["drift"] != 1 || codes["cyclic"] != 1 || codes["open_blocker"] != 1 {
		t.Fatalf("want missing-provenance, drift, and cycle blocks, got codes=%v history=%+v", codes, history)
	}
}

func TestSelectNextTask_AllBlockedIsObservable(t *testing.T) {
	const project = "proj-159"
	mp := provider.NewMemoryProvider()
	graph := deps.NewMemoryStore()
	task := &provider.Task{ID: "id-3", Ref: "FAC-3", Title: "drift", Priority: provider.PriorityHigh, Status: provider.StatusToDo, ProjectID: project, Labels: []string{"herd-smith"}, Description: dependencyFence("FAC-3", "id-3")}
	mp.AddTask(task)
	graph.AddTask(task)
	graph.EnsureTask("FAC-9", provider.StatusToDo, provider.PriorityLow)
	if _, err := graph.SeedBlocks("FAC-9", "FAC-3"); err != nil {
		t.Fatal(err)
	}
	local, err := store.New(filepath.Join(t.TempDir(), "blocked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: project}}, mp, nil, local, nil, nil)
	e.Deps = graph
	_, err = e.SelectNextTask(context.Background(), "herd-smith")
	if err == nil || !strings.Contains(err.Error(), "all candidates blocked") || !strings.Contains(err.Error(), "FAC-3") || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("all-blocked production path must expose identity/reason, got %v", err)
	}
	history, err := local.BlockedSelectionHistory(10)
	if err != nil || len(history) != 1 {
		t.Fatalf("all-blocked evidence missing: history=%+v err=%v", history, err)
	}
}

func TestSelectNextTask_BlockedFailsClosedWithoutEvidenceStore(t *testing.T) {
	const project = "proj-159"
	mp := provider.NewMemoryProvider()
	graph := deps.NewMemoryStore()
	task := &provider.Task{ID: "id-3", Ref: "FAC-3", Title: "drift", Priority: provider.PriorityHigh, Status: provider.StatusToDo, ProjectID: project, Labels: []string{"herd-smith"}, Description: dependencyFence("FAC-3", "id-3")}
	mp.AddTask(task)
	graph.AddTask(task)
	graph.EnsureTask("FAC-9", provider.StatusToDo, provider.PriorityLow)
	if _, err := graph.SeedBlocks("FAC-9", "FAC-3"); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: project}}, mp, nil, nil, nil, nil)
	e.Deps = graph
	_, err := e.SelectNextTask(context.Background(), "herd-smith")
	if err == nil || !strings.Contains(err.Error(), "durable dependency BLOCKED evidence unavailable") {
		t.Fatalf("nil evidence authority must fail closed, got %v", err)
	}
}

func TestSelectNextTask_NoRoleMatch(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "1", Ref: "FAC-1", Title: "No Match", Priority: provider.PriorityHigh,
		Status: "to-do", ProjectID: "proj-1", Labels: []string{"herd-someone-else"},
	})
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	engine := NewEngine(cfg, mp, nil, nil, nil, nil)

	task, err := engine.SelectNextTask(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("expected no error for no role match, got: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task for no role match, got %+v", task)
	}
}

func TestRunPulse_SelectsAndClaimsTask(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-99", Title: "Pulse Task", Priority: provider.PriorityUrgent,
		Status: "to-do", ProjectID: "proj-1", Labels: []string{"herd-smith"},
		Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-99\",\"task_id\":\"t-1\",\"edges\":[]}\n```\n",
	})
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	engine := NewEngine(cfg, mp, nil, nil, nil, nil)
	own, oerr := deps.OpenLeaseOwnership(filepath.Join(t.TempDir(), "lease.db"), "herd", "memory", "proj-1")
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer own.Close()
	engine.Ownership = own

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
	got, _ := mp.GetTask(context.Background(), "t-1")
	if got.Status != provider.StatusInProgress {
		t.Fatalf("claim should flip status, got %s", got.Status)
	}
}

func TestSelectNextTask_SamePrioritySortsByRef(t *testing.T) {
	mp := provider.NewMemoryProvider()
	fence := func(ref, id string) string {
		return "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"" + ref + "\",\"task_id\":\"" + id + "\",\"edges\":[]}\n```\n"
	}
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-10", Title: "Same Priority 10", Priority: provider.PriorityHigh, Status: "to-do", ProjectID: "proj-1", Labels: []string{"herd-smith"}, Description: fence("FAC-10", "1")})
	mp.AddTask(&provider.Task{ID: "2", Ref: "FAC-2", Title: "Same Priority 2", Priority: provider.PriorityHigh, Status: "to-do", ProjectID: "proj-1", Labels: []string{"herd-smith"}, Description: fence("FAC-2", "2")})
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	engine := NewEngine(cfg, mp, nil, nil, nil, nil)

	task, err := engine.SelectNextTask(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("expected task selection, got err: %v", err)
	}
	if task.Ref != "FAC-2" {
		t.Errorf("expected FAC-2 (numerically lower ticket) for same priority, got %s", task.Ref)
	}
}

func TestRunPulse_NoTasks(t *testing.T) {
	mp := provider.NewMemoryProvider()
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	engine := NewEngine(cfg, mp, nil, nil, nil, nil)

	task, err := engine.RunPulse(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("expected no error for empty queue, got: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task for empty queue, got %+v", task)
	}
}

func TestRunPulse_ClaimError(t *testing.T) {
	// Kaneo HTTP: list/get/relations OK; patch fails (claim path).
	// Task carries empty versioned provenance so the FAC-159 gate reaches claim.
	type kaneoTask struct {
		ID       string `json:"id"`
		Ref      string `json:"ref"`
		Title    string `json:"title"`
		Priority string `json:"priority"`
		Status   string `json:"status"`
		Labels   []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Description string `json:"description"`
	}
	task := kaneoTask{
		ID: "t-1", Ref: "FAC-99", Title: "Fail Claim", Priority: "urgent", Status: "to-do",
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "herd-smith"}},
		Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-99\",\"task_id\":\"t-1\",\"edges\":[]}\n```\n",
	}
	taskJSON, _ := json.Marshal(task)
	listJSON, _ := json.Marshal([]kaneoTask{task})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.Contains(r.URL.Path, "relation") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`))
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/task/") && !strings.Contains(r.URL.RawQuery, "projectId") {
			w.WriteHeader(http.StatusOK)
			w.Write(taskJSON)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(listJSON)
	}))
	defer server.Close()

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	kp := provider.NewKaneoProvider(server.URL, "proj-1", false)
	engine := NewEngine(cfg, kp, nil, nil, nil, nil)
	own, oerr := deps.OpenLeaseOwnership(filepath.Join(t.TempDir(), "lease.db"), "herd", "kaneo", "proj-1")
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer own.Close()
	engine.Ownership = own

	_, err := engine.RunPulse(context.Background(), "herd-smith")
	if err == nil {
		t.Fatal("expected error from claim failure")
	}
}

func TestSelectNextTask_ListError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	kp := provider.NewKaneoProvider(server.URL, "proj-1", false)
	engine := NewEngine(cfg, kp, nil, nil, nil, nil)

	_, err := engine.SelectNextTask(context.Background(), "herd-smith")
	if err == nil {
		t.Fatal("expected error from list failure")
	}
}

func TestRunPulse_SelectNextTaskError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	kp := provider.NewKaneoProvider(server.URL, "proj-1", false)
	engine := NewEngine(cfg, kp, nil, nil, nil, nil)

	_, err := engine.RunPulse(context.Background(), "herd-smith")
	if err == nil {
		t.Fatal("expected error from pulse with failed list")
	}
}
