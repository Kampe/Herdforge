package provider

import (
	"context"
	"fmt"
	"time"
)

// BoundClient is the production TaskProvider wrapper: every external board
// call gets a configured per-op deadline and timeout/ambiguous failures are
// labeled BLOCKED(provider_timeout) for control-plane consumers (FAC-150).
// Non-Kaneo live activation remains FAC-155; this type is provider-agnostic.
type BoundClient struct {
	Inner     TaskProvider
	Deadlines Deadlines
}

// NewBoundClient wraps inner with d (normalized). Inner must be non-nil.
func NewBoundClient(inner TaskProvider, d Deadlines) *BoundClient {
	if inner == nil {
		return nil
	}
	return &BoundClient{Inner: inner, Deadlines: d.Normalize()}
}

func (b *BoundClient) deadlines() Deadlines {
	if b == nil {
		return DefaultDeadlines()
	}
	return b.Deadlines.Normalize()
}

func (b *BoundClient) wrap(op string, kind OpKind, err error) error {
	if err == nil {
		return nil
	}
	// Preserve AmbiguousMutationError before AsTimeout (which would surface the
	// nested TimeoutError and drop the ambiguous classification).
	if IsAmbiguous(err) {
		return fmt.Errorf("%s: BLOCKED(provider_timeout): %w", op, err)
	}
	err = AsTimeout("provider", op, kind, b.deadlines().For(kind), err)
	class := ClassifyOpError(err)
	if class == OpTimeout || class == OpAmbiguous {
		return fmt.Errorf("%s: BLOCKED(provider_timeout): %w", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func (b *BoundClient) GetTask(ctx context.Context, id string) (*Task, error) {
	if b == nil || b.Inner == nil {
		return nil, fmt.Errorf("GetTask: nil provider")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpGet)
	defer cancel()
	t, err := b.Inner.GetTask(opCtx, id)
	return t, b.wrap("GetTask", OpGet, err)
}

func (b *BoundClient) ListTasks(ctx context.Context, projectID, status string) ([]*Task, error) {
	if b == nil || b.Inner == nil {
		return nil, fmt.Errorf("ListTasks: nil provider")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpList)
	defer cancel()
	tasks, err := b.Inner.ListTasks(opCtx, projectID, status)
	if err != nil {
		return nil, b.wrap("ListTasks", OpList, err)
	}
	// Never treat timeout as empty success: nil err + nil/empty is fine;
	// a wrapped timeout already returned above.
	return tasks, nil
}

func (b *BoundClient) ClaimTask(ctx context.Context, taskID, role string) error {
	if b == nil || b.Inner == nil {
		return fmt.Errorf("ClaimTask: nil provider")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("ClaimTask", OpMutate, b.Inner.ClaimTask(opCtx, taskID, role))
}

func (b *BoundClient) UpdateStatus(ctx context.Context, taskID, status string) error {
	if b == nil || b.Inner == nil {
		return fmt.Errorf("UpdateStatus: nil provider")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("UpdateStatus", OpMutate, b.Inner.UpdateStatus(opCtx, taskID, status))
}

func (b *BoundClient) AddComment(ctx context.Context, taskID, body string) error {
	if b == nil || b.Inner == nil {
		return fmt.Errorf("AddComment: nil provider")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpComment)
	defer cancel()
	return b.wrap("AddComment", OpComment, b.Inner.AddComment(opCtx, taskID, body))
}

// ConfigDeadlineSource is the subset of config needed without importing
// pkg/config (avoids cycles). cmd and packages pass Resolved() parts.
type ConfigDeadlineSource interface {
	// Resolved returns get,list,mutate,comment,readback durations (0 = default).
	ResolvedDeadlines() (get, list, mutate, comment, readback time.Duration, err error)
}

// DeadlinesFromResolved maps Resolved() output to Deadlines.
func DeadlinesFromResolved(get, list, mutate, comment, readback time.Duration, err error) Deadlines {
	if err != nil {
		return DefaultDeadlines()
	}
	return DeadlinesFromParts(get, list, mutate, comment, readback)
}

// Ensure BoundClient implements TaskProvider.
var _ TaskProvider = (*BoundClient)(nil)
