package provider

import (
	"context"
	"testing"
	"time"
)

func TestApplyDeadlines_Kaneo(t *testing.T) {
	k := NewKaneoProvider("http://x", "p", false)
	ApplyDeadlines(k, Deadlines{Get: 3 * time.Second, List: 4 * time.Second})
	if k.Deadlines.Get != 3*time.Second || k.Deadlines.List != 4*time.Second {
		t.Fatalf("deadlines not applied: %+v", k.Deadlines)
	}
}

func TestBoundOp_Fires(t *testing.T) {
	ctx, cancel := BoundOp(context.Background(), Deadlines{Get: 20 * time.Millisecond}, OpGet)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Bound op deadline did not fire")
	}
}

func TestDeadlinesFromParts_Normalize(t *testing.T) {
	d := DeadlinesFromParts(0, 0, 5*time.Second, 0, 0)
	if d.Get != DefaultGetDeadline || d.Mutate != 5*time.Second {
		t.Fatalf("%+v", d)
	}
}
