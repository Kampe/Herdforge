package timeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func knownEvent(id, task string, at time.Time) Envelope {
	return Envelope{Version: Version1, ID: id, BuildRun: "run-1", Task: task, Attempt: "1", Lane: "build", Session: "session-1", Model: "gpt", Provider: "codex", Source: "lifecycle", Type: "transition", Time: at, Evidence: "sha256:evidence", Correlation: CorrelationKnown}
}

func TestStoreReadChronologicalAndFiltered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	late := knownEvent("late", "FAC-2", time.Date(2026, 8, 13, 12, 1, 0, 0, time.UTC))
	early := knownEvent("early", "FAC-1", time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if err := store.Append(late); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(early); err != nil {
		t.Fatal(err)
	}
	events, err := store.Read(Filter{Task: "FAC-1", Source: "lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != "early" {
		t.Fatalf("filtered chronological events = %+v", events)
	}
	all, err := store.Read(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "early" || all[1].ID != "late" {
		t.Fatalf("chronological events = %+v", all)
	}
}

func TestUnknownCorrelationCannotCarryGuessedIdentity(t *testing.T) {
	event := knownEvent("id", "FAC-1", time.Now().UTC())
	event.Correlation = CorrelationUnknown
	if err := event.Validate(); err == nil {
		t.Fatal("unknown correlation with identities must be rejected")
	}
}

func TestAppendSameEventIsIdempotentButRejectsReboundID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	event := knownEvent("stable", "FAC-1", time.Now())
	if err := store.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(event); err != nil {
		t.Fatalf("idempotent append: %v", err)
	}
	rebound := event
	rebound.Task = "FAC-2"
	if err := store.Append(rebound); err == nil {
		t.Fatal("rebound event ID must be rejected")
	}
	events, err := store.Read(Filter{})
	if err != nil || len(events) != 1 {
		t.Fatalf("events after retries = %+v, %v", events, err)
	}
}

func TestFromLifecycleRequiresTrustedCompleteBinding(t *testing.T) {
	_, err := FromLifecycle(Binding{Task: "FAC-1"}, LifecycleEvent{ID: 1, ToState: "claimed", Time: time.Now().UTC()})
	if err == nil {
		t.Fatal("incomplete lifecycle correlation must be blocked")
	}
}

func TestReadBlocksCorruptRecordsRatherThanReturningPartialAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(knownEvent("good", "FAC-1", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(mustRead(t, path), []byte("{not-json}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := store.Read(Filter{})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("read error = %v, want ErrCorrupt", err)
	}
	if events != nil {
		t.Fatalf("corrupt stream returned partial events: %+v", events)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
