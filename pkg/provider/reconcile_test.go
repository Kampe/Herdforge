package provider

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubReader struct {
	task *Task
	err  error
	n    atomic.Int32
}

func (s *stubReader) GetTask(ctx context.Context, id string) (*Task, error) {
	s.n.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.task, nil
}

// countingWriter simulates a mutation that times out once and must not be
// re-invoked by reconciliation.
type countingWriter struct {
	writes atomic.Int32
	reader StatusReader
}

func (c *countingWriter) UpdateStatus(ctx context.Context, taskID, status string) error {
	c.writes.Add(1)
	// Simulate hang until deadline.
	<-ctx.Done()
	return AsTimeout("kaneo", "UpdateStatus", OpMutate, DefaultMutateDeadline, ctx.Err())
}

func TestReconcileStatus_WriteLanded(t *testing.T) {
	r := &stubReader{task: &Task{ID: "t1", Status: StatusInProgress}}
	writeErr := &TimeoutError{Provider: "kaneo", Op: "UpdateStatus", Cause: context.DeadlineExceeded}
	err := ReconcileStatus(context.Background(), r, DefaultDeadlines(), "kaneo", "UpdateStatus", "t1", StatusInProgress, writeErr)
	if err != nil {
		t.Fatalf("landed write should reconcile clean: %v", err)
	}
	if r.n.Load() != 1 {
		t.Fatalf("GetTask calls=%d want 1", r.n.Load())
	}
}

func TestReconcileStatus_WriteDidNotLand(t *testing.T) {
	r := &stubReader{task: &Task{ID: "t1", Status: StatusToDo}}
	writeErr := &TimeoutError{Cause: context.DeadlineExceeded}
	err := ReconcileStatus(context.Background(), r, DefaultDeadlines(), "kaneo", "UpdateStatus", "t1", StatusInProgress, writeErr)
	if !IsAmbiguous(err) {
		t.Fatalf("want AmbiguousMutationError, got %T %v", err, err)
	}
	var ae *AmbiguousMutationError
	errors.As(err, &ae)
	if ae.Actual != StatusToDo || ae.Want != StatusInProgress {
		t.Fatalf("Actual=%q Want=%q", ae.Actual, ae.Want)
	}
	// Must not look like success.
	if err == nil {
		t.Fatal("unreachable")
	}
}

func TestReconcileStatus_ReadAlsoFails(t *testing.T) {
	r := &stubReader{err: errors.New("board down")}
	writeErr := &TimeoutError{Cause: context.Canceled}
	err := ReconcileStatus(context.Background(), r, DefaultDeadlines(), "kaneo", "UpdateStatus", "t1", StatusDone, writeErr)
	if !IsAmbiguous(err) {
		t.Fatalf("want ambiguous, got %v", err)
	}
	var ae *AmbiguousMutationError
	errors.As(err, &ae)
	if ae.ReadErr == nil || !strings.Contains(ae.ReadErr.Error(), "board down") {
		t.Fatalf("ReadErr=%v", ae.ReadErr)
	}
}

func TestAfterMutation_CleanWriteReadback(t *testing.T) {
	r := &stubReader{task: &Task{ID: "t1", Status: StatusDone}}
	err := AfterMutation(context.Background(), r, DefaultDeadlines(), "kaneo", "UpdateStatus", "t1", StatusDone, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAfterMutation_CleanWriteDrift(t *testing.T) {
	r := &stubReader{task: &Task{ID: "t1", Status: StatusToDo}}
	err := AfterMutation(context.Background(), r, DefaultDeadlines(), "kaneo", "UpdateStatus", "t1", StatusDone, nil)
	var de *ReadbackDriftError
	if !errors.As(err, &de) {
		t.Fatalf("want ReadbackDriftError, got %T %v", err, err)
	}
}

func TestAfterMutation_NonTimeoutWriteErrPassthrough(t *testing.T) {
	r := &stubReader{task: &Task{ID: "t1", Status: StatusDone}}
	plain := errors.New("http 500")
	err := AfterMutation(context.Background(), r, DefaultDeadlines(), "kaneo", "UpdateStatus", "t1", StatusDone, plain)
	if !errors.Is(err, plain) {
		t.Fatalf("got %v", err)
	}
	if r.n.Load() != 0 {
		t.Fatal("must not readback on hard non-timeout write failure")
	}
}

func TestAfterMutation_TimeoutReconcilesWithoutSecondWrite(t *testing.T) {
	// Reader shows the write landed.
	r := &stubReader{task: &Task{ID: "t1", Status: StatusInProgress}}
	w := &countingWriter{reader: r}
	d := Deadlines{Mutate: 40 * time.Millisecond, Readback: 2 * time.Second}

	ctx := context.Background()
	writeCtx, cancel := WithOpDeadline(ctx, d, OpMutate)
	writeErr := w.UpdateStatus(writeCtx, "t1", StatusInProgress)
	cancel()
	if !IsTimeout(writeErr) {
		t.Fatalf("writeErr=%v", writeErr)
	}
	if w.writes.Load() != 1 {
		t.Fatalf("writes=%d", w.writes.Load())
	}

	err := AfterMutation(ctx, r, d, "kaneo", "UpdateStatus", "t1", StatusInProgress, writeErr)
	if err != nil {
		t.Fatalf("reconcile after timeout: %v", err)
	}
	// Critical: still exactly one write — no double-apply.
	if w.writes.Load() != 1 {
		t.Fatalf("double-apply: writes=%d want 1", w.writes.Load())
	}
}

func TestAfterMutation_TimeoutNotLandedNoDoubleApply(t *testing.T) {
	r := &stubReader{task: &Task{ID: "t1", Status: StatusToDo}}
	w := &countingWriter{}
	d := Deadlines{Mutate: 40 * time.Millisecond}

	ctx := context.Background()
	writeCtx, cancel := WithOpDeadline(ctx, d, OpMutate)
	writeErr := w.UpdateStatus(writeCtx, "t1", StatusInProgress)
	cancel()

	err := AfterMutation(ctx, r, d, "kaneo", "UpdateStatus", "t1", StatusInProgress, writeErr)
	if !IsAmbiguous(err) {
		t.Fatalf("want ambiguous (write lost), got %v", err)
	}
	if w.writes.Load() != 1 {
		t.Fatalf("must not re-apply on lost write: writes=%d", w.writes.Load())
	}
}

func TestAmbiguousMutationError_ErrorString(t *testing.T) {
	e := &AmbiguousMutationError{
		Provider: "kaneo",
		Op:       "UpdateStatus",
		TaskID:   "t1",
		Want:     "done",
		Actual:   "to-do",
		WriteErr: context.DeadlineExceeded,
	}
	msg := e.Error()
	for _, sub := range []string{"kaneo", "UpdateStatus", "t1", "done", "to-do"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("Error()=%q missing %q", msg, sub)
		}
	}
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("Unwrap write err")
	}
}
