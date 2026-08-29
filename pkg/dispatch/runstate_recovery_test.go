package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/security"
)

func TestReceiptLiveLaunchLookupFindsExactAcceptedLiveIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	receipt := launch.Receipt{TaskRef: "FAC-654", Accepted: true, Name: "task-fac-654", TabID: "tab-1", PaneID: "pane-1", HerdrSession: "session-1"}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) {
		return []herdr.AgentEntry{{Name: receipt.Name, TabID: receipt.TabID, PaneID: receipt.PaneID, Session: herdr.AgentSession{Value: receipt.HerdrSession}}}, nil
	})
	live, err := lookup.HasLiveLaunch(context.Background(), "task-1", receipt.TaskRef)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("accepted receipt with exact live Herdr identity was treated as absent")
	}
}

func TestReceiptLiveLaunchLookupRefusesUnknownReceiptEvidence(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed row", body: "{not-json}\n"},
		{name: "accepted task row lacks exact launch identity", body: `{"task_ref":"FAC-654","accepted":true,"name":"task-fac-654"}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{}, nil })
			if live, err := lookup.HasLiveLaunch(context.Background(), "task-1", "FAC-654"); err == nil {
				t.Fatalf("UNKNOWN receipt evidence returned live=%v with no error", live)
			}
		})
	}
}

func TestRecoverStaleRunRefusesLiveLeaseOrAdmittedLaunchWithoutMutation(t *testing.T) {
	tests := []struct {
		name     string
		claims   security.LiveClaimLookup
		launches LiveLaunchLookup
	}{
		{
			name:   "active lease",
			claims: security.MapClaimLookup{"FAC-654": {TaskRef: "FAC-654", Generation: 7, ExpiresAt: time.Now().Add(time.Hour)}},
			launches: LiveLaunchLookupFunc(func(context.Context, string, string) (bool, error) {
				return false, nil
			}),
		},
		{
			name:   "live admitted launch",
			claims: security.MapClaimLookup{},
			launches: LiveLaunchLookupFunc(func(context.Context, string, string) (bool, error) {
				return true, nil
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := runstate.Open(filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			task := &provider.Task{ID: "task-1", Ref: "FAC-654", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(10, 0)}
			tasks := provider.NewMemoryProvider()
			tasks.AddTask(task)
			stale, err := runstate.FromTasks("dispatch:task-1", "dispatch", task.Ref, "graph-old", runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Checkpoint(ctx, stale, 0); err != nil {
				t.Fatal(err)
			}

			recovery := StaleRunRecovery{
				Runs: store, Tasks: tasks, ProjectID: task.ProjectID,
				Graph:    func(context.Context) (string, error) { return "graph-new", nil },
				Claims:   tt.claims,
				Launches: tt.launches,
			}
			if _, err := recovery.Recover(ctx, task.ID, task.Ref); !errors.Is(err, ErrRecoveryActive) {
				t.Fatalf("recovery with live authority err=%v, want ErrRecoveryActive", err)
			}
			kept, err := store.Load(ctx, stale.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kept.Revision != 1 || kept.DependencyGraphRevision != "graph-old" {
				t.Fatalf("refused recovery mutated row: %+v", kept)
			}
		})
	}
}
