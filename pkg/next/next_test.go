package next

import (
	"context"
	"os"
	"testing"
)

func TestEval_NoTasks(t *testing.T) {
	cfg := testConfig()
	tp := newTestProvider([]testTask{})
	p := NewNextPicker(cfg, tp)
	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionClaim {
		t.Errorf("expected ActionClaim for empty board, got %s", act.Type)
	}
}

func TestEval_VerdictArtifacts(t *testing.T) {
	cfg := testConfig()
	tp := newTestProvider([]testTask{})
	tmp, _ := os.MkdirTemp("", "next-test-*")
	defer os.RemoveAll(tmp)
	p := NewNextPicker(cfg, tp)
	p.InboxDir = tmp

	os.WriteFile(tmp+"/test-verdict.md", []byte("verdict"), 0644)

	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionIngest {
		t.Errorf("expected ActionIngest with verdict present, got %s", act.Type)
	}
}

func TestEval_ReviewAtCap(t *testing.T) {
	cfg := testConfig()
	// 3 tasks in review hits the cap of 3
	tp := newTestProvider([]testTask{
		{ref: "FAC-1", status: "review", priority: "high"},
		{ref: "FAC-2", status: "review", priority: "medium"},
		{ref: "FAC-3", status: "review", priority: "medium"},
	})
	p := NewNextPicker(cfg, tp)
	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionReview {
		t.Errorf("expected ActionReview with 3 tasks in review (cap: 3), got %s", act.Type)
	}
}

func TestEval_NeedReview(t *testing.T) {
	cfg := testConfig()
	tp := newTestProvider([]testTask{
		{ref: "FAC-1", status: "in-progress", priority: "high"},
	})
	p := NewNextPicker(cfg, tp)
	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionClaim {
		t.Errorf("expected ActionClaim when in-progress task has no candidate SHA, got %s", act.Type)
	}
}

func TestSelftest(t *testing.T) {
	t.Skip("requires herd binary in PATH")
	cfg := testConfig()
	tp := newTestProvider([]testTask{})
	p := NewNextPicker(cfg, tp)
	if err := p.Selftest(context.Background()); err != nil {
		t.Fatal(err)
	}
}
