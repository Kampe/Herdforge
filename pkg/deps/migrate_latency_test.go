package deps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

type latencyMigrationProvider struct {
	*provider.MemoryProvider

	mu                   sync.Mutex
	getCalls             map[string]int
	relationCalls        map[string]int
	listTasksCalls       int
	listProjectRelCalls  int
	timeoutRelationID    string
	timeoutRelationCalls int
	blockRelationID      string
	blockRelationPrefix  string
	blockStarted         chan struct{}
	blockRelease         chan struct{}
	inFlight             int
	maxInFlight          int
}

func newLatencyMigrationProvider() *latencyMigrationProvider {
	return &latencyMigrationProvider{
		MemoryProvider: provider.NewMemoryProvider(),
		getCalls:       map[string]int{},
		relationCalls:  map[string]int{},
	}
}

func (p *latencyMigrationProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	p.mu.Lock()
	p.getCalls[id]++
	p.mu.Unlock()
	return p.MemoryProvider.GetTask(ctx, id)
}

func (p *latencyMigrationProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	p.mu.Lock()
	p.listTasksCalls++
	p.mu.Unlock()
	return p.MemoryProvider.ListTasks(ctx, projectID, status)
}

func (p *latencyMigrationProvider) ListRelations(ctx context.Context, taskID string) ([]provider.Relation, error) {
	p.mu.Lock()
	p.relationCalls[taskID]++
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	call := p.relationCalls[taskID]
	timeoutID := p.timeoutRelationID
	timeoutCalls := p.timeoutRelationCalls
	blockID := p.blockRelationID
	blockPrefix := p.blockRelationPrefix
	started := p.blockStarted
	release := p.blockRelease
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()

	if taskID == timeoutID && call <= timeoutCalls {
		return nil, provider.AsTimeout("fixture", "ListRelations", provider.OpList, time.Millisecond, context.DeadlineExceeded)
	}
	if taskID == blockID || (blockPrefix != "" && strings.HasPrefix(taskID, blockPrefix)) {
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, provider.AsTimeout("fixture", "ListRelations", provider.OpList, time.Millisecond, ctx.Err())
		case <-release:
		}
	}
	return p.MemoryProvider.ListRelations(ctx, taskID)
}

func (p *latencyMigrationProvider) ListProjectRelations(ctx context.Context, projectID string) ([]provider.Relation, error) {
	p.mu.Lock()
	p.listProjectRelCalls++
	p.mu.Unlock()
	return p.MemoryProvider.ListProjectRelations(ctx, projectID)
}

func (p *latencyMigrationProvider) counts() (map[string]int, map[string]int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	gets := make(map[string]int, len(p.getCalls))
	for key, value := range p.getCalls {
		gets[key] = value
	}
	relations := make(map[string]int, len(p.relationCalls))
	for key, value := range p.relationCalls {
		relations[key] = value
	}
	return gets, relations, p.listTasksCalls, p.listProjectRelCalls
}

func (p *latencyMigrationProvider) peakRelationConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

func seedMigrationChain(t *testing.T, p *latencyMigrationProvider, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		p.AddTask(&provider.Task{
			ID:        fmt.Sprintf("task-%d", i),
			Ref:       fmt.Sprintf("FAC-%d", 685+i),
			Status:    provider.StatusInProgress,
			ProjectID: "p",
		})
		if i > 1 {
			if _, err := p.CreateRelation(context.Background(), fmt.Sprintf("task-%d", i-1), fmt.Sprintf("task-%d", i), provider.RelationBlocks); err != nil {
				t.Fatalf("seed relation %d: %v", i, err)
			}
		}
	}
}

func TestPlanMigrationForRefReadsEachComponentMemberOnce(t *testing.T) {
	tests := []struct {
		name  string
		count int
	}{
		{name: "single task", count: 1},
		{name: "four task component", count: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := newLatencyMigrationProvider()
			seedMigrationChain(t, tp, tt.count)
			store := NewProviderStore(tp, "p")

			plan, err := PlanMigrationForRef(context.Background(), store, tp, "p", "FAC-686")
			if err != nil {
				t.Fatalf("PlanMigrationForRef: %v", err)
			}
			if plan == nil || len(plan.Items) != 1 {
				t.Fatalf("plan = %+v, want one exact item", plan)
			}
			gets, relations, broadTasks, broadRelations := tp.counts()
			if broadTasks != 0 || broadRelations != 0 {
				t.Fatalf("exact walk used broad reads: tasks=%d relations=%d", broadTasks, broadRelations)
			}
			if len(gets) != tt.count || len(relations) != tt.count {
				t.Fatalf("unique exact reads: gets=%v relations=%v, want %d each", gets, relations, tt.count)
			}
			for i := 1; i <= tt.count; i++ {
				id := fmt.Sprintf("task-%d", i)
				wantLookup := id
				if i == 1 {
					wantLookup = "FAC-686"
				}
				if gets[wantLookup] != 1 {
					t.Fatalf("GetTask(%s) calls = %d, want 1 (all=%v)", wantLookup, gets[wantLookup], gets)
				}
				if relations[id] != 1 {
					t.Fatalf("ListRelations(%s) calls = %d, want 1 (all=%v)", id, relations[id], relations)
				}
			}
		})
	}
}

func TestPlanMigrationForRefRetriesOnlyTimedOutOperation(t *testing.T) {
	tp := newLatencyMigrationProvider()
	seedMigrationChain(t, tp, 3)
	tp.timeoutRelationID = "task-3"
	tp.timeoutRelationCalls = 1
	store := NewProviderStore(tp, "p")

	plan, err := PlanMigrationForRef(context.Background(), store, tp, "p", "FAC-686")
	if err != nil {
		t.Fatalf("PlanMigrationForRef: %v", err)
	}
	if plan == nil || !plan.OK {
		t.Fatalf("plan = %+v, want recovered exact plan", plan)
	}
	gets, relations, broadTasks, broadRelations := tp.counts()
	if broadTasks != 0 || broadRelations != 0 {
		t.Fatalf("retry used broad reads: tasks=%d relations=%d", broadTasks, broadRelations)
	}
	if gets["FAC-686"] != 1 || gets["task-2"] != 1 || gets["task-3"] != 1 {
		t.Fatalf("completed GetTask reads repeated: %v", gets)
	}
	if relations["task-1"] != 1 || relations["task-2"] != 1 || relations["task-3"] != 2 {
		t.Fatalf("relation retries = %v, want only task-3 retried", relations)
	}
}

func TestPlanMigrationForRefTimeoutIsStructured(t *testing.T) {
	tp := newLatencyMigrationProvider()
	seedMigrationChain(t, tp, 1)
	tp.blockRelationID = "task-1"
	tp.blockRelease = make(chan struct{})
	store := NewProviderStore(tp, "p")
	ctx, cancel := WithMigrationRequestBudget(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err := PlanMigrationForRef(ctx, store, tp, "p", "FAC-686")
	if err == nil {
		t.Fatal("timed out component walk succeeded")
	}
	for _, field := range []string{"phase=relations", "ref=FAC-686", "tasks=1", "relations=0", "deadline="} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("timeout %q missing structured field %q", err, field)
		}
	}
	if provider.ClassifyOpError(err) != provider.OpTimeout {
		t.Fatalf("timeout class = %q, want provider_timeout: %v", provider.ClassifyOpError(err), err)
	}
}

func TestPlanMigrationForRefBoundsFrontierConcurrency(t *testing.T) {
	tp := newLatencyMigrationProvider()
	tp.AddTask(&provider.Task{ID: "root-id", Ref: "FAC-686", Status: provider.StatusInProgress, ProjectID: "p"})
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("leaf-%02d", i)
		tp.AddTask(&provider.Task{ID: id, Ref: fmt.Sprintf("FAC-%d", 700+i), Status: provider.StatusToDo, ProjectID: "p"})
		if _, err := tp.CreateRelation(context.Background(), "root-id", id, provider.RelationBlocks); err != nil {
			t.Fatalf("seed relation %d: %v", i, err)
		}
	}
	tp.blockRelationPrefix = "leaf-"
	tp.blockStarted = make(chan struct{}, 12)
	tp.blockRelease = make(chan struct{})
	store := NewProviderStore(tp, "p")
	done := make(chan error, 1)
	go func() {
		_, err := PlanMigrationForRef(context.Background(), store, tp, "p", "FAC-686")
		done <- err
	}()

	deadline := time.After(time.Second)
	for tp.peakRelationConcurrency() < DefaultMigrationFrontierConcurrency {
		select {
		case <-tp.blockStarted:
		case <-deadline:
			t.Fatalf("frontier never reached bounded concurrency: peak=%d", tp.peakRelationConcurrency())
		}
	}
	peak := tp.peakRelationConcurrency()
	if peak <= 1 || peak > DefaultMigrationFrontierConcurrency {
		t.Fatalf("relation frontier peak = %d, want 2..%d", peak, DefaultMigrationFrontierConcurrency)
	}
	close(tp.blockRelease)
	if err := <-done; err != nil {
		t.Fatalf("PlanMigrationForRef: %v", err)
	}
	gets, relations, broadTasks, broadRelations := tp.counts()
	if len(gets) != 13 || len(relations) != 13 || broadTasks != 0 || broadRelations != 0 {
		t.Fatalf("bounded frontier reads: gets=%v relations=%v broad=%d/%d", gets, relations, broadTasks, broadRelations)
	}
}

func TestPlanMigrationForRefStreamsStructuredComponentProgress(t *testing.T) {
	tp := newLatencyMigrationProvider()
	seedMigrationChain(t, tp, 2)
	tp.blockRelationID = "task-2"
	tp.blockStarted = make(chan struct{}, 1)
	tp.blockRelease = make(chan struct{})
	store := NewProviderStore(tp, "p")
	progress := make(chan MigrateItem, 8)
	done := make(chan error, 1)

	go func() {
		_, err := PlanMigrationForRefWithProgress(context.Background(), store, tp, "p", "FAC-686", func(item MigrateItem, _, _ int) {
			progress <- item
		})
		done <- err
	}()

	select {
	case <-tp.blockStarted:
	case <-time.After(time.Second):
		t.Fatal("component walk did not reach blocked frontier")
	}

	var event MigrateItem
	select {
	case event = <-progress:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no progress was emitted while component traversal was still running")
	}
	if event.Action != "component_progress" {
		t.Fatalf("progress action = %q, want component_progress", event.Action)
	}
	var detail struct {
		Phase         string `json:"phase"`
		Ref           string `json:"ref"`
		TasksRead     int    `json:"tasks_read"`
		RelationsRead int    `json:"relation_sets_read"`
		Pending       int    `json:"pending"`
		Deadline      string `json:"deadline"`
	}
	if err := json.Unmarshal([]byte(event.Detail), &detail); err != nil {
		t.Fatalf("progress detail is not structured JSON: %q: %v", event.Detail, err)
	}
	if detail.Ref != "FAC-686" || detail.TasksRead < 1 || detail.Deadline == "" || detail.Phase == "" {
		t.Fatalf("progress detail = %+v", detail)
	}
	close(tp.blockRelease)
	if err := <-done; err != nil {
		t.Fatalf("PlanMigrationForRefWithProgress: %v", err)
	}
}

type cancelAfterSetWriter struct {
	*recordingDescriptionWriter
	cancel context.CancelFunc
	once   sync.Once
}

type cancelErrorAfterSetWriter struct {
	*recordingDescriptionWriter
	cancel context.CancelFunc
	once   sync.Once
}

func (w *cancelErrorAfterSetWriter) SetDescription(ctx context.Context, taskID, description string) error {
	err := w.recordingDescriptionWriter.SetDescription(ctx, taskID, description)
	returnedCancellation := false
	w.once.Do(func() {
		w.cancel()
		returnedCancellation = true
	})
	if err == nil && returnedCancellation {
		return context.Canceled
	}
	return err
}

func (w *cancelAfterSetWriter) SetDescription(ctx context.Context, taskID, description string) error {
	err := w.recordingDescriptionWriter.SetDescription(ctx, taskID, description)
	if err == nil {
		w.once.Do(w.cancel)
	}
	return err
}

func TestApplyMigrationForRefCancellationGates(t *testing.T) {
	tests := []struct {
		name          string
		cancelLate    bool
		ambiguousLate bool
	}{
		{name: "pre mutation cancellation writes nothing"},
		{name: "post mutation cancellation rolls back exact before image", cancelLate: true},
		{name: "ambiguous canceled write rolls back exact before image", ambiguousLate: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := newExactMigrationProvider()
			const before = "cancellation before image"
			tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-686", Status: provider.StatusInProgress, ProjectID: "p", Description: before})
			store := NewProviderStore(tp, "p")
			baseWriter := &recordingDescriptionWriter{mp: tp.MemoryProvider}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var writer DescriptionWriter = baseWriter
			if tt.ambiguousLate {
				writer = &cancelErrorAfterSetWriter{recordingDescriptionWriter: baseWriter, cancel: cancel}
			} else if tt.cancelLate {
				writer = &cancelAfterSetWriter{recordingDescriptionWriter: baseWriter, cancel: cancel}
			} else {
				cancel()
			}
			journalDir := t.TempDir()

			plan, err := ApplyMigrationForRef(ctx, store, tp, "p", "FAC-686", writer, journalDir)
			if err == nil && (plan == nil || plan.OK) {
				t.Fatalf("canceled migration reported success: plan=%+v err=%v", plan, err)
			}
			task, getErr := tp.GetTask(context.Background(), "target-id")
			if getErr != nil || task.Description != before {
				t.Fatalf("description after cancellation = %q err=%v, want exact before image %q", task.Description, getErr, before)
			}
			if !tt.cancelLate && !tt.ambiguousLate {
				if len(baseWriter.setIDs) != 0 {
					t.Fatalf("pre-mutation cancellation wrote descriptions: %v", baseWriter.setIDs)
				}
				entries, readErr := os.ReadDir(journalDir)
				if readErr != nil {
					t.Fatalf("read journal dir: %v", readErr)
				}
				if len(entries) != 0 {
					t.Fatalf("pre-mutation cancellation wrote journal files: %v", entries)
				}
				return
			}
			if got := strings.Join(baseWriter.setIDs, ","); got != "target-id,target-id" {
				t.Fatalf("post-mutation write+rollback ids = %q", got)
			}
			if plan == nil || len(plan.Items) != 1 || !plan.Items[0].RolledBack || plan.Items[0].ReadbackOK {
				t.Fatalf("post-mutation cancellation plan = %+v", plan)
			}
		})
	}
}
