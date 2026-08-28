package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func seedLabelTasks() *MemoryProvider {
	p := NewMemoryProvider()
	p.AddTask(&Task{ID: "fac-140", Ref: "FAC-140", ProjectID: "p", Status: "to-do", Title: "source", Description: "source fields", Priority: PriorityHigh, Labels: []string{"forge-smith", "risk:R1"}})
	p.AddTask(&Task{ID: "fac-189", Ref: "FAC-189", ProjectID: "p", Status: "to-do", Title: "target", Description: "target fields", Priority: PriorityLow, Labels: []string{"unrelated"}})
	p.labels["source-forge-smith"] = TaskLabel{ID: "source-forge-smith", Name: "forge-smith", TaskID: "fac-140"}
	p.labels["source-risk"] = TaskLabel{ID: "source-risk", Name: "risk:R1", TaskID: "fac-140"}
	p.labels["target-unrelated"] = TaskLabel{ID: "target-unrelated", Name: "unrelated", TaskID: "fac-189"}
	return p
}

func TestRepairTaskRoleLabel_SourceTheftFixture(t *testing.T) {
	p := seedLabelTasks()
	if err := RepairTaskRoleLabel(context.Background(), p, "fac-140", "fac-189", "forge-smith"); err != nil {
		t.Fatal(err)
	}
	source, _ := p.ListTaskLabels(context.Background(), "fac-140")
	target, _ := p.ListTaskLabels(context.Background(), "fac-189")
	if len(source) != 2 || !ownsLabel(source, "source-forge-smith", "fac-140") || !ownsLabel(source, "source-risk", "fac-140") {
		t.Fatalf("source label stolen: %+v", source)
	}
	if len(target) != 2 || target[0].ID == "source-forge-smith" || !ownsLabel(target, "label-1", "fac-189") {
		t.Fatalf("target label not fresh/owned: %+v", target)
	}
	for _, id := range p.attachIDs {
		if id == "source-forge-smith" {
			t.Fatal("source-owned label was passed to destructive attach")
		}
	}
	if err := RepairTaskRoleLabel(context.Background(), p, "fac-140", "fac-189", "forge-smith"); err != nil {
		t.Fatal("idempotent repair: ", err)
	}
	target2, _ := p.ListTaskLabels(context.Background(), "fac-189")
	if len(target2) != 2 {
		t.Fatalf("idempotent repair duplicated target: %+v", target2)
	}
}

func TestRepairTaskRoleLabel_ReconcilesWrongAndDuplicateRoleFamily(t *testing.T) {
	p := NewMemoryProvider()
	p.AddTask(&Task{ID: "source", ProjectID: "project"})
	p.AddTask(&Task{ID: "target", ProjectID: "project"})
	for _, item := range []struct{ task, name string }{{"source", "forge-smith"}, {"target", "worker"}, {"target", "herd-smith"}, {"target", "forge-smith"}, {"target", "risk:R1"}} {
		row, err := p.CreateTaskLabel(context.Background(), item.task, item.name)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.AttachTaskLabel(context.Background(), item.task, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	beforeSource, _ := p.ListTaskLabels(context.Background(), "source")
	if err := RepairTaskRoleLabel(context.Background(), p, "source", "target", "forge-smith"); err != nil {
		t.Fatal(err)
	}
	afterSource, _ := p.ListTaskLabels(context.Background(), "source")
	if !sameLabels(beforeSource, afterSource) {
		t.Fatalf("source changed: before=%+v after=%+v", beforeSource, afterSource)
	}
	afterTarget, _ := p.ListTaskLabels(context.Background(), "target")
	roles := 0
	for _, label := range afterTarget {
		if isRoleLabel(label.Name) {
			roles++
		}
		if label.Name == "worker" {
			t.Fatal("wrong role survived")
		}
	}
	if roles != 1 || countRole(afterTarget, "forge-smith") != 1 {
		t.Fatalf("target role family not canonical: %+v", afterTarget)
	}
}

func TestMemoryProvider_ModelsDestructiveAttachedRowMove(t *testing.T) {
	p := seedLabelTasks()
	if err := p.AttachTaskLabel(context.Background(), "fac-189", "source-forge-smith"); err != nil {
		t.Fatal(err)
	}
	source, _ := p.ListTaskLabels(context.Background(), "fac-140")
	target, _ := p.ListTaskLabels(context.Background(), "fac-189")
	if len(source) != 1 || len(target) != 2 || !ownsLabel(target, "source-forge-smith", "fac-189") {
		t.Fatalf("fake does not model destructive source theft: source=%+v target=%+v", source, target)
	}
}

func TestRepairTaskRoleLabel_UnknownOwnershipFailsBeforeMutation(t *testing.T) {
	p := seedLabelTasks()
	delete(p.labels, "source-forge-smith")
	p.labels["bad"] = TaskLabel{ID: "bad", Name: "forge-smith"} // unreadable owner
	if err := RepairTaskRoleLabel(context.Background(), p, "fac-140", "fac-189", "forge-smith"); err == nil {
		t.Fatal("cross-owned source/target state must fail closed")
	}
	labels, _ := p.ListTaskLabels(context.Background(), "fac-189")
	if len(labels) != 1 || labels[0].ID != "target-unrelated" {
		t.Fatalf("mutation occurred before ownership check: %+v", labels)
	}
}

type mismatchLabels struct {
	*MemoryProvider
	mismatch bool
	failComp bool
	reads    int
}

func (m *mismatchLabels) ListTaskLabels(ctx context.Context, id string) ([]TaskLabel, error) {
	rows, err := m.MemoryProvider.ListTaskLabels(ctx, id)
	m.reads++
	if m.mismatch && m.reads == 3 && id == "fac-140" && len(rows) > 0 {
		rows[0].Name = "changed"
	}
	return rows, err
}
func (m *mismatchLabels) DetachTaskLabel(ctx context.Context, label string) error {
	if m.failComp {
		return errors.New("compensation unavailable")
	}
	return m.MemoryProvider.DetachTaskLabel(ctx, label)
}

func TestRepairTaskRoleLabel_MismatchCompensatesOrBlocks(t *testing.T) {
	base := seedLabelTasks()
	p := &mismatchLabels{MemoryProvider: base, mismatch: true}
	err := RepairTaskRoleLabel(context.Background(), p, "fac-140", "fac-189", "forge-smith")
	if err == nil {
		t.Fatal("source readback mismatch must fail")
	}
	if _, ok := err.(*LabelTransactionError); ok {
		t.Fatal("successful compensation should remain a normal mismatch error")
	}
	base = seedLabelTasks()
	p = &mismatchLabels{MemoryProvider: base, mismatch: true, failComp: true}
	err = RepairTaskRoleLabel(context.Background(), p, "fac-140", "fac-189", "forge-smith")
	if !errors.Is(err, ErrLabelTransactionBlocked) {
		t.Fatalf("compensation failure must be durable blocked: %v", err)
	}
}

func TestEnsureTaskRoleLabel_ZeroLabelTargetNeedsNoSource(t *testing.T) {
	p := NewMemoryProvider()
	p.AddTask(&Task{ID: "fac-187", Ref: "FAC-187", ProjectID: "p", Status: "to-do", Title: "orphan"})
	if err := EnsureTaskRoleLabel(context.Background(), p, "fac-187", "forge-smith"); err != nil {
		t.Fatal(err)
	}
	rows, _ := p.ListTaskLabels(context.Background(), "fac-187")
	if len(rows) != 1 || rows[0].TaskID != "fac-187" || rows[0].Name != "forge-smith" {
		t.Fatalf("zero-label target not repaired from source-free row: %+v", rows)
	}
	if err := EnsureTaskRoleLabel(context.Background(), p, "fac-187", ""); err == nil {
		t.Fatal("unknown intended role must remain blocked")
	}
}

type failTerminalEvidence struct{ calls int }

func (s *failTerminalEvidence) RecordLabelRepairEvidence(e LabelRepairEvidence) error {
	s.calls++
	if e.Phase == "terminal" {
		return errors.New("terminal evidence unavailable")
	}
	return nil
}

func TestEnsureTaskRoleLabel_TerminalEvidenceFailureCompensates(t *testing.T) {
	p := NewMemoryProvider()
	p.AddTask(&Task{ID: "target", ProjectID: "project"})
	generation, err := ObservedLabelGeneration(context.Background(), p, "target")
	if err != nil {
		t.Fatal(err)
	}
	sink := &failTerminalEvidence{}
	err = EnsureTaskRoleLabelWithOptions(context.Background(), p, "target", "forge-smith", LabelRepairOptions{
		Repository: "repo", Provider: "memory", Project: "project", Evidence: sink, Revision: generation, Generation: generation, Operation: "ensure-source-free", TransactionID: "tx-1",
	})
	if !errors.Is(err, ErrLabelTransactionBlocked) {
		t.Fatalf("terminal evidence failure must block, got %v", err)
	}
	labels, readErr := p.ListTaskLabels(context.Background(), "target")
	if readErr != nil || len(labels) != 0 {
		t.Fatalf("terminal evidence failure leaked mutation: labels=%+v err=%v", labels, readErr)
	}
}

type targetDriftProvider struct {
	*MemoryProvider
	reads int
}

func (p *targetDriftProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	p.reads++
	if p.reads == 3 {
		p.MemoryProvider.mu.Lock()
		p.MemoryProvider.tasks["fac-189"].Labels = []string{"forge-smith"}
		p.MemoryProvider.mu.Unlock()
	}
	return p.MemoryProvider.GetTask(ctx, id)
}

func TestRepairTaskRoleLabel_UnrelatedTargetLabelLossIsRejected(t *testing.T) {
	p := &targetDriftProvider{MemoryProvider: seedLabelTasks()}
	err := RepairTaskRoleLabel(context.Background(), p, "fac-140", "fac-189", "forge-smith")
	if err == nil || errors.Is(err, ErrLabelTransactionBlocked) {
		t.Fatalf("unrelated target-label loss must fail with compensatable mismatch, got %v", err)
	}
	rows, _ := p.ListTaskLabels(context.Background(), "fac-189")
	if len(rows) != 1 || rows[0].Name != "unrelated" {
		t.Fatalf("compensation did not restore target label: %+v", rows)
	}
}

type sourceDriftProvider struct {
	*MemoryProvider
	reads int
}

func (p *sourceDriftProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	p.reads++
	if p.reads == 3 {
		p.MemoryProvider.mu.Lock()
		p.MemoryProvider.tasks["fac-140"].Description = "drifted"
		p.MemoryProvider.mu.Unlock()
	}
	return p.MemoryProvider.GetTask(ctx, id)
}

func TestRepairTaskRoleLabel_SourceTaskDriftIsRejected(t *testing.T) {
	p := &sourceDriftProvider{MemoryProvider: seedLabelTasks()}
	if err := RepairTaskRoleLabel(context.Background(), p, "fac-140", "fac-189", "forge-smith"); err == nil {
		t.Fatal("source task readback drift must fail")
	}
}

func TestKaneoCLI_LabelArgvContract(t *testing.T) {
	var calls []string
	workspaceLists := 0
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		calls = append(calls, strings.Join(args, " "))
		switch {
		case strings.HasPrefix(strings.Join(args, " "), "task label list t1"):
			return &CLIResult{Stdout: []byte(`[{"id":"l1","name":"forge-smith","taskId":"t1"}]`)}, nil
		case strings.HasPrefix(strings.Join(args, " "), "task label list t2"):
			return &CLIResult{Stdout: []byte(`[{"id":"l2","name":"forge-smith","taskId":"t2"}]`)}, nil
		case strings.HasPrefix(strings.Join(args, " "), "label list"):
			workspaceLists++
			if workspaceLists == 1 {
				return &CLIResult{Stdout: []byte(`[]`)}, nil
			}
			return &CLIResult{Stdout: []byte(`[{"id":"l2","name":"forge-smith","taskId":""}]`)}, nil
		case strings.HasPrefix(strings.Join(args, " "), "label create"):
			return &CLIResult{Stdout: []byte(`{"id":"l2","name":"forge-smith","taskId":""}`)}, nil
		default:
			return &CLIResult{Stdout: []byte(`{"id":"l2","taskId":"t2"}`)}, nil
		}
	}
	k := NewKaneoProvider("", "project-1", true)
	if _, err := k.ListTaskLabels(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	created, err := k.CreateTaskLabel(context.Background(), "t2", "forge-smith")
	if err != nil || created.TaskID != "" {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	if err := k.AttachTaskLabel(context.Background(), "t2", "l2"); err != nil {
		t.Fatal(err)
	}
	if err := k.DetachTaskLabel(context.Background(), "l2"); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteTaskLabel(context.Background(), "l2"); !errors.Is(err, ErrWorkspaceLabelDeleteRefused) {
		t.Fatalf("workspace label delete must fail closed: %v", err)
	}
	want := []string{
		"task label list t1 --json --project project-1",
		"label list --json --project project-1",
		"label create --color #808080 forge-smith --json --project project-1",
		"label list --json --project project-1",
		"task label add t2 l2 --project project-1",
		"task label list t2 --json --project project-1",
		"task label delete l2 --project project-1",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CLI argv mismatch:\n got %v\nwant %v", calls, want)
	}
}

func TestKaneoCLI_RedundantAttachNullIsTypedFailure(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		command := strings.Join(args, " ")
		if strings.HasPrefix(command, "label list") || strings.HasPrefix(command, "task label list") {
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
		return &CLIResult{Stdout: []byte("null")}, nil
	}
	k := NewKaneoProvider("", "project-1", true)
	if err := k.AttachTaskLabel(context.Background(), "t1", "l1"); !errors.Is(err, ErrRedundantLabelAttach) {
		t.Fatalf("null redundant attach=%v", err)
	}
}

func TestKaneoCLI_LabelCreationProofRejectsReplay(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	created := 0
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(command, "label list"):
			if created == 0 {
				return &CLIResult{Stdout: []byte(`[]`)}, nil
			}
			return &CLIResult{Stdout: []byte(`[{"id":"orphan","name":"forge-smith","taskId":""}]`)}, nil
		case strings.HasPrefix(command, "label create"):
			created++
			return &CLIResult{Stdout: []byte(`{"id":"orphan","name":"forge-smith","taskId":""}`)}, nil
		default:
			return &CLIResult{Stdout: []byte(`[]`)}, nil
		}
	}
	k := NewKaneoProvider("", "project-1", true)
	row, err := k.CreateTaskLabel(context.Background(), "target", "forge-smith")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ProveLabelCreation(context.Background(), row, "target", "forge-smith", LabelRepairOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.ProveLabelCreation(context.Background(), row, "target", "forge-smith", LabelRepairOptions{}); err == nil {
		t.Fatal("replayed identity must not be proven twice")
	}
}

func TestKaneoCLI_LabelCreationProofRejectsProtectedOrphan(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
		command := strings.Join(args, " ")
		if strings.HasPrefix(command, "label list") {
			return &CLIResult{Stdout: []byte(`[{"id":"foreign","name":"forge-smith","taskId":""}]`)}, nil
		}
		if strings.HasPrefix(command, "label create") {
			return &CLIResult{Stdout: []byte(`{"id":"foreign","name":"forge-smith","taskId":""}`)}, nil
		}
		return &CLIResult{Stdout: []byte(`[]`)}, nil
	}
	k := NewKaneoProvider("", "project-1", true)
	row, err := k.CreateTaskLabel(context.Background(), "target", "forge-smith")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ProveLabelCreation(context.Background(), row, "target", "forge-smith", LabelRepairOptions{}); err == nil {
		t.Fatal("foreign orphan must not be proven")
	}
}

func TestLabelFence_CanonicalPairAndTargetContention(t *testing.T) {
	p := NewMemoryProvider()
	f1, err := acquireLabelFence(p, "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		f2, e := acquireLabelFence(p, "B", "A")
		if e == nil {
			e = f2.release()
		}
		got <- e
	}()
	select {
	case <-got:
		t.Fatal("A/B and B/A acquired different pair authority")
	case <-time.After(50 * time.Millisecond):
	}
	if err := f1.release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pair contender did not acquire after release")
	}
	f3, err := acquireLabelFence(p, "source-1", "target")
	if err != nil {
		t.Fatal(err)
	}
	got = make(chan error, 1)
	go func() {
		f4, e := acquireLabelFence(p, "source-2", "target")
		if e == nil {
			e = f4.release()
		}
		got <- e
	}()
	select {
	case <-got:
		t.Fatal("two source candidates acquired one target authority")
	case <-time.After(50 * time.Millisecond):
	}
	if err := f3.release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("target contender did not acquire after release")
	}
}

func TestLabelFence_StaleOwnerReleaseIsObservable(t *testing.T) {
	p := NewMemoryProvider()
	f, err := acquireLabelFence(p, "source", "target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.files[0].Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.files[0].Write([]byte(`{"owner":"stale","generation":"stale","sequence":0}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.files[0].Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.release(); err == nil {
		t.Fatal("stale owner release must be observable")
	}
	for _, file := range f.files {
		_ = file.Close()
	}
}

func TestKaneoHTTP_LabelAssociationContractAndFailClosedBodies(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/api/label/l1/task" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"taskId":"t1"}` {
				t.Errorf("attach body=%s", body)
			}
			_, _ = w.Write([]byte(`{"id":"l1","taskId":"t1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"l1"}`))
	}))
	defer srv.Close()
	k := NewKaneoProvider(srv.URL, "project-1", false)
	k.KeyTrustedOrigin = srv.URL
	if err := k.AttachTaskLabel(context.Background(), "t1", "l1"); err != nil {
		t.Fatal(err)
	}
	if err := k.DetachTaskLabel(context.Background(), "l1"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, "\n") != "POST /api/label/l1/task\nDELETE /api/label/l1/task" {
		t.Fatalf("association routes=%v", paths)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"error":"boom"}`)) }))
	defer bad.Close()
	k = NewKaneoProvider(bad.URL, "project-1", false)
	k.KeyTrustedOrigin = bad.URL
	if err := k.AttachTaskLabel(context.Background(), "t1", "l1"); err == nil {
		t.Fatal("HTTP 200 error body must fail closed")
	}
}
