package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AmbiguousMutationError reports that a write may or may not have landed
// after a timeout/cancel. Callers must NOT blind-retry the write and must
// NOT treat the mutation as success. Reconcile via readback / outbox.
type AmbiguousMutationError struct {
	Provider string
	Op       string
	TaskID   string
	Want     string // desired status or comment marker
	// WriteErr is the original timeout/cancel (or transport) failure.
	WriteErr error
	// ReadErr is set when post-timeout reconciliation read also failed.
	ReadErr error
	// Actual is the observed status when the read succeeded but did not match.
	Actual string
}

func (e *AmbiguousMutationError) Error() string {
	if e == nil {
		return "<nil ambiguous mutation>"
	}
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(": ")
	}
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString("ambiguous mutation")
	if e.TaskID != "" {
		b.WriteString(" on task ")
		b.WriteString(e.TaskID)
	}
	if e.Want != "" {
		fmt.Fprintf(&b, " (want %q", e.Want)
		if e.Actual != "" {
			fmt.Fprintf(&b, ", actual %q", e.Actual)
		}
		b.WriteString(")")
	}
	if e.WriteErr != nil {
		b.WriteString(": write: ")
		b.WriteString(e.WriteErr.Error())
	}
	if e.ReadErr != nil {
		b.WriteString("; readback: ")
		b.WriteString(e.ReadErr.Error())
	}
	return b.String()
}

func (e *AmbiguousMutationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.WriteErr
}

// IsAmbiguous reports whether err is (or wraps) an AmbiguousMutationError.
func IsAmbiguous(err error) bool {
	var ae *AmbiguousMutationError
	return errors.As(err, &ae)
}

// StatusReader is the read surface required for post-mutation reconciliation.
// TaskProvider implementations satisfy this via GetTask.
type StatusReader interface {
	GetTask(ctx context.Context, id string) (*Task, error)
}

// ReconcileStatus reads the task after an ambiguous write. Outcomes:
//
//   - read succeeds and status matches want → nil (write landed; no re-apply)
//   - read succeeds and status differs → *AmbiguousMutationError with Actual
//   - read fails → *AmbiguousMutationError with ReadErr
//
// want is normalized before comparison. This never issues a second write.
func ReconcileStatus(ctx context.Context, r StatusReader, provider, op, taskID, want string, writeErr error) error {
	if r == nil {
		return &AmbiguousMutationError{
			Provider: provider,
			Op:       op,
			TaskID:   taskID,
			Want:     NormalizeStatus(want),
			WriteErr: writeErr,
			ReadErr:  fmt.Errorf("nil status reader"),
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Always bound the reconciliation read independently of a dead write ctx.
	dctx, cancel := WithOpDeadline(ctx, DefaultDeadlines(), OpReadback)
	defer cancel()

	got, err := r.GetTask(dctx, taskID)
	if err != nil {
		return &AmbiguousMutationError{
			Provider: provider,
			Op:       op,
			TaskID:   taskID,
			Want:     NormalizeStatus(want),
			WriteErr: writeErr,
			ReadErr:  err,
		}
	}
	actual := ""
	if got != nil {
		actual = got.Status
	}
	if err := VerifyStatusReadback(taskID, want, actual); err != nil {
		return &AmbiguousMutationError{
			Provider: provider,
			Op:       op,
			TaskID:   taskID,
			Want:     NormalizeStatus(want),
			Actual:   NormalizeStatus(actual),
			WriteErr: writeErr,
		}
	}
	// Write landed. Success without re-applying.
	return nil
}

// AfterMutation handles the post-write path:
//
//   - writeErr == nil → VerifyStatusReadback via GetTask (normal readback)
//   - writeErr is timeout/cancel → ReconcileStatus (no second write)
//   - other writeErr → returned as-is
//
// parent is the caller context (not the expired write child). deadlines
// bound the readback/reconcile GetTask.
func AfterMutation(
	parent context.Context,
	r StatusReader,
	deadlines Deadlines,
	provider, op, taskID, want string,
	writeErr error,
) error {
	if parent == nil {
		parent = context.Background()
	}
	if writeErr != nil && !IsTimeout(writeErr) {
		return writeErr
	}
	if writeErr != nil {
		// Ambiguous timeout path: reconcile only.
		return ReconcileStatus(parent, r, provider, op, taskID, want, writeErr)
	}
	// Clean write: fail-closed readback.
	dctx, cancel := WithOpDeadline(parent, deadlines, OpReadback)
	defer cancel()
	got, err := r.GetTask(dctx, taskID)
	if err != nil {
		return fmt.Errorf("%s %s readback after write: %w", provider, op, err)
	}
	actual := ""
	if got != nil {
		actual = got.Status
	}
	return VerifyStatusReadback(taskID, want, actual)
}
