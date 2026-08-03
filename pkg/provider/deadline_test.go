package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultDeadlines_AllPositive(t *testing.T) {
	d := DefaultDeadlines()
	for _, op := range []OpKind{OpGet, OpList, OpMutate, OpComment, OpReadback} {
		if d.For(op) <= 0 {
			t.Errorf("default deadline for %s must be positive, got %v", op, d.For(op))
		}
	}
	if d.Max() < d.List {
		t.Fatalf("Max()=%v should be at least List=%v", d.Max(), d.List)
	}
}

func TestDeadlines_NormalizeZeroAndNegative(t *testing.T) {
	d := Deadlines{Get: -1, List: 0, Mutate: 5 * time.Second}.Normalize()
	if d.Get != DefaultGetDeadline {
		t.Errorf("Get=%v want default", d.Get)
	}
	if d.List != DefaultListDeadline {
		t.Errorf("List=%v want default", d.List)
	}
	if d.Mutate != 5*time.Second {
		t.Errorf("Mutate=%v want 5s override", d.Mutate)
	}
}

func TestWithOpDeadline_Fires(t *testing.T) {
	d := Deadlines{Get: 30 * time.Millisecond}
	ctx, cancel := WithOpDeadline(context.Background(), d, OpGet)
	defer cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("ctx.Err()=%v want DeadlineExceeded", ctx.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("op deadline did not fire within 2s — bound is broken")
	}
}

func TestWithOpDeadline_ParentNearerWins(t *testing.T) {
	parent, pcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer pcancel()
	d := Deadlines{Get: 5 * time.Second} // far longer than parent
	ctx, cancel := WithOpDeadline(parent, d, OpGet)
	defer cancel()

	start := time.Now()
	<-ctx.Done()
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("parent deadline ignored: waited %v (want ~20ms)", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatal("expected canceled/deadline context")
	}
}

func TestWithOpDeadline_NilContextUsesBackground(t *testing.T) {
	d := Deadlines{Get: 20 * time.Millisecond}
	ctx, cancel := WithOpDeadline(nil, d, OpGet)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("nil parent should still bind via Background")
	}
}

func TestTimeoutError_IsTimeoutAndUnwrap(t *testing.T) {
	te := &TimeoutError{
		Provider: "kaneo",
		Op:       "GetTask",
		Kind:     OpGet,
		Deadline: 15 * time.Second,
		Cause:    context.DeadlineExceeded,
	}
	if !IsTimeout(te) {
		t.Fatal("IsTimeout(TimeoutError) = false")
	}
	if !te.Timeout() {
		t.Fatal("Timeout() = false")
	}
	if !errors.Is(te, context.DeadlineExceeded) {
		t.Fatal("Unwrap must surface DeadlineExceeded")
	}
	msg := te.Error()
	if !strings.Contains(msg, "kaneo") || !strings.Contains(msg, "GetTask") {
		t.Fatalf("Error()=%q missing provider/op", msg)
	}
}

func TestIsTimeout_ContextCanceled(t *testing.T) {
	if !IsTimeout(context.Canceled) {
		t.Fatal("IsTimeout(Canceled) = false")
	}
	if IsTimeout(errors.New("plain")) {
		t.Fatal("IsTimeout(plain) = true")
	}
	if IsTimeout(nil) {
		t.Fatal("IsTimeout(nil) = true")
	}
}

func TestAsTimeout_WrapsDeadline(t *testing.T) {
	err := AsTimeout("github", "ListTasks", OpList, DefaultListDeadline, context.DeadlineExceeded)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("want *TimeoutError, got %T %v", err, err)
	}
	if te.Provider != "github" || te.Op != "ListTasks" || te.Kind != OpList {
		t.Fatalf("fields: %+v", te)
	}
	// Non-timeout passes through.
	plain := errors.New("boom")
	if got := AsTimeout("x", "y", OpGet, time.Second, plain); !errors.Is(got, plain) {
		t.Fatalf("plain error mutated: %v", got)
	}
	if AsTimeout("x", "y", OpGet, time.Second, nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestAsTimeout_PreservesExisting(t *testing.T) {
	orig := &TimeoutError{Provider: "kaneo", Op: "GetTask", Kind: OpGet, Cause: context.Canceled}
	got := AsTimeout("github", "ListTasks", OpList, time.Second, orig)
	var te *TimeoutError
	if !errors.As(got, &te) {
		t.Fatal("expected TimeoutError")
	}
	if te.Provider != "kaneo" || te.Op != "GetTask" {
		t.Fatalf("existing fields overwritten: %+v", te)
	}
}

func TestClassifyContextErr(t *testing.T) {
	if ClassifyContextErr(context.Background(), "k", "op", OpGet, time.Second) != nil {
		t.Fatal("live context must classify as nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ClassifyContextErr(ctx, "kaneo", "GetTask", OpGet, DefaultGetDeadline)
	if !IsTimeout(err) {
		t.Fatalf("canceled ctx: %v", err)
	}
}

// Mutation non-vacuity: if deadline were infinite / Background, the fire test
// would hang past the 2s guard. This documents the class.
func TestWithOpDeadline_MutationBackgroundWouldNotFire(t *testing.T) {
	// Control: Background alone never fires.
	ctx := context.Background()
	select {
	case <-ctx.Done():
		t.Fatal("Background must not be done")
	default:
	}
	// Bound: short deadline does fire (proved above). If someone replaces
	// WithOpDeadline with identity/Background, TestWithOpDeadline_Fires fails.
}
