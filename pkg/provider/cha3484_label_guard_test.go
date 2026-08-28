package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingDeleteLabels struct {
	*MemoryProvider
	deleted []string
}

func (r *recordingDeleteLabels) DeleteTaskLabel(ctx context.Context, id string) error {
	r.deleted = append(r.deleted, id)
	return r.MemoryProvider.DeleteTaskLabel(ctx, id)
}

func TestCompensateTargetLabel_NeverDeletesWorkspaceLabel(t *testing.T) {
	p := NewMemoryProvider()
	p.AddTask(&Task{ID: "target", ProjectID: "project"})
	task, err := p.GetTask(context.Background(), "target")
	if err != nil {
		t.Fatal(err)
	}
	created, err := p.CreateTaskLabel(context.Background(), "target", "forge-smith")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AttachTaskLabel(context.Background(), "target", created.ID); err != nil {
		t.Fatal(err)
	}
	rec := &recordingDeleteLabels{MemoryProvider: p}
	_ = compensateTargetLabel(context.Background(), rec, "target", nil, task, created, errors.New("cause"))
	if len(rec.deleted) != 0 {
		t.Fatalf("compensateTargetLabel issued DeleteTaskLabel %v", rec.deleted)
	}
}

func TestCompensateLabel_NeverDeletesWorkspaceLabel(t *testing.T) {
	p := NewMemoryProvider()
	p.AddTask(&Task{ID: "source", ProjectID: "project"})
	p.AddTask(&Task{ID: "target", ProjectID: "project"})
	src, err := p.GetTask(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := p.GetTask(context.Background(), "target")
	if err != nil {
		t.Fatal(err)
	}
	created, err := p.CreateTaskLabel(context.Background(), "target", "forge-smith")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AttachTaskLabel(context.Background(), "target", created.ID); err != nil {
		t.Fatal(err)
	}
	rec := &recordingDeleteLabels{MemoryProvider: p}
	_ = compensateLabel(context.Background(), rec, "source", "target", nil, nil, src, tgt, created, nil, errors.New("cause"))
	if len(rec.deleted) != 0 {
		t.Fatalf("compensateLabel issued DeleteTaskLabel %v", rec.deleted)
	}
}

func argvHasWorkspaceLabelDelete(args []string) bool {
	for i, arg := range args {
		if arg != "label" || i+1 >= len(args) || args[i+1] != "delete" {
			continue
		}
		if i == 0 || args[i-1] != "task" {
			return true
		}
	}
	return false
}

func TestKaneoCLI_DeleteTaskLabelNeverIssuesLabelDelete(t *testing.T) {
	var calls [][]string
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		copied := append([]string(nil), args...)
		calls = append(calls, copied)
		return &CLIResult{Stdout: []byte(`{"id":"l2"}`)}, nil
	}
	k := NewKaneoProvider("", "project-1", true)
	err := k.DeleteTaskLabel(context.Background(), "l2")
	if err == nil {
		t.Fatal("DeleteTaskLabel must fail closed")
	}
	if !errors.Is(err, ErrWorkspaceLabelDeleteRefused) {
		t.Fatalf("want ErrWorkspaceLabelDeleteRefused, got %v", err)
	}
	for _, args := range calls {
		if argvHasWorkspaceLabelDelete(args) {
			t.Fatalf("DeleteTaskLabel issued workspace label delete argv %v", args)
		}
	}
}

func TestKaneoCLI_AttachTaskLabelFailsClosedWhenDonorLostRow(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(command, "label list"):
			return &CLIResult{Stdout: []byte(`[{"id":"l1","name":"bounded","taskId":"donor"}]`)}, nil
		case strings.HasPrefix(command, "task label add"):
			return &CLIResult{Stdout: []byte(`{"id":"l1","taskId":"target"}`)}, nil
		case strings.HasPrefix(command, "task label list target"):
			return &CLIResult{Stdout: []byte(`[{"id":"l1","name":"bounded","taskId":"target"}]`)}, nil
		case strings.HasPrefix(command, "task label list donor"):
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		default:
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
	}
	k := NewKaneoProvider("", "project-1", true)
	err := k.AttachTaskLabel(context.Background(), "target", "l1")
	if err == nil {
		t.Fatal("attach that steals the donor row must fail closed")
	}
}

func TestKaneoCLI_AttachTaskLabelKeepsDonorAndTarget(t *testing.T) {
	var sawTarget, sawDonor bool
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(command, "label list"):
			return &CLIResult{Stdout: []byte(`[{"id":"l1","name":"bounded","taskId":"donor"}]`)}, nil
		case strings.HasPrefix(command, "task label add"):
			return &CLIResult{Stdout: []byte(`{"id":"l1","taskId":"target"}`)}, nil
		case strings.HasPrefix(command, "task label list target"):
			sawTarget = true
			return &CLIResult{Stdout: []byte(`[{"id":"l1","name":"bounded","taskId":"target"}]`)}, nil
		case strings.HasPrefix(command, "task label list donor"):
			sawDonor = true
			return &CLIResult{Stdout: []byte(`[{"id":"l1","name":"bounded","taskId":"donor"}]`)}, nil
		default:
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
	}
	k := NewKaneoProvider("", "project-1", true)
	if err := k.AttachTaskLabel(context.Background(), "target", "l1"); err != nil {
		t.Fatalf("clone-shaped attach must succeed: %v", err)
	}
	if !sawTarget || !sawDonor {
		t.Fatalf("attach must read back target and donor, target=%v donor=%v", sawTarget, sawDonor)
	}
}

func TestKaneoCLI_CreateTaskLabelRejectsAttachedRow(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		command := strings.Join(args, " ")
		if strings.HasPrefix(command, "label list") {
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		if strings.HasPrefix(command, "label create") {
			return &CLIResult{Stdout: []byte(`{"id":"old-row","name":"herd-smith","taskId":"other"}`)}, nil
		}
		return &CLIResult{Stdout: []byte(`[]`)}, nil
	}
	k := NewKaneoProvider("", "project-1", true)
	row, err := k.CreateTaskLabel(context.Background(), "target", "herd-smith")
	if err == nil {
		t.Fatalf("create must refuse an attached row, got %+v", row)
	}
}

type attachedCreateProvider struct {
	*MemoryProvider
	attaches int
	taskID   string
}

func (a *attachedCreateProvider) CreateTaskLabel(_ context.Context, _, name string) (TaskLabel, error) {
	return TaskLabel{ID: "live-row", Name: name, TaskID: a.taskID}, nil
}

func (a *attachedCreateProvider) AttachTaskLabel(ctx context.Context, taskID, labelID string) error {
	a.attaches++
	return a.MemoryProvider.AttachTaskLabel(ctx, taskID, labelID)
}

func TestEnsureTaskRoleLabel_CreatedAttachedRowIsNotAttached(t *testing.T) {
	base := NewMemoryProvider()
	base.AddTask(&Task{ID: "target", ProjectID: "project"})
	for _, donor := range []string{"other", "target"} {
		p := &attachedCreateProvider{MemoryProvider: base, taskID: donor}
		err := EnsureTaskRoleLabel(context.Background(), p, "target", "forge-smith")
		if err == nil {
			t.Fatalf("create result attached to %s must fail closed", donor)
		}
		if p.attaches != 0 {
			t.Fatalf("attach issued for create result attached to %s", donor)
		}
	}
}
