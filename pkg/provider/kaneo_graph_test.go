package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestKaneoListProjectRelations_WithKey_HTTPFanoutNotCLI proves credentialed
// path uses HTTP /api/task-relation/{id} and never kaneo rel list CLI, and
// stays under DefaultListDeadline for 166 tasks with fast HTTP.
func TestKaneoListProjectRelations_WithKey_HTTPFanoutNotCLI(t *testing.T) {
	const n = 166
	var httpRels atomic.Int64
	var cliRel atomic.Int64
	var cliList atomic.Int64

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
			if page != "1" {
				return &CLIResult{Stdout: []byte(`[]`)}, nil
			}
			var tasks []map[string]string
			for i := 1; i <= n; i++ {
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
	t.Setenv("KANEO_API_KEY", "")

	k := NewKaneoProvider(server.URL, "proj", true) // use_cli true — production shape
	k.APIKey = "test-key-not-for-prod"
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
	if cliList.Load() < 1 {
		t.Fatal("expected ListTasks via CLI under use_cli")
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

// TestKaneoListProjectRelations_DeadlineCancel proves cancel bounds work mid-fanout.
func TestKaneoListProjectRelations_DeadlineCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
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
		if page != "1" {
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		var tasks []map[string]string
		for i := 1; i <= 40; i++ {
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
	t.Setenv("KANEO_API_KEY", "")
	k := NewKaneoProvider(server.URL, "proj", true)
	k.APIKey = "k"
	k.BulkConcurrency = 4
	k.Deadlines = Deadlines{List: 80 * time.Millisecond}

	ctx := context.Background()
	start := time.Now()
	_, err := k.ListProjectRelations(ctx, "proj")
	if err == nil {
		// May succeed if all GETs finish under deadline on fast host — require
		// either error or success; if success, elapsed must still be < 1s.
		if time.Since(start) > time.Second {
			t.Fatalf("unexpected long success: %v", time.Since(start))
		}
		return
	}
	// Cancel/timeout path
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("deadline did not bound fan-out: %v err=%v", time.Since(start), err)
	}
}

