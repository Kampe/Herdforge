package deps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

type exactMigrationProvider struct {
	*provider.MemoryProvider
	getCalls            []string
	listTasksCalls      int
	listRelationsCalls  []string
	listProjectRelCalls int
	getOverride         func(context.Context, string) (*provider.Task, error)
}

func newExactMigrationProvider() *exactMigrationProvider {
	return &exactMigrationProvider{MemoryProvider: provider.NewMemoryProvider()}
}

func (p *exactMigrationProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	p.getCalls = append(p.getCalls, id)
	if p.getOverride != nil {
		return p.getOverride(ctx, id)
	}
	return p.MemoryProvider.GetTask(ctx, id)
}

func (p *exactMigrationProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	p.listTasksCalls++
	return p.MemoryProvider.ListTasks(ctx, projectID, status)
}

func (p *exactMigrationProvider) ListRelations(ctx context.Context, taskID string) ([]provider.Relation, error) {
	p.listRelationsCalls = append(p.listRelationsCalls, taskID)
	return p.MemoryProvider.ListRelations(ctx, taskID)
}

func (p *exactMigrationProvider) ListProjectRelations(ctx context.Context, projectID string) ([]provider.Relation, error) {
	p.listProjectRelCalls++
	return p.MemoryProvider.ListProjectRelations(ctx, projectID)
}

// TestPlanMigrationForRefScopesWithoutBoardScan1233 is the named mutation
// guard for FAC-646. Removing the exact-ref branch makes this exercise the
// 1,233 unrelated active cards and fail on ListTasks/ListProjectRelations.
func TestPlanMigrationForRefScopesWithoutBoardScan1233(t *testing.T) {
	tp := newExactMigrationProvider()
	for i := 0; i < 1233; i++ {
		tp.AddTask(&provider.Task{
			ID:        fmt.Sprintf("unrelated-%04d", i),
			Ref:       fmt.Sprintf("FAC-%d", 10000+i),
			Status:    provider.StatusToDo,
			ProjectID: "p",
		})
	}
	tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-646", Status: provider.StatusInProgress, ProjectID: "p"})
	store := NewProviderStore(tp, "p")

	plan, err := PlanMigrationForRef(context.Background(), store, tp, "p", "FAC-646")
	if err != nil {
		t.Fatalf("PlanMigrationForRef: %v", err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("exact plan item count = %d, want 1", len(plan.Items))
	}
	if plan.Items[0].Ref != "FAC-646" || plan.Items[0].TaskID != "target-id" {
		t.Fatalf("exact plan item = %+v, want FAC-646/target-id", plan.Items[0])
	}
	if tp.listTasksCalls != 0 || store.ListTasksCalls.Load() != 0 {
		t.Fatalf("exact plan listed tasks: provider=%d store=%d", tp.listTasksCalls, store.ListTasksCalls.Load())
	}
	if tp.listProjectRelCalls != 0 || store.BulkRelCalls.Load() != 0 {
		t.Fatalf("exact plan listed project relations: provider=%d store=%d", tp.listProjectRelCalls, store.BulkRelCalls.Load())
	}
	if got := strings.Join(tp.listRelationsCalls, ","); got != "target-id" {
		t.Fatalf("scoped relation snapshots touched %q, want target-id only", got)
	}
	for _, lookup := range tp.getCalls {
		if lookup != "FAC-646" && lookup != "target-id" {
			t.Fatalf("exact plan resolved unrelated task %q", lookup)
		}
	}
}

func TestPlanMigrationForRefReadsExactRelationComponentWithoutProjectScan(t *testing.T) {
	tp := newExactMigrationProvider()
	tp.AddTask(&provider.Task{ID: "blocker-id", Ref: "FAC-545", Status: provider.StatusDone, ProjectID: "p"})
	tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-646", Status: provider.StatusInProgress, ProjectID: "p"})
	if _, err := tp.CreateRelation(context.Background(), "blocker-id", "target-id", provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}
	store := NewProviderStore(tp, "p")

	plan, err := PlanMigrationForRef(context.Background(), store, tp, "p", "FAC-646")
	if err != nil {
		t.Fatalf("PlanMigrationForRef: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].EdgeCount != 1 || len(plan.Items[0].IntendedEdges) != 1 {
		t.Fatalf("relation plan = %+v, want one exact blocks edge", plan.Items)
	}
	edge := plan.Items[0].IntendedEdges[0]
	if edge.SourceID != "blocker-id" || edge.TargetID != "target-id" || edge.Type != EdgeBlocks {
		t.Fatalf("intended edge = %+v", edge)
	}
	if tp.listTasksCalls != 0 || tp.listProjectRelCalls != 0 {
		t.Fatalf("relation component fell back broad: tasks=%d relations=%d", tp.listTasksCalls, tp.listProjectRelCalls)
	}
	if got := strings.Join(tp.listRelationsCalls, ","); got != "target-id,blocker-id" {
		t.Fatalf("scoped relation walk = %q, want target then blocker", got)
	}
}

func TestPlanMigrationForRefRejectsInvalidExactIdentityWithoutBroadFallback(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*exactMigrationProvider)
		wantDetail string
	}{
		{name: "missing", wantDetail: "task not found"},
		{
			name: "terminal",
			configure: func(tp *exactMigrationProvider) {
				tp.AddTask(&provider.Task{ID: "done-id", Ref: "FAC-646", Status: provider.StatusDone, ProjectID: "p"})
			},
			wantDetail: "terminal task status",
		},
		{
			name: "duplicate ref",
			configure: func(tp *exactMigrationProvider) {
				tp.AddTask(&provider.Task{ID: "duplicate-a", Ref: "FAC-646", Status: provider.StatusToDo, ProjectID: "p"})
				tp.AddTask(&provider.Task{ID: "duplicate-b", Ref: "FAC-646", Status: provider.StatusToDo, ProjectID: "p"})
			},
			wantDetail: "duplicate task ref",
		},
		{
			name: "mismatched ref",
			configure: func(tp *exactMigrationProvider) {
				tp.getOverride = func(context.Context, string) (*provider.Task, error) {
					return &provider.Task{ID: "wrong-id", Ref: "FAC-999", Status: provider.StatusToDo, ProjectID: "p"}, nil
				}
			},
			wantDetail: "task ref mismatch",
		},
		{
			name: "mismatched project",
			configure: func(tp *exactMigrationProvider) {
				tp.AddTask(&provider.Task{ID: "moved-id", Ref: "FAC-646", Status: provider.StatusToDo, ProjectID: "other"})
			},
			wantDetail: "task project mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := newExactMigrationProvider()
			if tt.configure != nil {
				tt.configure(tp)
			}
			store := NewProviderStore(tp, "p")
			plan, err := PlanMigrationForRef(context.Background(), store, tp, "p", "FAC-646")
			if err == nil || plan != nil {
				t.Fatalf("invalid exact identity admitted: plan=%+v err=%v", plan, err)
			}
			if !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("error %q does not contain %q", err, tt.wantDetail)
			}
			if tp.listTasksCalls != 0 || tp.listProjectRelCalls != 0 {
				t.Fatalf("identity rejection fell back broad: tasks=%d relations=%d", tp.listTasksCalls, tp.listProjectRelCalls)
			}
		})
	}
}

func TestPlanMigrationForRefPreservesProviderTimeoutDiagnostic(t *testing.T) {
	tp := newExactMigrationProvider()
	tp.getOverride = func(context.Context, string) (*provider.Task, error) {
		return nil, provider.AsTimeout("kaneo", "GetTask", provider.OpGet, 0, context.DeadlineExceeded)
	}
	store := NewProviderStore(tp, "p")
	_, err := PlanMigrationForRef(context.Background(), store, tp, "p", "FAC-646")
	if err == nil || provider.ClassifyOpError(err) != provider.OpTimeout {
		t.Fatalf("provider timeout diagnostic lost: %v", err)
	}
	if tp.listTasksCalls != 0 || tp.listProjectRelCalls != 0 {
		t.Fatalf("provider timeout fell back broad: tasks=%d relations=%d", tp.listTasksCalls, tp.listProjectRelCalls)
	}
}

type recordingDescriptionWriter struct {
	mp              *provider.MemoryProvider
	setIDs          []string
	getCalls        int
	corruptReadback bool
}

func (w *recordingDescriptionWriter) SetDescription(ctx context.Context, taskID, description string) error {
	w.setIDs = append(w.setIDs, taskID)
	return (MemoryDescriptionWriter{MP: w.mp}).SetDescription(ctx, taskID, description)
}

func (w *recordingDescriptionWriter) GetDescription(ctx context.Context, taskID string) (string, error) {
	w.getCalls++
	if w.corruptReadback && w.getCalls == 2 {
		return "description without a dependency fence", nil
	}
	return (MemoryDescriptionWriter{MP: w.mp}).GetDescription(ctx, taskID)
}

func TestApplyMigrationForRefWritesOnlyTargetAndJournalsBeforeImage(t *testing.T) {
	tp := newExactMigrationProvider()
	const before = "target before description"
	tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-646", Status: provider.StatusInProgress, ProjectID: "p", Description: before})
	tp.AddTask(&provider.Task{ID: "other-id", Ref: "FAC-647", Status: provider.StatusToDo, ProjectID: "p", Description: "other before"})
	store := NewProviderStore(tp, "p")
	writer := &recordingDescriptionWriter{mp: tp.MemoryProvider}

	plan, err := ApplyMigrationForRef(context.Background(), store, tp, "p", "FAC-646", writer, t.TempDir())
	if err != nil {
		t.Fatalf("ApplyMigrationForRef: %v", err)
	}
	if !plan.OK || len(plan.Items) != 1 || !plan.Items[0].Applied || !plan.Items[0].ReadbackOK {
		t.Fatalf("apply plan = %+v", plan)
	}
	if got := strings.Join(writer.setIDs, ","); got != "target-id" {
		t.Fatalf("description writes = %q, want target-id only", got)
	}
	other, err := tp.GetTask(context.Background(), "other-id")
	if err != nil || other.Description != "other before" {
		t.Fatalf("unrelated task mutated: task=%+v err=%v", other, err)
	}
	raw, err := os.ReadFile(plan.JournalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var journal Journal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatalf("decode journal: %v", err)
	}
	if len(journal.Entries) != 1 || journal.Entries[0].TaskID != "target-id" || journal.Entries[0].BeforeDesc != before {
		t.Fatalf("journal entries = %+v", journal.Entries)
	}
	if tp.listTasksCalls != 0 || tp.listProjectRelCalls != 0 {
		t.Fatalf("exact apply fell back broad: tasks=%d relations=%d", tp.listTasksCalls, tp.listProjectRelCalls)
	}
}

func TestApplyMigrationForRefReadbackFailureRollsBack(t *testing.T) {
	tp := newExactMigrationProvider()
	const before = "before readback failure"
	tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-646", Status: provider.StatusInProgress, ProjectID: "p", Description: before})
	store := NewProviderStore(tp, "p")
	writer := &recordingDescriptionWriter{mp: tp.MemoryProvider, corruptReadback: true}

	plan, err := ApplyMigrationForRef(context.Background(), store, tp, "p", "FAC-646", writer, t.TempDir())
	if err != nil {
		t.Fatalf("ApplyMigrationForRef: %v", err)
	}
	if plan.OK || len(plan.Items) != 1 || !plan.Items[0].RolledBack || plan.Items[0].ReadbackOK {
		t.Fatalf("failed readback plan = %+v", plan)
	}
	if got := strings.Join(writer.setIDs, ","); got != "target-id,target-id" {
		t.Fatalf("write+rollback ids = %q", got)
	}
	task, err := tp.GetTask(context.Background(), "target-id")
	if err != nil || task.Description != before {
		t.Fatalf("rollback description = %q err=%v, want %q", task.Description, err, before)
	}
}

type revisionSequenceStore struct {
	*MemoryStore
	revisions []string
	calls     int
}

func (s *revisionSequenceStore) SnapshotGraphForTask(ctx context.Context, _ Ref, _ TaskID, _ []DependencyEdge) (*GraphSnapshot, error) {
	s.calls++
	snap, err := s.MemoryStore.SnapshotGraph(ctx)
	if err != nil {
		return nil, err
	}
	idx := s.calls - 1
	if idx >= len(s.revisions) {
		idx = len(s.revisions) - 1
	}
	snap.ProviderRevision = s.revisions[idx]
	return snap, nil
}

func TestApplyMigrationForRefMovedProviderRevisionRollsBack(t *testing.T) {
	mp := provider.NewMemoryProvider()
	const before = "before provider moved"
	mp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-646", Status: provider.StatusInProgress, ProjectID: "p", Description: before})
	store := &revisionSequenceStore{MemoryStore: NewMemoryStore(), revisions: []string{"initial", "fresh", "moved"}}
	writer := &recordingDescriptionWriter{mp: mp}

	plan, err := ApplyMigrationForRef(context.Background(), store, mp, "p", "FAC-646", writer, t.TempDir())
	if err != nil {
		t.Fatalf("ApplyMigrationForRef: %v", err)
	}
	if plan.OK || len(plan.Items) != 1 || !plan.Items[0].RolledBack || !strings.Contains(plan.Items[0].Detail, "revision moved") {
		t.Fatalf("moved revision plan = %+v", plan)
	}
	if store.calls != 3 {
		t.Fatalf("scoped snapshot calls = %d, want plan + fresh replan + readback", store.calls)
	}
	task, err := mp.GetTask(context.Background(), "target-id")
	if err != nil || task.Description != before {
		t.Fatalf("moved-revision rollback description = %q err=%v", task.Description, err)
	}
}
