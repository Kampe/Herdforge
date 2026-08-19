package webhook

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestTaskCreatedHandler_CreatesOneGroomingCardAndIgnoresReplay(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	board := provider.NewMemoryProvider()
	r, err := NewReceiver(testSecret, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.RegisterHandler(NewTaskCreatedHandler(board, "project-1"))
	body := []byte(`{"provider":"auditor","type":"task.created","task_ref":"EXT-9","project_id":"project-1","payload":{"title":"Audit finding","body":"Untrusted report"}}`)
	for _, deliveryID := range []string{"delivery-1", "delivery-1"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, newValidRequest(testSecret, deliveryID, body))
		if w.Code != 200 {
			t.Fatalf("delivery %s: status=%d body=%s", deliveryID, w.Code, w.Body.String())
		}
	}
	tasks, err := board.ListTasks(context.Background(), "project-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("replay created %d cards, want 1", len(tasks))
	}
	card := tasks[0]
	if card.Status != provider.StatusToDo || card.Title != "Audit finding" || card.Description == "Untrusted report" {
		t.Fatalf("card was not normalized: %+v", card)
	}
	if !hasLabel(card.Labels, ExternalSourceLabel) {
		t.Fatalf("labels=%v, want %q", card.Labels, ExternalSourceLabel)
	}
	p, err := deps.ExtractProvenanceFromText(card.Description)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.ParseProvenanceJSON(b); err != nil {
		t.Fatalf("generated provenance is not parseable: %v", err)
	}
}

type failingCreator struct{}

func (failingCreator) CreateTask(context.Context, *provider.Task) (*provider.Task, error) {
	return nil, &provider.ProviderError{Provider: "memory", Op: "CreateTask", StatusCode: 503, Message: "unavailable"}
}

func TestTaskCreatedHandler_ProviderErrorIsReturned(t *testing.T) {
	r, store := newTestReceiver(t, testSecret, nil)
	defer store.Close()
	r.RegisterHandler(NewTaskCreatedHandler(failingCreator{}, "project-1"))
	body := []byte(`{"provider":"auditor","type":"task.created","task_ref":"EXT-10","project_id":"project-1","payload":{"title":"Audit finding"}}`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, newValidRequest(testSecret, "delivery-error", body))
	if w.Code < 400 {
		t.Fatalf("provider error returned success: %d", w.Code)
	}
	event, err := store.Get("delivery-error")
	if err != nil || event == nil || event.Status != StatusPending {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}
