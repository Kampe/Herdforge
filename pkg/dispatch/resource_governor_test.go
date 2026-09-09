package dispatch

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type testResourcePermit struct {
	closed bool
	err    error
}

func (p *testResourcePermit) Close() error { p.closed = true; return p.err }

type testDispatchGovernor struct {
	permit *testResourcePermit
	err    error
}

func (g testDispatchGovernor) AcquireDispatch(context.Context) (io.Closer, resources.GovernorReport, error) {
	return g.permit, resources.GovernorReport{HostID: "host-a"}, g.err
}

type resourceCheckedWorktree struct {
	permit *testResourcePermit
	calls  int
	err    error
}

func (w *resourceCheckedWorktree) CreateTaskWorktreeFrom(context.Context, string, string) (*worktree.WorktreeInfo, error) {
	w.calls++
	if w.permit != nil && w.permit.closed {
		return nil, errors.New("resource permit released before worktree creation")
	}
	if w.err != nil {
		return nil, w.err
	}
	return &worktree.WorktreeInfo{Path: "task", Branch: "herd/task"}, nil
}

func (w *resourceCheckedWorktree) RepoRoot() string { return "repo" }

func TestResourcePermitHoldsAcrossWorktreeMutation(t *testing.T) {
	permit := &testResourcePermit{}
	wt := &resourceCheckedWorktree{permit: permit}
	d := &Dispatcher{Worktree: wt, Resources: testDispatchGovernor{permit: permit}}
	if _, err := d.withResourcePermit(context.Background(), func() (*worktree.WorktreeInfo, error) {
		return wt.CreateTaskWorktreeFrom(context.Background(), "FAC-1", "main")
	}); err != nil {
		t.Fatal(err)
	}
	if wt.calls != 1 || !permit.closed {
		t.Fatalf("calls=%d permit.closed=%t", wt.calls, permit.closed)
	}
}

func TestResourcePermitFailsBeforeWorktreeMutation(t *testing.T) {
	wt := &resourceCheckedWorktree{}
	d := &Dispatcher{Worktree: wt, Resources: testDispatchGovernor{err: errors.New("pressure")}}
	if _, err := d.withResourcePermit(context.Background(), func() (*worktree.WorktreeInfo, error) {
		return wt.CreateTaskWorktreeFrom(context.Background(), "FAC-1", "main")
	}); err == nil {
		t.Fatal("resource refusal reached no error")
	}
	if wt.calls != 0 {
		t.Fatalf("resource refusal crossed worktree mutation boundary: calls=%d", wt.calls)
	}
}

func TestResourcePermitPropagatesReleaseFailure(t *testing.T) {
	permit := &testResourcePermit{err: errors.New("unlock failed")}
	wt := &resourceCheckedWorktree{permit: permit}
	d := &Dispatcher{Worktree: wt, Resources: testDispatchGovernor{permit: permit}}
	if _, err := d.withResourcePermit(context.Background(), func() (*worktree.WorktreeInfo, error) {
		return wt.CreateTaskWorktreeFrom(context.Background(), "FAC-1", "main")
	}); err == nil || !errors.Is(err, permit.err) {
		t.Fatalf("release error=%v", err)
	}
}
