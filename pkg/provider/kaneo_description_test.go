package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdateDescriptionAcknowledgesPUTWithoutWaitingForEnrichment(t *testing.T) {
	var puts atomic.Int32
	var enrichment atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/task/description/task-1":
			puts.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read put body: %v", err)
				http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
				return
			}
			var payload struct {
				Description string `json:"description"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.Description != "after-image" {
				t.Errorf("put payload = %s", body)
				http.Error(w, `{"error":"payload"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"task-1","ref":"FAC-768","title":"t","description":"after-image","status":"to-do","priority":"high","projectId":"proj-1"}`))
		case strings.Contains(r.URL.Path, "/project") || r.URL.Query().Get("full") != "" || strings.HasSuffix(r.URL.Path, "/full"):
			enrichment.Add(1)
			select {
			case <-started:
			default:
				close(started)
			}
			<-r.Context().Done()
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":"unexpected"}`, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	k := NewKaneoProvider(server.URL, "proj-1", true)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := k.UpdateDescription(ctx, "task-1", "after-image"); err != nil {
		t.Fatalf("scoped description PUT should acknowledge without enrichment: %v", err)
	}
	if puts.Load() != 1 {
		t.Fatalf("description PUTs = %d, want 1", puts.Load())
	}
	if enrichment.Load() != 0 {
		t.Fatalf("presentation enrichment consumed mutation ack: calls=%d", enrichment.Load())
	}
}

func TestUpdateDescriptionRejectsWrongTaskProjectErrorBodyAndMismatch(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong task", body: `{"id":"other","ref":"FAC-1","description":"after-image","projectId":"proj-1"}`},
		{name: "wrong project", body: `{"id":"task-1","ref":"FAC-768","description":"after-image","projectId":"other-project"}`},
		{name: "error body", body: `{"error":"description write failed"}`},
		{name: "description mismatch", body: `{"id":"task-1","ref":"FAC-768","description":"not-the-after-image","projectId":"proj-1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/api/task/description/task-1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			k := NewKaneoProvider(server.URL, "proj-1", false)
			err := k.UpdateDescription(context.Background(), "task-1", "after-image")
			if err == nil {
				t.Fatal("accepted invalid description acknowledgement")
			}
		})
	}
}

func TestReadDescriptionRejectsWrongTaskProjectAndErrorBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong task", body: `{"id":"other","ref":"FAC-1","description":"secret","projectId":"proj-1"}`},
		{name: "wrong project", body: `{"id":"task-1","ref":"FAC-768","description":"secret","projectId":"other-project"}`},
		{name: "error body", body: `{"error":"task read failed"}`},
		{name: "missing project", body: `{"id":"task-1","ref":"FAC-768","description":"secret"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/task/task-1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			k := NewKaneoProvider(server.URL, "proj-1", true)
			got, err := k.ReadDescription(context.Background(), "task-1")
			if err == nil {
				t.Fatalf("accepted invalid description readback %q", got)
			}
			if got != "" {
				t.Fatalf("refused readback still returned %q", got)
			}
		})
	}
}

func TestReadDescriptionReturnsExactDescriptionWithoutEnrichment(t *testing.T) {
	var enrichment atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/task/task-1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"task-1","ref":"FAC-768","title":"t","description":"full after-image","status":"to-do","priority":"high","projectId":"proj-1"}`))
		case strings.Contains(r.URL.Path, "/project") || r.URL.Query().Get("full") != "":
			enrichment.Add(1)
			<-r.Context().Done()
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":"unexpected"}`, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	k := NewKaneoProvider(server.URL, "proj-1", true)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	got, err := k.ReadDescription(ctx, "task-1")
	if err != nil {
		t.Fatalf("exact description readback: %v", err)
	}
	if got != "full after-image" {
		t.Fatalf("description = %q", got)
	}
	if enrichment.Load() != 0 {
		t.Fatalf("readback waited on enrichment: calls=%d", enrichment.Load())
	}
}
