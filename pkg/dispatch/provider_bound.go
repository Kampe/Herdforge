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

func (d *Dispatcher) updateStatusBound(ctx context.Context, taskID, status string) error {
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpMutate)
	defer cancel()
	err := d.TaskProvider.UpdateStatus(opCtx, taskID, status)
	d.health.observe(err)
	return err
}

func (d *Dispatcher) addCommentBound(ctx context.Context, taskID, body string) error {
	d.ensureDeadlinesApplied()
	opCtx, cancel := provider.BoundOp(ctx, d.deadlines(), provider.OpComment)
	defer cancel()
	err := d.TaskProvider.AddComment(opCtx, taskID, body)
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
