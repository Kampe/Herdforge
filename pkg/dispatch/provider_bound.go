package dispatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// ProviderLaneState mirrors daemon projection labels for dispatch callers.
type ProviderLaneState string

const (
	ProviderOK         ProviderLaneState = "ok"
	ProviderBlocked    ProviderLaneState = "blocked"
	ProviderRecovering ProviderLaneState = "recovering"
)

// ProviderHealth is the dispatch-side board health snapshot.
type ProviderHealth struct {
	State     ProviderLaneState
	Class     provider.OpFailureClass
	LastError string
	UpdatedAt time.Time
}

func (h ProviderHealth) String() string {
	switch h.State {
	case ProviderBlocked:
		if h.Class == provider.OpAmbiguous {
			return "BLOCKED(provider_ambiguous)"
		}
		return "BLOCKED(provider_timeout)"
	case ProviderRecovering:
		return "recovering"
	default:
		return "ok"
	}
}

type dispatchHealth struct {
	mu    sync.Mutex
	state ProviderLaneState
	class provider.OpFailureClass
	err   string
	at    time.Time
}

func (h *dispatchHealth) snapshot() ProviderHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	return ProviderHealth{State: h.state, Class: h.class, LastError: h.err, UpdatedAt: h.at}
}

func (h *dispatchHealth) observe(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.at = time.Now()
	if err == nil {
		h.class = provider.OpOK
		h.err = ""
		if h.state == ProviderRecovering || h.state == ProviderBlocked {
			// Successful bound call clears block (dispatch is one-shot; OK immediately).
			h.state = ProviderOK
		}
		return
	}
	class := provider.ClassifyOpError(err)
	h.class = class
	h.err = err.Error()
	if class == provider.OpTimeout || class == provider.OpAmbiguous {
		h.state = ProviderBlocked
	}
}

// ProviderHealth returns the last observed board call health for this dispatcher.
func (d *Dispatcher) ProviderHealth() ProviderHealth {
	if d == nil {
		return ProviderHealth{State: ProviderOK}
	}
	return d.health.snapshot()
}

// ProviderStatus is ok | recovering | BLOCKED(provider_timeout).
func (d *Dispatcher) ProviderStatus() string {
	return d.ProviderHealth().String()
}

func (d *Dispatcher) deadlines() provider.Deadlines {
	if d == nil || d.Config == nil {
		return provider.DefaultDeadlines()
	}
	g, l, m, c, r, err := d.Config.TaskProvider.Deadlines.Resolved()
	if err != nil {
		return provider.DefaultDeadlines()
	}
	return provider.DeadlinesFromParts(g, l, m, c, r)
}

func (d *Dispatcher) ensureDeadlinesApplied() {
	if d == nil || d.TaskProvider == nil {
		return
	}
	provider.ApplyDeadlines(d.TaskProvider, d.deadlines())
}

func (d *Dispatcher) listTasksBound(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpList)
	defer cancel()
	tasks, err := d.TaskProvider.ListTasks(opCtx, projectID, status)
	d.health.observe(err)
	return tasks, err
}

// getTaskBound is the O(1) task re-read used when selection already supplied
// the provider identity. Keeping it separate makes the bounded operation
// explicit and preserves the legacy list path for callers without an ID.
func (d *Dispatcher) getTaskBound(ctx context.Context, taskID string) (*provider.Task, error) {
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpGet)
	defer cancel()
	task, err := d.TaskProvider.GetTask(opCtx, taskID)
	d.health.observe(err)
	return task, err
}

func (d *Dispatcher) updateStatusBound(ctx context.Context, taskID, status string) error {
	// When a ClaimStack is wired (production FAC-147), fail closed unless
	// the caller uses updateStatusFenced with a live lease generation.
	if d != nil && d.Claims != nil {
		return fmt.Errorf("dispatch: Claims is set; use updateStatusFenced with a live lease (FAC-147 fail-closed)")
	}
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpMutate)
	defer cancel()
	err := d.TaskProvider.UpdateStatus(opCtx, taskID, status)
	d.health.observe(err)
	return err
}

// updateStatusFenced applies board status under ClaimStack MutateStatusGuarded
// (Begin/Complete + FencedCAS + AdvanceFence on the acquired generation).
func (d *Dispatcher) updateStatusFenced(ctx context.Context, task *provider.Task, role, status string) error {
	if d == nil || d.Claims == nil {
		return fmt.Errorf("dispatch: updateStatusFenced requires Claims")
	}
	if task == nil {
		return fmt.Errorf("dispatch: updateStatusFenced requires task")
	}
	if d.Config == nil {
		return fmt.Errorf("dispatch: updateStatusFenced requires Config")
	}
	owner, err := provider.ProcessOwnerID()
	if err != nil || owner == "" {
		return fmt.Errorf("process owner identity: %w", err)
	}
	taskRole, err := provider.TaskOwnershipRole(task, role)
	if err != nil {
		return err
	}
	key := provider.LeaseKey(".", d.Config.TaskProvider.Type, d.Config.TaskProvider.ProjectID, task.Ref)
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpMutate)
	defer cancel()
	_, err = d.Claims.MutateStatusGuarded(opCtx, key, owner, taskRole, taskRole, task.ID, status)
	d.health.observe(err)
	return err
}

func (d *Dispatcher) addCommentBound(ctx context.Context, taskID, body string) error {
	if d != nil && d.Claims != nil {
		return fmt.Errorf("dispatch: Claims is set; use addCommentFenced with a live lease (FAC-147 fail-closed)")
	}
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpComment)
	defer cancel()
	err := d.TaskProvider.AddComment(opCtx, taskID, body)
	d.health.observe(err)
	return err
}

// addCommentFenced posts a board comment under ClaimStack MutateCommentGuarded.
func (d *Dispatcher) addCommentFenced(ctx context.Context, task *provider.Task, role, body string) error {
	if d == nil || d.Claims == nil {
		return fmt.Errorf("dispatch: addCommentFenced requires Claims")
	}
	if task == nil {
		return fmt.Errorf("dispatch: addCommentFenced requires task")
	}
	if d.Config == nil {
		return fmt.Errorf("dispatch: addCommentFenced requires Config")
	}
	owner, err := provider.ProcessOwnerID()
	if err != nil || owner == "" {
		return fmt.Errorf("process owner identity: %w", err)
	}
	taskRole, err := provider.TaskOwnershipRole(task, role)
	if err != nil {
		return err
	}
	key := provider.LeaseKey(".", d.Config.TaskProvider.Type, d.Config.TaskProvider.ProjectID, task.Ref)
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpComment)
	defer cancel()
	_, err = d.Claims.MutateCommentGuarded(opCtx, key, owner, taskRole, taskRole, task.ID, body)
	d.health.observe(err)
	return err
}

func formatBoardErr(op string, err error) error {
	if err == nil {
		return nil
	}
	class := provider.ClassifyOpError(err)
	if class == provider.OpTimeout || class == provider.OpAmbiguous {
		return fmt.Errorf("%s: BLOCKED(provider_timeout): %w", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// listActiveTasksBound resolves a ticket ref without walking terminal columns.
//
// A dispatchable ticket is by definition not done or archived, but the lookup
// asked for the whole board and paid for it: this repository's done column
// held 525 cards across 7 pages and exceeded the list deadline on its own, so
// dispatch failed with BLOCKED(provider_timeout) while resolving a single ref.
func (d *Dispatcher) listActiveTasksBound(ctx context.Context, projectID string) ([]*provider.Task, error) {
	d.ensureDeadlinesApplied()
	// The bound is SCALED to the fan-out, not applied as if this were one call.
	// ListActiveTasks reads every active column, and each inner ListTasks
	// already carries its own OpList deadline and read retry. Budgeting the
	// whole fan-out with a single per-op deadline made it expire before columns
	// that individually answered well inside their own budget, so dispatch
	// failed to resolve a ref that was sitting on the board.
	//
	// It must still be bounded: removing the ceiling entirely lets a stalled
	// provider hang the caller forever (pkg/dispatch hung to a 600s test
	// timeout when this was unbounded).
	opCtx, cancel := context.WithTimeout(ctx, activeFanOutDeadline(d.deadlines()))
	defer cancel()
	tasks, err := provider.ListActiveTasks(opCtx, d.TaskProvider, projectID)
	d.health.observe(err)
	return tasks, err
}

// activeFanOutDeadline is the per-column list budget multiplied by the number
// of columns the fan-out reads, so a wider board gets proportionally more time
// while the operation stays bounded. A caller deadline still wins.
func activeFanOutDeadline(d provider.Deadlines) time.Duration {
	columns := len(provider.ActiveStatuses())
	if columns < 1 {
		columns = 1
	}
	return d.For(provider.OpList) * time.Duration(columns)
}
