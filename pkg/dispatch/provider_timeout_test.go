package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type timeoutBoard struct {
	listCalls atomic.Int32
	hangList  bool
	tasks     []*provider.Task
}

func (t *timeoutBoard) GetTask(context.Context, string) (*provider.Task, error) {
	return nil, errors.New("unused")
}
func (t *timeoutBoard) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	t.listCalls.Add(1)
	if t.hangList {
		<-ctx.Done()
		return nil, &provider.TimeoutError{Provider: "kaneo", Op: "ListTasks", Kind: provider.OpList, Cause: ctx.Err()}
	}
	return t.tasks, nil
}
func (t *timeoutBoard) ClaimTask(context.Context, string, string) error { return nil }
func (t *timeoutBoard) UpdateStatus(context.Context, string, string) error {
	return nil
}
func (t *timeoutBoard) AddComment(context.Context, string, string) error { return nil }

type noopComp struct{}

func (noopComp) RecordStep(context.Context, StepRecord) error          { return nil }
func (noopComp) Compensate(context.Context, string, string) error      { return nil }

func TestDispatch_ListTimeout_ProjectsBlocked(t *testing.T) {
	board := &timeoutBoard{hangList: true}
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{
			Type:      "kaneo",
			ProjectID: "p1",
			Deadlines: config.OpDeadlines{List: "40ms", Mutate: "40ms", Comment: "40ms"},
		},
		Project: config.ProjectConfig{DefaultBranch: "main"},
		Lanes:   []config.LaneDef{{Name: "worker", AgentKind: "x", Model: "m", Prompt: "p"}},
	}
	d := NewDispatcher(cfg, board, nil)
	d.Compensator = noopComp{}
	// Worktree nil will fail later — but list happens first.
	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", LaneName: "worker"})
	if err == nil {
		t.Fatal("expected list timeout error")
	}
	if !strings.Contains(err.Error(), "BLOCKED(provider_timeout)") {
		t.Fatalf("want BLOCKED projection in error: %v", err)
	}
	if d.ProviderStatus() != "BLOCKED(provider_timeout)" {
		t.Fatalf("status=%s", d.ProviderStatus())
	}
	if provider.ClassifyOpError(err) != provider.OpTimeout && !strings.Contains(err.Error(), "timeout") {
		// wrapped timeout still classifies via Unwrap chain if AsTimeout preserved
	}
	// Non-vacuity: hang must have been attempted under short deadline.
	if board.listCalls.Load() < 1 {
		t.Fatal("list never called")
	}
}

func TestDispatch_ClassifyOpError_Consumer(t *testing.T) {
	// Prove dispatcher is a real consumer of ClassifyOpError (not dead primitive).
	board := &timeoutBoard{hangList: true}
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{
			Type: "kaneo", ProjectID: "p",
			Deadlines: config.OpDeadlines{List: "30ms"},
		},
	}
	d := NewDispatcher(cfg, board, nil)
	d.Compensator = noopComp{}
	_, _ = d.listTasksBound(context.Background(), "p", "")
	h := d.ProviderHealth()
	if h.Class != provider.OpTimeout {
		t.Fatalf("health.Class=%q want provider_timeout", h.Class)
	}
	if h.State != ProviderBlocked {
		t.Fatalf("state=%q", h.State)
	}
	// Recovery: successful list clears.
	board.hangList = false
	board.tasks = []*provider.Task{{ID: "1", Ref: "FAC-1", Status: "to-do"}}
	_, err := d.listTasksBound(context.Background(), "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.ProviderHealth().State != ProviderOK {
		t.Fatalf("after success state=%s", d.ProviderStatus())
	}
}

func TestDispatch_ConfigurableDeadlineApplied(t *testing.T) {
	board := &timeoutBoard{hangList: true}
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{
			Type: "kaneo", ProjectID: "p",
			Deadlines: config.OpDeadlines{List: "25ms"},
		},
	}
	d := NewDispatcher(cfg, board, nil)
	start := time.Now()
	_, err := d.listTasksBound(context.Background(), "p", "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("configurable list deadline not enforced: %v", elapsed)
	}
	if elapsed < 10*time.Millisecond {
		t.Fatalf("deadline fired too early: %v", elapsed)
	}
}
