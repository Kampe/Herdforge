package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/security"
)

func writeLaunchReceipts(t *testing.T, path string, receipts ...launch.Receipt) []byte {
	t.Helper()
	body := make([]byte, 0)
	for _, receipt := range receipts {
		row, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, row...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return body
}

func genuinePreEditReceipt(taskRef, name, cwd string) launch.Receipt {
	return launch.Receipt{
		CreatedAt:     time.Unix(10, 0).UTC(),
		TaskRef:       taskRef,
		Accepted:      true,
		Name:          name,
		CWD:           cwd,
		Branch:        "herd/" + strings.ToLower(taskRef),
		Provider:      "codex",
		Model:         "gpt-5.6-sol",
		BuilderFamily: "openai",
	}
}

func seedStaleDispatchRecovery(t *testing.T) (*runstate.Store, *provider.MemoryProvider, *provider.Task, runstate.RunState, runstate.RunState) {
	t.Helper()
	ctx := context.Background()
	store, err := runstate.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	task := &provider.Task{ID: "task-1", Ref: "FAC-667", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(10, 0)}
	tasks := provider.NewMemoryProvider()
	tasks.AddTask(task)
	stale, err := runstate.FromTasks("dispatch:"+task.ID, "dispatch", task.Ref, "graph-old", runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Checkpoint(ctx, stale, 0); err != nil {
		t.Fatal(err)
	}
	unrelatedTask := &provider.Task{ID: "task-other", Ref: "FAC-OTHER", ProjectID: task.ProjectID, Status: provider.StatusToDo, UpdatedAt: time.Unix(20, 0)}
	unrelated, err := runstate.FromTasks("dispatch:"+unrelatedTask.ID, "dispatch", unrelatedTask.Ref, "graph-other", runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{unrelatedTask})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Checkpoint(ctx, unrelated, 0); err != nil {
		t.Fatal(err)
	}
	return store, tasks, task, stale, unrelated
}

func TestReceiptLiveLaunchLookupAllowsGenuinePreEditReceiptAfterAuthoritativeAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	receipt := genuinePreEditReceipt("FAC-667", "task-fac-667-sol", ".herd/worktrees/fac-667")
	writeLaunchReceipts(t, path, receipt)
	listCalled := false
	lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) {
		listCalled = true
		return []herdr.AgentEntry{}, nil
	})
	live, err := lookup.HasLiveLaunch(context.Background(), "n9qcrrippi3e7ovvxfklcb77", receipt.TaskRef)
	if err != nil {
		t.Fatalf("genuine pre-edit receipt was treated as UNKNOWN: %v", err)
	}
	if live {
		t.Fatal("genuine pre-edit receipt with authoritative Herdr absence was treated as live")
	}
	if !listCalled {
		t.Fatal("pre-edit receipt permitted recovery without authoritative Herdr inventory")
	}
}

func TestReceiptLiveLaunchLookupFindsExactAcceptedLiveIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	receipt := launch.Receipt{CreatedAt: time.Unix(10, 0).UTC(), TaskRef: "FAC-654", Accepted: true, Name: "task-fac-654", CWD: ".herd/worktrees/fac-654", TabID: "wK:tab-1", PaneID: "wK:pane-1", HerdrSession: "session-1"}
	writeLaunchReceipts(t, path, receipt)
	lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) {
		return []herdr.AgentEntry{{Name: receipt.Name, Workspace: "wK", Cwd: receipt.CWD, TabID: receipt.TabID, PaneID: receipt.PaneID, Session: herdr.AgentSession{Value: receipt.HerdrSession}}}, nil
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
		name    string
		receipt launch.Receipt
		body    string
	}{
		{name: "malformed row", body: "{not-json}\n"},
		{name: "incomplete pre-edit provenance", receipt: launch.Receipt{TaskRef: "FAC-654", Accepted: true, Name: "task-fac-654", CWD: ".herd/worktrees/fac-654"}},
		{name: "tab without pane or session", receipt: launch.Receipt{TaskRef: "FAC-654", Accepted: true, Name: "task-fac-654", CWD: ".herd/worktrees/fac-654", TabID: "wK:tab-1"}},
		{name: "tab and pane without session", receipt: launch.Receipt{TaskRef: "FAC-654", Accepted: true, Name: "task-fac-654", CWD: ".herd/worktrees/fac-654", TabID: "wK:tab-1", PaneID: "wK:pane-1"}},
		{name: "session without tab or pane", receipt: launch.Receipt{TaskRef: "FAC-654", Accepted: true, Name: "task-fac-654", CWD: ".herd/worktrees/fac-654", HerdrSession: "session-1"}},
		{name: "post-launch identity without workspace-qualified ids", receipt: launch.Receipt{TaskRef: "FAC-654", Accepted: true, Name: "task-fac-654", CWD: ".herd/worktrees/fac-654", TabID: "tab-1", PaneID: "pane-1", HerdrSession: "session-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
			if tt.body != "" {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				writeLaunchReceipts(t, path, tt.receipt)
			}
			lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{}, nil })
			if live, err := lookup.HasLiveLaunch(context.Background(), "task-1", "FAC-654"); err == nil {
				t.Fatalf("UNKNOWN receipt evidence returned live=%v with no error", live)
			}
		})
	}
}

func TestReceiptLiveLaunchLookupRequiresExactTaskEvidenceAndHerdrRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	writeLaunchReceipts(t, path, genuinePreEditReceipt("FAC-OTHER", "task-fac-667-sol", ".herd/worktrees/fac-667"))
	listCalled := false
	lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) {
		listCalled = true
		return []herdr.AgentEntry{}, nil
	})
	if live, err := lookup.HasLiveLaunch(context.Background(), "task-1", "FAC-667"); err == nil {
		t.Fatalf("missing exact task receipt returned live=%v without UNKNOWN", live)
	}
	if listCalled {
		t.Fatal("Herdr listing was consulted without exact task-bound receipt evidence")
	}

	writeLaunchReceipts(t, path, genuinePreEditReceipt("FAC-667", "task-fac-667-sol", ".herd/worktrees/fac-667"))
	want := errors.New("herdr unavailable")
	lookup = NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) { return nil, want })
	if live, err := lookup.HasLiveLaunch(context.Background(), "task-1", "FAC-667"); !errors.Is(err, want) {
		t.Fatalf("Herdr failure returned live=%v err=%v, want wrapped %v", live, err, want)
	}
}

func TestReceiptLiveLaunchLookupDoesNotConfuseSameNameWrongTaskWorktree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	receipt := genuinePreEditReceipt("FAC-667", "task-fac-667-sol", ".herd/worktrees/fac-667")
	writeLaunchReceipts(t, path,
		launch.Receipt{TaskRef: "FAC-OTHER", Accepted: true, Name: receipt.Name, TabID: "partial-unrelated"},
		receipt,
	)
	lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) {
		return []herdr.AgentEntry{{
			Name: receipt.Name, Workspace: "wK", Cwd: ".herd/worktrees/fac-other",
			TabID: "wK:tab-other", PaneID: "wK:pane-other", Session: herdr.AgentSession{Value: "session-other"},
		}}, nil
	})
	live, err := lookup.HasLiveLaunch(context.Background(), "task-1", receipt.TaskRef)
	if err != nil {
		t.Fatal(err)
	}
	if live {
		t.Fatal("same-name agent in another task worktree was treated as the exact live launch")
	}
}

func TestReceiptLiveLaunchLookupRefusesAmbiguousMatchingLiveIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	receipt := genuinePreEditReceipt("FAC-667", "task-fac-667-sol", ".herd/worktrees/fac-667")
	writeLaunchReceipts(t, path, receipt)
	lookup := NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) {
		return []herdr.AgentEntry{
			{Name: receipt.Name, Workspace: "wK", Cwd: receipt.CWD, TabID: "wK:tab-1", PaneID: "wK:pane-1", Session: herdr.AgentSession{Value: "session-1"}},
			{Name: receipt.Name, Workspace: "wK", Cwd: receipt.CWD, TabID: "wK:tab-2", PaneID: "wK:pane-2", Session: herdr.AgentSession{Value: "session-2"}},
		}, nil
	})
	if live, err := lookup.HasLiveLaunch(context.Background(), "task-1", receipt.TaskRef); err == nil {
		t.Fatalf("ambiguous matching live identities returned live=%v without UNKNOWN", live)
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

func TestRecoverStaleRunWithGenuinePreEditReceiptIsIdempotentAndPreservesEvidence(t *testing.T) {
	ctx := context.Background()
	store, tasks, task, _, unrelated := seedStaleDispatchRecovery(t)
	receiptPath := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	receiptBody := writeLaunchReceipts(t, receiptPath, genuinePreEditReceipt(task.Ref, "task-fac-667-sol", ".herd/worktrees/fac-667"))
	recovery := StaleRunRecovery{
		Runs: store, Tasks: tasks, ProjectID: task.ProjectID,
		Graph:  func(context.Context) (string, error) { return "graph-new", nil },
		Claims: security.MapClaimLookup{},
		Launches: NewReceiptLiveLaunchLookup(receiptPath, func() ([]herdr.AgentEntry, error) {
			return []herdr.AgentEntry{}, nil
		}),
	}
	first, err := recovery.Recover(ctx, task.ID, task.Ref)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := recovery.Recover(ctx, task.ID, task.Ref)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if retry.Revision != first.Revision || !retry.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("retry rewrote recovered run: first=%+v retry=%+v", first, retry)
	}
	readback, err := store.Load(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if readback.Revision != first.Revision || readback.Recovery == nil {
		t.Fatalf("recovered run readback=%+v", readback)
	}
	kept, err := store.Load(ctx, unrelated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Revision != 1 || kept.DependencyGraphRevision != "graph-other" || kept.Recovery != nil {
		t.Fatalf("unrelated run changed: %+v", kept)
	}
	after, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, receiptBody) {
		t.Fatalf("append-only launch receipt evidence changed: before=%q after=%q", receiptBody, after)
	}
}

func TestRecoverStaleRunReceiptAuthorityFailuresDoNotMutateRun(t *testing.T) {
	wantHerdr := errors.New("herdr inventory unavailable")
	tests := []struct {
		name     string
		receipt  launch.Receipt
		raw      string
		agents   []herdr.AgentEntry
		listErr  error
		wantLive bool
	}{
		{
			name: "complete live identity",
			receipt: launch.Receipt{CreatedAt: time.Unix(10, 0).UTC(), TaskRef: "FAC-667", Accepted: true, Name: "task-fac-667-sol", CWD: ".herd/worktrees/fac-667",
				TabID: "wK:tab-1", PaneID: "wK:pane-1", HerdrSession: "session-1"},
			agents:   []herdr.AgentEntry{{Name: "task-fac-667-sol", Workspace: "wK", Cwd: ".herd/worktrees/fac-667", TabID: "wK:tab-1", PaneID: "wK:pane-1", Session: herdr.AgentSession{Value: "session-1"}}},
			wantLive: true,
		},
		{name: "partial post-launch identity", receipt: launch.Receipt{TaskRef: "FAC-667", Accepted: true, Name: "task-fac-667-sol", CWD: ".herd/worktrees/fac-667", TabID: "wK:tab-1"}},
		{name: "malformed ledger", raw: "{not-json}\n"},
		{name: "Herdr listing failure", receipt: genuinePreEditReceipt("FAC-667", "task-fac-667-sol", ".herd/worktrees/fac-667"), listErr: wantHerdr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, tasks, task, stale, unrelated := seedStaleDispatchRecovery(t)
			path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
			if tt.raw != "" {
				if err := os.WriteFile(path, []byte(tt.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				writeLaunchReceipts(t, path, tt.receipt)
			}
			recovery := StaleRunRecovery{
				Runs: store, Tasks: tasks, ProjectID: task.ProjectID,
				Graph:  func(context.Context) (string, error) { return "graph-new", nil },
				Claims: security.MapClaimLookup{},
				Launches: NewReceiptLiveLaunchLookup(path, func() ([]herdr.AgentEntry, error) {
					return tt.agents, tt.listErr
				}),
			}
			_, err := recovery.Recover(ctx, task.ID, task.Ref)
			if err == nil {
				t.Fatal("recovery authority failure was accepted")
			}
			if tt.wantLive && !errors.Is(err, ErrRecoveryActive) {
				t.Fatalf("complete live identity err=%v, want ErrRecoveryActive", err)
			}
			if tt.listErr != nil && !errors.Is(err, tt.listErr) {
				t.Fatalf("Herdr error=%v, want wrapped %v", err, tt.listErr)
			}
			kept, loadErr := store.Load(ctx, stale.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if kept.Revision != 1 || kept.DependencyGraphRevision != "graph-old" || kept.Recovery != nil {
				t.Fatalf("refused recovery mutated target row: %+v", kept)
			}
			other, loadErr := store.Load(ctx, unrelated.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if other.Revision != 1 || other.DependencyGraphRevision != "graph-other" || other.Recovery != nil {
				t.Fatalf("refused recovery mutated unrelated row: %+v", other)
			}
		})
	}
}

func TestRecoverStaleRunWithPreEditReceiptHasOneConcurrentWinner(t *testing.T) {
	ctx := context.Background()
	store, tasks, task, _, unrelated := seedStaleDispatchRecovery(t)
	receiptPath := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	writeLaunchReceipts(t, receiptPath, genuinePreEditReceipt(task.Ref, "task-fac-667-sol", ".herd/worktrees/fac-667"))
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	var listMu sync.Mutex
	listCalls := 0
	list := func() ([]herdr.AgentEntry, error) {
		listMu.Lock()
		listCalls++
		listMu.Unlock()
		ready <- struct{}{}
		<-release
		return []herdr.AgentEntry{}, nil
	}
	type result struct {
		state *runstate.RunState
		err   error
	}
	results := make(chan result, 2)
	for i, graph := range []string{"graph-a", "graph-b"} {
		i, graph := i, graph
		go func() {
			recovery := StaleRunRecovery{
				Runs: store, Tasks: tasks, ProjectID: task.ProjectID,
				Graph: func(context.Context) (string, error) {
					return graph, nil
				},
				Claims:   security.MapClaimLookup{},
				Launches: NewReceiptLiveLaunchLookup(receiptPath, list),
			}
			state, err := recovery.Recover(ctx, task.ID, task.Ref)
			if err != nil {
				err = fmt.Errorf("candidate %d: %w", i, err)
			}
			results <- result{state: state, err: err}
		}()
	}
	<-ready
	<-ready
	close(release)
	var successes, conflicts int
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.state != nil:
			successes++
		case errors.Is(result.err, runstate.ErrConcurrent):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result: state=%+v err=%v", result.state, result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent outcomes success=%d conflict=%d", successes, conflicts)
	}
	listMu.Lock()
	if listCalls != 2 {
		t.Fatalf("Herdr authoritative reads=%d, want 2", listCalls)
	}
	listMu.Unlock()
	kept, err := store.Load(ctx, unrelated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Revision != 1 || kept.DependencyGraphRevision != "graph-other" || kept.Recovery != nil {
		t.Fatalf("concurrent recovery mutated unrelated run: %+v", kept)
	}
}
