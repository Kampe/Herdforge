package worktree

import (
	"context"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

type unheldHoldReader struct{}

func (unheldHoldReader) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Generation: 1}, nil
}
func (unheldHoldReader) CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) {
	return 1, nil
}

type heldHoldReader struct{}

func (heldHoldReader) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Held: true, Generation: 1, Reason: "maintenance", Code: "operator_hold"}, nil
}
func (heldHoldReader) CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) {
	return 1, nil
}

func reapHoldIdentities(w *WorktreeInfo) []lifecycle.HoldIdentity {
	task := strings.TrimPrefix(w.Branch, "herd/")
	return []lifecycle.HoldIdentity{
		{Repository: "repo", Owner: "owner", Lane: w.Branch, Scope: "lane"},
		{Repository: "repo", Owner: "owner", Lane: w.Branch, Task: task, Scope: "task"},
	}
}

func TestClassifyHeldIdentityRefusesBeforeGitEvidence(t *testing.T) {
	wm := NewWorktreeManager(t.TempDir())
	wt := &WorktreeInfo{Path: t.TempDir(), Branch: "herd/held", Commit: "head"}
	candidate := wm.classifyOne(context.Background(), wt, ReapPolicy{AutoReap: true, HoldReader: heldHoldReader{}, IdentitySetFor: func(w *WorktreeInfo) []lifecycle.HoldIdentity {
		return append(reapHoldIdentities(w)[:1], lifecycle.HoldIdentity{Repository: "repo", Owner: "owner", Lane: w.Branch, Task: "FAC-HELD", Scope: "task"})
	}}, "", nil)
	if candidate.Class != ReapClassUnknown || candidate.Eligible || candidate.PreserveAction == "" {
		t.Fatalf("held candidate=%+v", candidate)
	}
	if candidate.Reason != "worktree identity is held" {
		t.Fatalf("reason=%q", candidate.Reason)
	}
}
