package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/store"
)

type fac507FailingProvider struct {
	*provider.MemoryProvider
}

func (p fac507FailingProvider) GetTask(context.Context, string) (*provider.Task, error) {
	return nil, errors.New("provider unavailable")
}

func TestRevalidateBlockedEvidenceArchivedAndDeletedTasks(t *testing.T) {
	tests := []struct {
		name     string
		archived bool
		deleted  bool
		wantRows int
	}{
		{name: "archived is suppressed", archived: true, wantRows: 0},
		{name: "deleted is suppressed", deleted: true, wantRows: 0},
		{name: "active remains visible", wantRows: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := tempStatusStore(t)
			tp := provider.NewMemoryProvider()
			status := provider.StatusInProgress
			if tc.archived {
				status = provider.StatusArchived
			}
			if !tc.deleted {
				if _, err := tp.CreateTask(context.Background(), &provider.Task{ID: "task", Ref: "FAC-507", Title: "status test", ProjectID: "test", Status: status}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.RecordBlockedSelection("FAC-507", "task", "pulse", "missing_provenance", "blocked", "graph", "provider"); err != nil {
				t.Fatal(err)
			}
			got, err := revalidateBlockedEvidence(context.Background(), st, tp, mustBlockedHistory(t, st))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantRows {
				t.Fatalf("visible rows = %d, want %d: %+v", len(got), tc.wantRows, got)
			}
		})
	}
}

func TestRevalidateBlockedEvidenceFailsClosedOnProviderError(t *testing.T) {
	st := tempStatusStore(t)
	if _, err := st.RecordBlockedSelection("FAC-507", "task", "pulse", "drift", "blocked", "graph", "provider"); err != nil {
		t.Fatal(err)
	}
	_, err := revalidateBlockedEvidence(context.Background(), st, fac507FailingProvider{provider.NewMemoryProvider()}, mustBlockedHistory(t, st))
	if err == nil {
		t.Fatal("provider read failure must not be treated as active evidence")
	}
}

func tempStatusStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir() + "/herdforge.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustBlockedHistory(t *testing.T, st *store.Store) []store.BlockedRecord {
	t.Helper()
	rows, err := st.BlockedSelectionHistory(10)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
