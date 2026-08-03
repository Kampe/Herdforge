package daemon

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

// timeoutProvider fails List/Claim with a typed timeout after N ok calls.
type timeoutProvider struct {
	listOK   atomic.Int32
	claims   atomic.Int32
	failAfter int32 // list calls that succeed before timeout
	tasks    []*provider.Task
}

func (t *timeoutProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	for _, task := range t.tasks {
		if task.ID == id || task.Ref == id {
			return task, nil
		}
	}
	return nil, errors.New("not found")
}
func (t *timeoutProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	n := t.listOK.Add(1)
	if t.failAfter >= 0 && n > t.failAfter {
		<-ctx.Done()
		return nil, &provider.TimeoutError{Provider: "kaneo", Op: "ListTasks", Kind: provider.OpList, Cause: ctx.Err()}
	}
	var out []*provider.Task
	for _, task := range t.tasks {
		if status == "" || task.Status == status {
			out = append(out, task)
		}
	}
	return out, nil
}
func (t *timeoutProvider) ClaimTask(ctx context.Context, taskID, role string) error {
	t.claims.Add(1)
	return nil
}
func (t *timeoutProvider) UpdateStatus(ctx context.Context, taskID, status string) error {
	return nil
}
func (t *timeoutProvider) AddComment(ctx context.Context, taskID, body string) error {
	return nil
}

// RelationProvider (FAC-159): empty graph so selection is not capability-blocked.
func (t *timeoutProvider) ListRelations(context.Context, string) ([]provider.Relation, error) {
	return nil, nil
}
func (t *timeoutProvider) CreateRelation(context.Context, string, string, provider.RelationType) (*provider.Relation, error) {
	return nil, errors.New("timeoutProvider: create not used")
}
func (t *timeoutProvider) DeleteRelation(context.Context, string) error {
	return errors.New("timeoutProvider: delete not used")
}

func newTimeoutEngine(tp provider.TaskProvider) *Engine {
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{
			Type:      "kaneo",
			ProjectID: "p1",
			Deadlines: config.OpDeadlines{List: "50ms", Mutate: "50ms", Get: "50ms"},
		},
	}
	return NewEngine(cfg, tp, nil, nil, nil, nil)
}

func TestPulse_TimeoutProjectsBlockedAndRefusesClaim(t *testing.T) {
	tp := &timeoutProvider{
		failAfter: 0, // first list times out
		tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent,
				Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-1\",\"task_id\":\"1\",\"edges\":[]}\n```\n"},
		},
	}
	e := newTimeoutEngine(tp)
	_, err := e.RunPulse(context.Background(), "worker")
	if err == nil {
		t.Fatal("expected pulse error on provider timeout")
	}
	if !strings.Contains(err.Error(), "BLOCKED(provider_timeout)") {
		t.Fatalf("error should project BLOCKED: %v", err)
	}
	if e.ProviderStatus() != "BLOCKED(provider_timeout)" {
		t.Fatalf("status=%s", e.ProviderStatus())
	}
	if tp.claims.Load() != 0 {
		t.Fatalf("must not claim while timing out: claims=%d", tp.claims.Load())
	}

	// Second pulse while blocked: refuse without another claim.
	_, err = e.RunPulse(context.Background(), "worker")
	if err == nil || !strings.Contains(err.Error(), "BLOCKED(provider_timeout)") {
		t.Fatalf("blocked pulse must refuse: %v", err)
	}
	if tp.claims.Load() != 0 {
		t.Fatal("claim while blocked")
	}
}

func TestForgeLoop_TimeoutBlockedRecoveringOK(t *testing.T) {
	// failAfter=0: first list times out; after recovery probe, allow success.
	tp := &timeoutProvider{
		failAfter: 0,
		tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent, Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-1\",\"task_id\":\"1\",\"edges\":[]}\n```\n"},
		},
	}
	e := newTimeoutEngine(tp)
	logs := []string{}
	d := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{}},
		logs:       &logs,
	}

	// Tick 1: list in-review times out → BLOCKED, no dispatch.
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1}); err != nil {
		t.Fatal(err)
	}
	if e.ProviderStatus() != "BLOCKED(provider_timeout)" {
		t.Fatalf("after tick1 status=%s logs=%v", e.ProviderStatus(), logs)
	}
	if len(d.actions) != 0 {
		t.Fatalf("must not dispatch while blocked: %v", d.actions)
	}

	// Allow lists to succeed for recovery.
	tp.failAfter = 100
	// Tick 2: beginRecovery → recovering → successful list → ok → may dispatch.
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1}); err != nil {
		t.Fatal(err)
	}
	// After successful probe, health should be ok (recovering → ok on nil err).
	if e.ProviderHealth().State != ProviderOK {
		t.Fatalf("after recovery status=%s (want ok)", e.ProviderStatus())
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "BLOCKED(provider_timeout)") {
		t.Fatalf("logs must mention BLOCKED: %v", logs)
	}
	if !strings.Contains(joined, "recovering") {
		t.Fatalf("logs must mention recovering: %v", logs)
	}
}

// Mutation non-vacuity: if observe ignored timeouts, status would stay ok.
func TestProviderHealth_MutationTimeoutWouldStayOK(t *testing.T) {
	var h providerHealth
	h.state = ProviderOK
	h.observe(&provider.TimeoutError{Cause: context.DeadlineExceeded})
	if h.snapshot().State != ProviderBlocked {
		t.Fatal("timeout must force blocked")
	}
	h.beginRecovery()
	if h.snapshot().State != ProviderRecovering {
		t.Fatal("beginRecovery must enter recovering")
	}
	h.observe(nil)
	if h.snapshot().State != ProviderOK {
		t.Fatal("success while recovering must clear to ok")
	}
}

type recordingDriver struct {
	fakeDriver
	logs *[]string
}

func (r *recordingDriver) Log(msg string) {
	*r.logs = append(*r.logs, msg)
}
