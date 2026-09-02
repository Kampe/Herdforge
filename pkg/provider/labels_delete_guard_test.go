package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// kaneoLabelWorld is a Kaneo-shaped recording fake. It holds real label state
// so every readback in the provider is answered from that state rather than
// from a canned reply, and it records the argv of every call so a test can
// assert on what was NOT invoked. No live board is touched.
type kaneoLabelWorld struct {
	rows      map[string]TaskLabel
	calls     []string
	semantics string // "move" or "clone", the two observed task label add behaviors
	nextID    int
}

func newKaneoLabelWorld(semantics string) *kaneoLabelWorld {
	return &kaneoLabelWorld{rows: map[string]TaskLabel{}, semantics: semantics}
}

func (w *kaneoLabelWorld) seed(id, name, taskID string) {
	w.rows[id] = TaskLabel{ID: id, Name: name, TaskID: taskID}
}

func (w *kaneoLabelWorld) install(t *testing.T) *KaneoProvider {
	t.Helper()
	previous := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = previous })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		joined := strings.Join(args, " ")
		w.calls = append(w.calls, joined)
		switch {
		case strings.HasPrefix(joined, "label delete"):
			// The operation under investigation. Reaching it is the defect.
			return nil, fmt.Errorf("fake: name-wide workspace label delete invoked")
		case strings.HasPrefix(joined, "label list"):
			return &CLIResult{Stdout: w.encodeRows(w.all())}, nil
		case strings.HasPrefix(joined, "task label list"):
			return &CLIResult{Stdout: w.encodeRows(w.ownedBy(args[3]))}, nil
		case strings.HasPrefix(joined, "task label add"):
			return w.add(args[3], args[4])
		case strings.HasPrefix(joined, "task label delete"):
			return w.detach(args[3])
		case strings.HasPrefix(joined, "task get"):
			return w.getTask(args[2])
		}
		return nil, fmt.Errorf("fake: unexpected argv %q", joined)
	}
	return NewKaneoProvider("", "project-1", true)
}

func (w *kaneoLabelWorld) all() []TaskLabel {
	out := make([]TaskLabel, 0, len(w.rows))
	for _, row := range w.rows {
		out = append(out, row)
	}
	return out
}

func (w *kaneoLabelWorld) ownedBy(taskID string) []TaskLabel {
	out := make([]TaskLabel, 0, len(w.rows))
	for _, row := range w.rows {
		if row.TaskID == taskID {
			out = append(out, row)
		}
	}
	return out
}

func (w *kaneoLabelWorld) encodeRows(rows []TaskLabel) []byte {
	body, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return body
}

// add applies whichever semantic this world was built with. Both report the
// same success shape, which is why the provider cannot trust the response.
func (w *kaneoLabelWorld) add(taskID, labelID string) (*CLIResult, error) {
	row, ok := w.rows[labelID]
	if !ok {
		return nil, fmt.Errorf("fake: unknown label %q", labelID)
	}
	switch w.semantics {
	case "move":
		row.TaskID = taskID
		w.rows[labelID] = row
	case "clone":
		w.nextID++
		clone := TaskLabel{ID: fmt.Sprintf("%s-clone-%d", labelID, w.nextID), Name: row.Name, TaskID: taskID}
		w.rows[clone.ID] = clone
	default:
		return nil, fmt.Errorf("fake: unset attach semantics")
	}
	body, err := json.Marshal(TaskLabel{ID: labelID, Name: row.Name, TaskID: taskID})
	if err != nil {
		return nil, err
	}
	return &CLIResult{Stdout: body}, nil
}

func (w *kaneoLabelWorld) detach(labelID string) (*CLIResult, error) {
	row, ok := w.rows[labelID]
	if !ok {
		return nil, fmt.Errorf("fake: unknown label %q", labelID)
	}
	row.TaskID = ""
	w.rows[labelID] = row
	body, err := json.Marshal([]TaskLabel{{ID: labelID, Name: row.Name}})
	if err != nil {
		return nil, err
	}
	return &CLIResult{Stdout: body}, nil
}

func (w *kaneoLabelWorld) getTask(taskID string) (*CLIResult, error) {
	labels := make([]kaneoLabel, 0)
	for _, row := range w.ownedBy(taskID) {
		labels = append(labels, kaneoLabel{Name: row.Name})
	}
	body, err := json.Marshal(kaneoTaskDTO{ID: taskID, Title: "fake", ProjectId: "project-1", Labels: labels})
	if err != nil {
		return nil, err
	}
	return &CLIResult{Stdout: body}, nil
}

func (w *kaneoLabelWorld) issued(prefix string) bool {
	for _, call := range w.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

// TestCompensationNeverIssuesWorkspaceLabelDeleteArgv is the control the
// 2026-08-28 incident lacked. Both compensation functions run against a
// recording fake, and neither may reach `label delete`.
//
// Mutation-proved: restoring `p.DeleteTaskLabel(ctx, created.ID)` after the
// detach in either function turns this test red, because DeleteTaskLabel is
// reached before any read that could bail out first.
func TestCompensationNeverIssuesWorkspaceLabelDeleteArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *KaneoProvider, TaskLabel) error
	}{
		{"compensateTargetLabel", func(ctx context.Context, k *KaneoProvider, created TaskLabel) error {
			original := []TaskLabel{{ID: "kept", Name: "unrelated", TaskID: "target"}}
			return compensateTargetLabel(ctx, k, "target", original, &Task{ID: "target"}, created, errors.New("cause"))
		}},
		{"compensateLabel", func(ctx context.Context, k *KaneoProvider, created TaskLabel) error {
			source := []TaskLabel{{ID: "source-role", Name: "herd-smith", TaskID: "source"}}
			original := []TaskLabel{{ID: "kept", Name: "unrelated", TaskID: "target"}}
			return compensateLabel(ctx, k, "source", "target", source, original, &Task{ID: "source"}, &Task{ID: "target"}, created, nil, errors.New("cause"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := newKaneoLabelWorld("move")
			world.seed("kept", "unrelated", "target")
			world.seed("source-role", "herd-smith", "source")
			world.seed("fresh", "herd-smith", "target")
			k := world.install(t)

			// The compensation outcome is not the assertion; the argv is. A
			// readback mismatch is an acceptable rollback verdict, a name-wide
			// delete never is.
			_ = tc.run(context.Background(), k, TaskLabel{ID: "fresh", Name: "herd-smith"})

			if world.issued("label delete") {
				t.Fatalf("compensation issued a name-wide workspace delete: %v", world.calls)
			}
			if !world.issued("task label delete fresh") {
				t.Fatalf("compensation did not detach the row it created: %v", world.calls)
			}
			if _, present := world.rows["fresh"]; !present {
				t.Fatal("compensation destroyed the row instead of detaching it")
			}
			if row := world.rows["fresh"]; row.TaskID != "" {
				t.Fatalf("created row still attached after compensation: %+v", row)
			}
		})
	}
}

// TestDeleteTaskLabelFailsClosedWithoutArgv pins that the primitive itself is
// unreachable, not merely unused by today's callers.
func TestDeleteTaskLabelFailsClosedWithoutArgv(t *testing.T) {
	world := newKaneoLabelWorld("move")
	world.seed("l1", "bounded", "")
	k := world.install(t)

	err := k.DeleteTaskLabel(context.Background(), "l1")
	if !errors.Is(err, ErrWorkspaceLabelDeleteUnguarded) {
		t.Fatalf("workspace label delete must fail closed, got %v", err)
	}
	if len(world.calls) != 0 {
		t.Fatalf("failed-closed delete still shelled out: %v", world.calls)
	}
	if _, present := world.rows["l1"]; !present {
		t.Fatal("row removed despite the refusal")
	}
}

// TestAttachTaskLabelProvesTargetAndDonorPriorHolder covers the reason the
// readback exists: the same command has been observed moving a row off its
// holder and cloning it. The caller states neither expectation, so a move that
// strips a third task must fail closed while a clone must succeed.
func TestAttachTaskLabelProvesTargetAndDonorPriorHolder(t *testing.T) {
	t.Run("move off a third task fails closed", func(t *testing.T) {
		world := newKaneoLabelWorld("move")
		world.seed("donor-row", "herd-smith", "donor-task")
		k := world.install(t)

		err := k.AttachTaskLabel(context.Background(), "target", "donor-row")
		if err == nil {
			t.Fatal("a move that stripped the donor task must fail closed")
		}
		if !strings.Contains(err.Error(), "donor-task") || !strings.Contains(err.Error(), "lost its") {
			t.Fatalf("refusal must name the donor that lost its row, got %v", err)
		}
	})

	t.Run("clone leaves the donor intact and succeeds", func(t *testing.T) {
		world := newKaneoLabelWorld("clone")
		world.seed("donor-row", "herd-smith", "donor-task")
		k := world.install(t)

		if err := k.AttachTaskLabel(context.Background(), "target", "donor-row"); err != nil {
			t.Fatalf("clone semantics must satisfy the readback: %v", err)
		}
		if row := world.rows["donor-row"]; row.TaskID != "donor-task" {
			t.Fatalf("donor lost its row under clone semantics: %+v", row)
		}
	})

	t.Run("unattached donor attaches to the target", func(t *testing.T) {
		world := newKaneoLabelWorld("move")
		world.seed("fresh", "herd-smith", "")
		k := world.install(t)

		if err := k.AttachTaskLabel(context.Background(), "target", "fresh"); err != nil {
			t.Fatalf("unattached donor must attach cleanly: %v", err)
		}
		if row := world.rows["fresh"]; row.TaskID != "target" {
			t.Fatalf("target does not hold the row: %+v", row)
		}
	})

	t.Run("donor absent from the workspace fails before the add", func(t *testing.T) {
		world := newKaneoLabelWorld("move")
		k := world.install(t)

		if err := k.AttachTaskLabel(context.Background(), "target", "ghost"); err == nil {
			t.Fatal("an unreadable donor preimage must fail closed")
		}
		if world.issued("task label add") {
			t.Fatalf("attach mutated the board without a donor preimage: %v", world.calls)
		}
	})
}

// idempotentCreateProvider models the observed `label create` behavior that
// returns a pre-existing row instead of creating one. Here the returned row is
// another card's live label, which is the case that turns "create fresh, then
// attach" into a silent unlabelling of that card.
type idempotentCreateProvider struct {
	*MemoryProvider
	donorID string
}

func (p *idempotentCreateProvider) CreateTaskLabel(_ context.Context, _, _ string) (TaskLabel, error) {
	row, _, _ := p.MemoryProvider.LookupWorkspaceLabel(context.Background(), p.donorID)
	// The CLI reports the row as unattached even when it is not.
	return TaskLabel{ID: row.ID, Name: row.Name}, nil
}

// ProveLabelCreation passes deliberately. Each provider implements its own
// creation proof, so the transaction cannot rely on one having caught this;
// keeping the proof permissive here puts the labels.go donor guard under test
// rather than the fake's proof.
func (p *idempotentCreateProvider) ProveLabelCreation(context.Context, TaskLabel, string, string, LabelRepairOptions) error {
	return nil
}

// TestLabelTransactionRefusesLiveDonorRow proves the donor proof in labels.go:
// a create that hands back somebody else's live row is refused before any
// attach, and the other card keeps its label.
func TestLabelTransactionRefusesLiveDonorRow(t *testing.T) {
	memory := NewMemoryProvider()
	memory.AddTask(&Task{ID: "other", ProjectID: "p", Status: "to-do", Labels: []string{"herd-smith"}})
	memory.AddTask(&Task{ID: "target", ProjectID: "p", Status: "to-do"})
	memory.labels["live-row"] = TaskLabel{ID: "live-row", Name: "herd-smith", TaskID: "other"}
	p := &idempotentCreateProvider{MemoryProvider: memory, donorID: "live-row"}

	err := EnsureTaskRoleLabel(context.Background(), p, "target", "herd-smith")
	if err == nil {
		t.Fatal("adopting another card's live row must fail closed")
	}
	if !errors.Is(err, ErrLabelOwnershipUnknown) {
		t.Fatalf("refusal must report unknown ownership, got %v", err)
	}
	if row := memory.labels["live-row"]; row.TaskID != "other" {
		t.Fatalf("donor card lost its label: %+v", row)
	}
	for _, id := range memory.attachIDs {
		if id == "live-row" {
			t.Fatal("live row was passed to attach despite the refusal")
		}
	}
}
