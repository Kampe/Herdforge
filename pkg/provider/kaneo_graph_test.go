package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestKaneoListProjectRelations_NoCredentials_RefusesCLIFanout is the
// production-path proof for audit vr8a7lvxx21e6shmb1z02atj: with use_cli true
// and no HTTP key, ListProjectRelations must fail closed BEFORE any per-task
// relation CLI subprocess (never silent 166-way CLI fan-out).
func TestKaneoListProjectRelations_NoCredentials_RefusesCLIFanout(t *testing.T) {
	t.Setenv("KANEO_API_KEY", "")
	// Isolate from installed UserConfigDir profile (not HOME/XDG Join).
	withUserConfigDir(t, t.TempDir())

	var cliCalls atomic.Int64
	old := kaneoRunCLI
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		cliCalls.Add(1)
		// If ListTasks were reached, return 166 fake tasks — fan-out must not run.
		if len(args) >= 2 && args[0] == "task" && args[1] == "list" {
			var tasks []map[string]string
			for i := 1; i <= 166; i++ {
				tasks = append(tasks, map[string]string{
					"id": fmt.Sprintf("id-%d", i), "ref": fmt.Sprintf("FAC-%d", i),
					"status": "to-do", "title": "t", "projectId": "proj",
				})
			}
			b, _ := json.Marshal(tasks)
			return &CLIResult{Stdout: b}, nil
		}
		if len(args) >= 3 && args[0] == "task" && args[1] == "rel" {
			t.Errorf("relation CLI must not run on credential fail-closed path: %v", args)
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		return &CLIResult{Stdout: []byte(`[]`)}, nil
	}
	t.Cleanup(func() { kaneoRunCLI = old })

	k := NewKaneoProvider("https://kanban-api.example.test", "proj", true)
	k.APIKey = ""
	// Ensure resolved key is empty (no profile).
	if k.resolvedAPIKey() != "" {
		t.Fatalf("test isolation failed: resolved key non-empty")
	}

	start := time.Now()
	_, err := k.ListProjectRelations(context.Background(), "proj")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected credential fail-closed error")
	}
	if !errors.Is(err, ErrGraphCredentialsRequired) && !strings.Contains(err.Error(), "refusing silent CLI") {
		t.Fatalf("want ErrGraphCredentialsRequired, got %v", err)
	}
	// Must not ListTasks (would start fan-out prep) nor any rel list CLI.
	if cliCalls.Load() != 0 {
		t.Fatalf("want 0 CLI calls before/during credential refuse, got %d", cliCalls.Load())
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("fail-closed must be immediate, took %v", elapsed)
	}
}

func TestKaneoListProjectRelations_ExplicitCLIMode(t *testing.T) {
	t.Setenv(EnvKaneoRelationsCLI, "1")
	t.Setenv("KANEO_API_KEY", "")
	withUserConfigDir(t, t.TempDir())
	var relCalls, listCalls atomic.Int64
	old := kaneoRunCLI
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		if len(args) >= 2 && args[0] == "task" && args[1] == "list" {
			listCalls.Add(1)
			// Key off the requested page, not call order: pages are fetched
			// concurrently so arrival order is not deterministic.
			page := "1"
			for i, a := range args {
				if a == "--page" && i+1 < len(args) {
					page = args[i+1]
				}
			}
			status := ""
			for i, a := range args {
				if a == "--status" && i+1 < len(args) {
					status = args[i+1]
				}
			}
			if page != "1" || status != StatusToDo {
				return &CLIResult{Stdout: []byte(`[]`)}, nil
			}
			return &CLIResult{Stdout: []byte(`[{"id":"task-1","ref":"CHA-1","status":"to-do","title":"t","projectId":"project"}]`)}, nil
		}
		if len(args) >= 3 && args[0] == "task" && args[1] == "rel" {
			relCalls.Add(1)
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		return &CLIResult{Stdout: []byte(`[]`)}, nil
	}
	t.Cleanup(func() { kaneoRunCLI = old })
	k := NewKaneoProvider("https://kanban.example", "project", true)
	if !k.UseCLI || os.Getenv(EnvKaneoRelationsCLI) != "1" {
		t.Fatal("explicit CLI relation mode was not enabled")
	}
	if _, err := k.ListProjectRelations(context.Background(), "project"); err != nil {
		t.Fatalf("explicit CLI relation mode: %v", err)
	}
	if relCalls.Load() != 1 {
		t.Fatalf("want one CLI relation read, got %d", relCalls.Load())
	}
}

// TestKaneoListProjectRelations_WithKey_HTTPFanoutNotCLI proves credentialed
// path uses HTTP /api/task-relation/{id} and never kaneo rel list CLI, and
// stays under DefaultListDeadline for 166 tasks with fast HTTP.
func TestKaneoListProjectRelations_WithKey_HTTPFanoutNotCLI(t *testing.T) {
	const n = 166
	var httpRels atomic.Int64
	var cliRel atomic.Int64
	var cliList atomic.Int64
	// Status columns are walked concurrently, so the stub records pages under a
	// mutex and the assertion below checks the per-page multiset rather than a
	// serial sequence: cross-column interleaving is expected, not a defect.
	var cliMu sync.Mutex
	cliPageCount := map[string]int{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/api/task-relation/") {
			httpRels.Add(1)
			// one edge involving first and last task when listing either end
			id := strings.TrimPrefix(r.URL.Path, "/api/task-relation/")
			if id == "id-1" || id == fmt.Sprintf("id-%d", n) {
				b, _ := json.Marshal([]kaneoRelationDTO{{
					ID: "rel-1", SourceTaskID: "id-1", TargetTaskID: fmt.Sprintf("id-%d", n),
					RelationType: "blocks",
				}})
				w.WriteHeader(http.StatusOK)
				w.Write(b)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`404`))
	}))
	t.Cleanup(server.Close)

	old := kaneoRunCLI
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		if len(args) >= 2 && args[0] == "task" && args[1] == "list" {
			cliList.Add(1)
			page := "1"
			for i, a := range args {
				if a == "--page" && i+1 < len(args) {
					page = args[i+1]
				}
			}
			cliMu.Lock()
			cliPageCount[page]++
			cliMu.Unlock()
			start, end := 0, 0
			switch page {
			case "1":
				start, end = 1, 100
			case "2":
				start, end = 101, n
			default:
				// Page 3 terminates the column; batched fetching may also
				// request page 4+ speculatively. Both return empty.
				return &CLIResult{Stdout: []byte(`[]`)}, nil
			}
			var tasks []map[string]string
			for i := start; i <= end; i++ {
				tasks = append(tasks, map[string]string{
					"id": fmt.Sprintf("id-%d", i), "ref": fmt.Sprintf("FAC-%d", i),
					"status": "to-do", "title": "t", "projectId": "proj",
				})
			}
			b, _ := json.Marshal(tasks)
			return &CLIResult{Stdout: b}, nil
		}
		if len(args) >= 3 && args[1] == "rel" {
			cliRel.Add(1)
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		return &CLIResult{Stdout: []byte(`[]`)}, nil
	}
	t.Cleanup(func() { kaneoRunCLI = old })

	// Isolate profile resolver; credentials come only from explicit APIKey.
	withUserConfigDir(t, t.TempDir())
	const key = "test-key-not-for-prod"
	t.Setenv("KANEO_API_KEY", key)
	t.Setenv("KANEO_API_URL", server.URL)

	k := NewKaneoProvider(server.URL, "proj", true) // use_cli true — production shape
	k.BulkConcurrency = 16
	k.Deadlines = Deadlines{List: DefaultListDeadline}

	start := time.Now()
	rels, err := k.ListProjectRelations(context.Background(), "proj")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].ID != "rel-1" {
		t.Fatalf("want dual-end assembled edge, got %+v", rels)
	}
	if cliRel.Load() != 0 {
		t.Fatalf("must not use CLI rel list when HTTP credentials present, got %d", cliRel.Load())
	}
	cliMu.Lock()
	gotPages := fmt.Sprintf("1=%d 2=%d 3=%d", cliPageCount["1"], cliPageCount["2"], cliPageCount["3"])
	cliMu.Unlock()
	// Six status columns, each paginating 100 then 66 then an empty page.
	// Pages are fetched in concurrent batches, so each column requests its
	// whole first batch; what matters is that every column walked pages 1-3
	// and terminated on the empty one.
	if gotPages != "1=6 2=6 3=6" {
		t.Fatalf("want every column to walk 100+66+empty; calls=%d pages=%s", cliList.Load(), gotPages)
	}
	// One HTTP GET per task id (fan-out), not sequential CLI.
	if httpRels.Load() != int64(n) {
		t.Fatalf("want %d HTTP relation GETs, got %d", n, httpRels.Load())
	}
	if elapsed > DefaultListDeadline {
		t.Fatalf("HTTP fan-out exceeded list deadline: %v > %v", elapsed, DefaultListDeadline)
	}
	// Fast httptest should finish well under 5s for 166 concurrent.
	if elapsed > 5*time.Second {
		t.Fatalf("HTTP fan-out unexpectedly slow: %v", elapsed)
	}
}

// TestKaneoListRelations_UseCLITrustedOriginUsesHTTP proves the exact closure
// read surface avoids relation CLI subprocesses when use_cli=true and an
// independently trusted HTTP origin is available.
func TestKaneoListRelations_UseCLITrustedOriginUsesHTTP(t *testing.T) {
	var httpCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer closure-key" {
			t.Errorf("missing origin-bound bearer auth: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)

	var cliCalls atomic.Int64
	old := kaneoRunCLI
	kaneoRunCLI = func(context.Context, string, ...string) (*CLIResult, error) {
		cliCalls.Add(1)
		return &CLIResult{Stdout: []byte(`[]`)}, nil
	}
	t.Cleanup(func() { kaneoRunCLI = old })

	withUserConfigDir(t, t.TempDir())
	t.Setenv("KANEO_API_KEY", "closure-key")
	t.Setenv("KANEO_API_URL", server.URL)
	k := NewKaneoProvider(server.URL, "proj", true)
	if _, err := k.ListRelations(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
	if httpCalls.Load() != 1 {
		t.Fatalf("want one HTTP closure read, got %d", httpCalls.Load())
	}
	if cliCalls.Load() != 0 {
		t.Fatalf("want zero relation CLI calls under use_cli=true, got %d", cliCalls.Load())
	}
}

// TestKaneoListProjectRelations_DeadlineCancel proves the internal production
// deadline returns a timeout, bounds wall time, and cancels every live handler.
func TestKaneoListProjectRelations_DeadlineCancel(t *testing.T) {
	const fallback = 300 * time.Millisecond
	var started atomic.Int64
	var canceled atomic.Int64
	var completed atomic.Int64
	canceledCh := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		select {
		case <-r.Context().Done():
			canceled.Add(1)
			canceledCh <- struct{}{}
			return
		case <-time.After(fallback):
			completed.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(server.Close)

	old := kaneoRunCLI
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		page := "1"
		for i, a := range args {
			if a == "--page" && i+1 < len(args) {
				page = args[i+1]
			}
		}
		// Pages are fetched in concurrent batches, so pages past the empty
		// terminator may be requested speculatively and discarded.
		if page != "1" {
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		var tasks []map[string]string
		for i := 1; i <= 8; i++ {
			tasks = append(tasks, map[string]string{
				"id": fmt.Sprintf("id-%d", i), "ref": fmt.Sprintf("FAC-%d", i),
				"status": "to-do", "title": "t", "projectId": "proj",
			})
		}
		b, _ := json.Marshal(tasks)
		return &CLIResult{Stdout: b}, nil
	}
	t.Cleanup(func() { kaneoRunCLI = old })

	withUserConfigDir(t, t.TempDir())
	t.Setenv("KANEO_API_KEY", "cancel-key")
	t.Setenv("KANEO_API_URL", server.URL)
	k := NewKaneoProvider(server.URL, "proj", true)
	k.BulkConcurrency = 4
	k.Deadlines = Deadlines{List: 60 * time.Millisecond}

	start := time.Now()
	_, err := k.ListProjectRelations(context.Background(), "proj")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("deadline path must return a timeout error, never success")
	}
	if !IsTimeout(err) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want timeout classification, got %T %v", err, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("deadline did not bound fan-out: %v err=%v", elapsed, err)
	}
	wantCanceled := started.Load()
	if wantCanceled == 0 || wantCanceled > int64(k.BulkConcurrency) {
		t.Fatalf("want 1..%d started handlers, got %d", k.BulkConcurrency, wantCanceled)
	}
	for i := int64(0); i < wantCanceled; i++ {
		select {
		case <-canceledCh:
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("handler cancellation missing: started=%d canceled=%d", wantCanceled, canceled.Load())
		}
	}
	if canceled.Load() != wantCanceled {
		t.Fatalf("every started handler must be canceled: started=%d canceled=%d", wantCanceled, canceled.Load())
	}
	if completed.Load() != 0 {
		t.Fatalf("no handler may complete normally after deadline, got %d", completed.Load())
	}
}

// TestKaneoListProjectRelations_LargeBoardUsesBoundedBurst proves the large
// board strategy scales the Kaneo-only fan-out without changing the ordinary
// list deadline or allowing an unbounded number of requests in flight.
func TestKaneoListProjectRelations_LargeBoardUsesBoundedBurst(t *testing.T) {
	const n = 1500
	var httpRels, inFlight, maxInFlight atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/task-relation/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		httpRels.Add(1)
		current := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if current <= old || maxInFlight.CompareAndSwap(old, current) {
				break
			}
		}
		defer inFlight.Add(-1)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimPrefix(r.URL.Path, "/api/task-relation/")
		if id == "id-1" || id == fmt.Sprintf("id-%d", n) {
			_, _ = fmt.Fprintf(w, `[{"id":"rel-1","sourceTaskId":"id-1","targetTaskId":"id-%d","relationType":"blocks"}]`, n)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	old := kaneoRunCLI
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		page := 1
		for i, arg := range args {
			if arg == "--page" && i+1 < len(args) {
				_, _ = fmt.Sscanf(args[i+1], "%d", &page)
			}
		}
		if page == 16 {
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		if page < 1 || page > 15 {
			t.Fatalf("unexpected task-list page %d", page)
		}
		tasks := make([]map[string]string, 0, 100)
		for i := (page-1)*100 + 1; i <= page*100; i++ {
			tasks = append(tasks, map[string]string{
				"id": fmt.Sprintf("id-%d", i), "ref": fmt.Sprintf("FAC-%d", i),
				"status": "to-do", "title": "t", "projectId": "proj",
			})
		}
		body, _ := json.Marshal(tasks)
		return &CLIResult{Stdout: body}, nil
	}
	defer func() { kaneoRunCLI = old }()

	withUserConfigDir(t, t.TempDir())
	t.Setenv("KANEO_API_KEY", "large-board-key")
	t.Setenv("KANEO_API_URL", server.URL)
	k := NewKaneoProvider(server.URL, "proj", true)
	start := time.Now()
	rels, err := k.ListProjectRelations(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].ID != "rel-1" {
		t.Fatalf("want one dual-end relation, got %+v", rels)
	}
	if httpRels.Load() != n {
		t.Fatalf("want %d relation reads, got %d", n, httpRels.Load())
	}
	if maxInFlight.Load() > DefaultKaneoLargeBoardConcurrency {
		t.Fatalf("in-flight relation reads exceeded large-board bound: %d", maxInFlight.Load())
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("large-board snapshot did not finish in bounded time: %v", elapsed)
	}
}
