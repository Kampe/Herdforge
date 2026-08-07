package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestDtoToTask_PositionAndUpdatedAt proves Kaneo DTO fields populate Task
// (FAC-147 review #3/#6 root cause: missing DTO fields always zeroed PUT).
func TestDtoToTask_PositionAndUpdatedAt(t *testing.T) {
	pos := 12.5
	dto := kaneoTaskDTO{
		ID: "id1", Ref: "FAC-1", Title: "t", Status: "todo",
		Priority: "high", ProjectId: "p",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Position:  &pos,
	}
	got := dtoToTask(dto)
	if !got.HasPosition || got.Position != 12.5 {
		t.Fatalf("position: HasPosition=%v Position=%v", got.HasPosition, got.Position)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be populated from DTO")
	}
	// Absent position must not claim HasPosition.
	got2 := dtoToTask(kaneoTaskDTO{ID: "id2", Status: "todo"})
	if got2.HasPosition {
		t.Fatal("absent position must leave HasPosition false")
	}
}

// TestMutateStatusFullSchemaPUT_OmitsUnknownPosition proves a zero Position
// without HasPosition is not serialized as "position":0 (rank clobber).
func TestMutateStatusFullSchemaPUT_OmitsUnknownPosition(t *testing.T) {
	var sawPosition atomic.Bool
	var sawZero atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "t1", "ref": "FAC-1", "title": "hello", "status": "todo",
				"priority": "medium", "projectId": "p",
				// deliberately no position field
				"createdAt": time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		if r.Method == http.MethodPut {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["position"]; ok {
				sawPosition.Store(true)
				if body["position"] == float64(0) {
					sawZero.Store(true)
				}
			}
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	k := NewKaneoProvider(srv.URL, "p", false)
	k.AtomicFenceServer = true
	if err := k.mutateStatusFullSchemaPUT(context.Background(), "t1", "in-progress"); err != nil {
		t.Fatal(err)
	}
	if sawPosition.Load() {
		t.Fatalf("PUT must omit position when provider did not return it (zero-clobber guard); sawZero=%v", sawZero.Load())
	}
}

// TestMutateStatusFullSchemaPUT_PreservesKnownPosition proves rank survives PUT.
func TestMutateStatusFullSchemaPUT_PreservesKnownPosition(t *testing.T) {
	var gotPos atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "t1", "ref": "FAC-1", "title": "hello", "status": "todo",
				"priority": "medium", "projectId": "p", "position": 7.0,
				"createdAt": time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		if r.Method == http.MethodPut {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotPos.Store(body["position"])
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	k := NewKaneoProvider(srv.URL, "p", false)
	k.AtomicFenceServer = true
	if err := k.mutateStatusFullSchemaPUT(context.Background(), "t1", "done"); err != nil {
		t.Fatal(err)
	}
	v := gotPos.Load()
	if v == nil {
		t.Fatal("expected position in PUT body")
	}
	if v.(float64) != 7.0 {
		t.Fatalf("position=%v want 7", v)
	}
}

// TestMemoryUpdateStatus_ResolvesByRef restores FAC-159 ref-keyed callers.
func TestMemoryUpdateStatus_ResolvesByRef(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "uuid-1", Ref: "FAC-159", Status: "todo", Title: "x"})
	if err := mp.UpdateStatus(context.Background(), "FAC-159", StatusInProgress); err != nil {
		t.Fatalf("ref-keyed UpdateStatus: %v", err)
	}
	got, err := mp.GetTask(context.Background(), "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInProgress {
		t.Fatalf("status=%s", got.Status)
	}
}
