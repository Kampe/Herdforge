package deps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestKaneoDescriptionWriterHTTPAckIgnoresCLIEnrichment(t *testing.T) {
	var puts atomic.Int32
	var enrichment atomic.Int32
	var cli atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/task/description/target-id":
			puts.Add(1)
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Description string `json:"description"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.Description != "after" {
				http.Error(w, `{"error":"payload"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"target-id","ref":"FAC-768","description":"after","projectId":"p"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/task/target-id":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"target-id","ref":"FAC-768","description":"after","projectId":"p"}`))
		default:
			enrichment.Add(1)
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	k := provider.NewKaneoProvider(server.URL, "p", true)
	writer := KaneoDescriptionWriter{
		ProjectID: "p",
		Provider:  k,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cli.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := writer.SetDescription(ctx, "target-id", "after"); err != nil {
		t.Fatalf("HTTP description writer waited on CLI enrichment: %v", err)
	}
	got, err := writer.GetDescription(ctx, "target-id")
	if err != nil || got != "after" {
		t.Fatalf("HTTP description readback = %q err=%v", got, err)
	}
	if puts.Load() != 1 {
		t.Fatalf("description PUTs = %d, want 1", puts.Load())
	}
	if cli.Load() != 0 {
		t.Fatalf("CLI runner consumed mutation ack: calls=%d", cli.Load())
	}
	if enrichment.Load() != 0 {
		t.Fatalf("presentation enrichment consumed mutation ack: calls=%d", enrichment.Load())
	}
}

func TestKaneoDescriptionWriterReadbackRejectsWrongIdentity(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong task", body: `{"id":"other","ref":"FAC-1","description":"after","projectId":"p"}`},
		{name: "wrong project", body: `{"id":"target-id","ref":"FAC-768","description":"after","projectId":"other"}`},
		{name: "error body", body: `{"error":"not the card"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			writer := KaneoDescriptionWriter{
				ProjectID: "p",
				Provider:  provider.NewKaneoProvider(server.URL, "p", true),
			}
			got, err := writer.GetDescription(context.Background(), "target-id")
			if err == nil {
				t.Fatalf("accepted invalid readback %q", got)
			}
			if got != "" {
				t.Fatalf("refused readback still returned %q", got)
			}
		})
	}
}

func TestDescriptionWriterForUsesKaneoHTTPProvider(t *testing.T) {
	k := provider.NewKaneoProvider("https://kanban-api.example", "p", true)
	bound := provider.NewBoundClient(k, provider.DefaultDeadlines())
	writer, err := DescriptionWriterFor(bound, "p")
	if err != nil {
		t.Fatalf("DescriptionWriterFor: %v", err)
	}
	kw, ok := writer.(KaneoDescriptionWriter)
	if !ok {
		t.Fatalf("writer type %T, want KaneoDescriptionWriter", writer)
	}
	if kw.Provider != k {
		t.Fatal("description writer did not keep the Kaneo HTTP provider")
	}
}

func TestKaneoCLIGetDescriptionRejectsErrorBodyAndWrongIdentity(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{name: "error body", out: `{"error":"task get failed"}`},
		{name: "wrong task", out: `{"id":"other","ref":"FAC-1","description":"after","projectId":"p"}`},
		{name: "wrong project", out: `{"id":"target-id","ref":"FAC-768","description":"after","projectId":"other"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := KaneoDescriptionWriter{
				ProjectID: "p",
				Run: func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tt.out), nil
				},
			}
			got, err := writer.GetDescription(context.Background(), "target-id")
			if err == nil {
				t.Fatalf("accepted invalid CLI readback %q", got)
			}
			if got != "" {
				t.Fatalf("refused CLI readback still returned %q", got)
			}
		})
	}
}
