package provider

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type hangInner struct {
	lists atomic.Int32
}

func (h *hangInner) GetTask(ctx context.Context, id string) (*Task, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (h *hangInner) ListTasks(ctx context.Context, projectID, status string) ([]*Task, error) {
	h.lists.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (h *hangInner) ClaimTask(ctx context.Context, taskID, role string) error {
	<-ctx.Done()
	return ctx.Err()
}
func (h *hangInner) UpdateStatus(ctx context.Context, taskID, status string) error {
	<-ctx.Done()
	return ctx.Err()
}
func (h *hangInner) AddComment(ctx context.Context, taskID, body string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestBoundClient_ListTimeout_NeverEmptySuccess(t *testing.T) {
	inner := &hangInner{}
	bc := NewBoundClient(inner, Deadlines{List: 40 * time.Millisecond})
	tasks, err := bc.ListTasks(context.Background(), "p", "")
	if err == nil {
		t.Fatal("timeout must not return nil error")
	}
	if tasks != nil {
		t.Fatalf("timeout must not return tasks: %v", tasks)
	}
	if !strings.Contains(err.Error(), "BLOCKED(provider_timeout)") {
		t.Fatalf("want BLOCKED label: %v", err)
	}
	for _, want := range []string{
		`"provider":"task-provider"`,
		`"phase":"ListTasks"`,
		`"applied_deadline":"40ms"`,
		`"last_successful_cache_revision":"none"`,
		`"outcome":"timed-out"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("structured diagnostics omit %q: %v", want, err)
		}
	}
	if ClassifyOpError(err) != OpTimeout {
		t.Fatalf("class=%q", ClassifyOpError(err))
	}
	if inner.lists.Load() != 1 {
		t.Fatalf("lists=%d", inner.lists.Load())
	}
}

func TestBoundClient_ReadFailureIsStructuredUnknown(t *testing.T) {
	bc := NewBoundClient(&staticInner{}, Deadlines{Get: 2 * time.Second})
	_, err := bc.GetTask(context.Background(), "FAC-607")
	if err == nil {
		t.Fatal("provider failure returned success")
	}
	for _, want := range []string{
		"UNKNOWN, not an empty or clean result",
		`"provider":"task-provider"`,
		`"phase":"GetTask"`,
		`"applied_deadline":"2s"`,
		`"last_successful_cache_revision":"none"`,
		`"outcome":"failed"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("structured diagnostics omit %q: %v", want, err)
		}
	}
}

func TestBoundClient_MutateAmbiguousLabeled(t *testing.T) {
	inner := &staticInner{updateErr: &AmbiguousMutationError{
		Provider: "kaneo", Op: "UpdateStatus",
		WriteErr: &TimeoutError{Cause: context.DeadlineExceeded},
	}}
	bc := NewBoundClient(inner, DefaultDeadlines())
	err := bc.UpdateStatus(context.Background(), "t", "done")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsAmbiguous(err) || !strings.Contains(err.Error(), "BLOCKED") {
		t.Fatalf("want ambiguous BLOCKED: %v", err)
	}
	if ClassifyOpError(err) != OpAmbiguous {
		t.Fatalf("class=%q", ClassifyOpError(err))
	}
}

type staticInner struct {
	updateErr error
	tasks     []*Task
}

func (s *staticInner) GetTask(context.Context, string) (*Task, error) { return nil, errors.New("x") }
func (s *staticInner) ListTasks(context.Context, string, string) ([]*Task, error) {
	return s.tasks, nil
}
func (s *staticInner) ClaimTask(context.Context, string, string) error { return nil }
func (s *staticInner) UpdateStatus(context.Context, string, string) error {
	return s.updateErr
}
func (s *staticInner) AddComment(context.Context, string, string) error { return nil }

func TestNewProductionProvider_ActivatesOnlyConfiguredProviders(t *testing.T) {
	_, err := NewProductionProvider(TaskConfig{Type: "github"})
	if err == nil {
		t.Fatal("unsupported provider must refuse activation")
	}
	for _, tc := range []TaskConfig{
		{Type: "kaneo", ProjectID: "p", List: time.Second},
		{Type: "linear", APIKey: "linear-test-key", ProjectID: "linear-project", List: time.Second},
	} {
		tp, err := NewProductionProvider(tc)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := tp.(*BoundClient); !ok {
			t.Fatalf("want BoundClient, got %T", tp)
		}
	}
	if _, err := NewProductionProvider(TaskConfig{Type: "linear"}); err == nil {
		t.Fatal("linear without an explicit credential must fail")
	}
	if _, err := NewProductionProvider(TaskConfig{Type: "linear", APIKey: "linear-test-key", ProjectID: " \t "}); err == nil {
		t.Fatal("linear without a non-blank project ID must fail")
	}
}
