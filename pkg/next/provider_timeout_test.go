package next

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestEval_ProviderTimeout_NotFreeCapacity(t *testing.T) {
	cfg := testConfig()
	inner := &hangListProvider{}
	tp := provider.NewBoundClient(inner, provider.Deadlines{List: 30 * time.Millisecond})
	p := NewNextPicker(cfg, tp)
	act, err := p.Eval(context.Background())
	if err == nil {
		t.Fatalf("timeout must not return success action: %+v", act)
	}
	if act != nil {
		t.Fatalf("action must be nil on error: %+v", act)
	}
	if !strings.Contains(err.Error(), "BLOCKED(provider_timeout)") {
		t.Fatalf("want BLOCKED projection: %v", err)
	}
	// Mutation: returning ActionClaim on timeout would free-capacity advance.
	if provider.ClassifyOpError(err) != provider.OpTimeout {
		t.Fatalf("class=%q", provider.ClassifyOpError(err))
	}
}

func TestEvalAll_ProviderTimeout_EmptySliceForbidden(t *testing.T) {
	cfg := testConfig()
	tp := provider.NewBoundClient(&hangListProvider{}, provider.Deadlines{List: 30 * time.Millisecond})
	p := NewNextPicker(cfg, tp)
	actions, err := p.EvalAll(context.Background())
	if err == nil {
		t.Fatalf("want error, got actions=%v", actions)
	}
	if actions != nil {
		t.Fatal("must not return claim-next actions on timeout")
	}
}

type hangListProvider struct{}

func (h *hangListProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	return nil, context.Canceled
}
func (h *hangListProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (h *hangListProvider) ClaimTask(context.Context, string, string) error { return nil }
func (h *hangListProvider) UpdateStatus(context.Context, string, string) error {
	return nil
}
func (h *hangListProvider) AddComment(context.Context, string, string) error { return nil }
