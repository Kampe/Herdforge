package deps

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// endpointFaultProvider wraps provider.MemoryProvider to reproduce, without a
// live board, two real Kaneo characteristics FAC-777's live evidence
// established:
//   - a single-task GetTask can fail for an id that a status-scoped ListTasks
//     still resolves (archived-column read succeeds, single GET 404s/400s);
//   - an unfiltered ListTasks("") can omit a row that a status-scoped read
//     finds (so hydrateFresh alone cannot be trusted to have cached it).
type endpointFaultProvider struct {
	*provider.MemoryProvider
	hideFromUnfiltered map[string]bool
	failGetTask        map[string]error
}

func newEndpointFaultProvider() *endpointFaultProvider {
	return &endpointFaultProvider{
		MemoryProvider:     provider.NewMemoryProvider(),
		hideFromUnfiltered: map[string]bool{},
		failGetTask:        map[string]error{},
	}
}

func (p *endpointFaultProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	if err, ok := p.failGetTask[id]; ok {
		return nil, err
	}
	return p.MemoryProvider.GetTask(ctx, id)
}

func (p *endpointFaultProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	tasks, err := p.MemoryProvider.ListTasks(ctx, projectID, status)
	if err != nil {
		return nil, err
	}
	if status != "" {
		return tasks, nil
	}
	out := make([]*provider.Task, 0, len(tasks))
	for _, t := range tasks {
		if t != nil && p.hideFromUnfiltered[t.ID] {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// TestArchivedEndpoint_ColumnEvidence_NotSingleGet reproduces the FAC-777
// live-corrected positive: an authenticated archived endpoint whose single
// GetTask fails, but whose project-scoped archived-column listing resolves
// it by exact id. This must NOT be confused with the earlier (now known
// stale) claim that CHA-2554 itself was archived -- it is a hermetic fixture
// only, matching the coordinator's fixture spec (GetTask of T may 404).
func TestArchivedEndpoint_ColumnEvidence_NotSingleGet(t *testing.T) {
	p := newEndpointFaultProvider()
	proj := "proj"
	tArchived := &provider.Task{ID: "id-t", Ref: "CHA-T", Status: provider.StatusArchived, ProjectID: proj, Title: "T"}
	bTask := &provider.Task{ID: "id-b", Ref: "FAC-B", Status: "to-do", ProjectID: proj, Title: "B"}
	p.AddTask(tArchived)
	p.AddTask(bTask)
	p.hideFromUnfiltered[tArchived.ID] = true
	p.failGetTask[tArchived.ID] = fmt.Errorf("kaneo: GetTask %s: 404 not found", tArchived.ID)
	if _, err := p.CreateRelation(context.Background(), tArchived.ID, bTask.ID, provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}

	store := NewProviderStore(p, proj)
	ctx := context.Background()

	// GREEN: full snapshot succeeds and keeps the edge.
	snap, err := store.SnapshotGraph(ctx)
	if err != nil {
		t.Fatalf("archived endpoint (column evidence) must not abort the snapshot: %v", err)
	}
	if len(snap.Edges) != 1 || string(snap.Edges[0].SourceRef) != "CHA-T" || string(snap.Edges[0].TargetRef) != "FAC-B" {
		t.Fatalf("archived-terminal edge must be kept, got %+v", snap.Edges)
	}

	// GREEN: B is eligible -- archived prerequisite is terminal, not open.
	desiredB := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-B", TaskID: TaskID(bTask.ID), Present: true,
		Edges: []DependencyEdge{{SourceRef: "CHA-T", TargetRef: "FAC-B", Type: EdgeBlocks}},
	}
	if _, err := RequireTaskLaunch(ctx, store, EntryDispatch, "FAC-B", desiredB, ""); err != nil {
		t.Fatalf("task with archived (column-evidenced) prerequisite must be eligible: %v", err)
	}

	// GREEN: the archived task itself stays ineligible for launch.
	desiredT := &Provenance{Version: SchemaVersion, TaskRef: "CHA-T", TaskID: TaskID(tArchived.ID), Present: true}
	_, terr := RequireTaskLaunch(ctx, store, EntryDispatch, "CHA-T", desiredT, "")
	var blocked *BlockedError
	if terr == nil || !errors.As(terr, &blocked) || blocked.Code != "terminal" {
		t.Fatalf("archived task itself must stay BLOCKED(terminal), got %v", terr)
	}
}

// TestForeignEndpoint_NoArchiveEvidence_StaysRefused mirrors the live
// n0azs82a evidence: GetTask fails with a workspace/400-shaped error and the
// id is absent from the archived column. This must never be classified
// archived, and the task genuinely connected to it must stay refused.
func TestForeignEndpoint_NoArchiveEvidence_StaysRefused(t *testing.T) {
	p := newEndpointFaultProvider()
	proj := "proj"
	bTask := &provider.Task{ID: "id-b", Ref: "FAC-B", Status: "to-do", ProjectID: proj, Title: "B"}
	// The endpoint is genuinely in-project (mirrors live n0azs82a: same
	// Chainseer project, GetTask 400 workspace-shaped, absent from both the
	// unfiltered and the archived-column listing).
	foreign := &provider.Task{ID: "id-foreign", Ref: "CHA-FOREIGN", Status: "to-do", ProjectID: proj, Title: "foreign"}
	p.AddTask(bTask)
	p.AddTask(foreign)
	p.hideFromUnfiltered["id-foreign"] = true
	p.failGetTask["id-foreign"] = fmt.Errorf("kaneo: workspace ID could not be determined (400)")
	if _, err := p.CreateRelation(context.Background(), "id-foreign", bTask.ID, provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}

	store := NewProviderStore(p, proj)
	ctx := context.Background()

	if _, err := store.SnapshotGraph(ctx); err == nil {
		t.Fatalf("full snapshot must stay honest (fail closed) for a genuinely foreign/missing endpoint")
	}

	desiredB := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-B", TaskID: TaskID(bTask.ID), Present: true,
		Edges: []DependencyEdge{{SourceRef: "CHA-FOREIGN", TargetRef: "FAC-B", Type: EdgeBlocks}},
	}
	if _, err := RequireTaskLaunch(ctx, store, EntryDispatch, "FAC-B", desiredB, ""); err == nil {
		t.Fatalf("task genuinely depending on a foreign/missing endpoint must refuse before claim")
	}
}

// TestUnrelatedTaskIsolatedFromForeignEndpoint proves the corrected FAC-777
// requirement: a bad (missing/foreign) endpoint attached to an UNRELATED
// board component must not poison an eligible task's dispatch, because the
// existing scoped graph authority (ValidateLaunch -> SnapshotGraphForTask)
// isolates the requested connected component. The full project snapshot for
// the SAME board still fails honestly -- proving this is real isolation, not
// a weakened fail-closed guard.
func TestUnrelatedTaskIsolatedFromForeignEndpoint(t *testing.T) {
	p := newEndpointFaultProvider()
	proj := "proj"
	aTask := &provider.Task{ID: "id-a", Ref: "FAC-A", Status: "to-do", ProjectID: proj, Title: "A"}
	bTask := &provider.Task{ID: "id-b", Ref: "FAC-B", Status: "to-do", ProjectID: proj, Title: "B"}
	foreign := &provider.Task{ID: "id-foreign", Ref: "CHA-FOREIGN", Status: "to-do", ProjectID: proj, Title: "foreign"}
	p.AddTask(aTask) // A has no relation to anything -- its own component.
	p.AddTask(bTask)
	p.AddTask(foreign)
	p.hideFromUnfiltered["id-foreign"] = true
	p.failGetTask["id-foreign"] = fmt.Errorf("kaneo: workspace ID could not be determined (400)")
	if _, err := p.CreateRelation(context.Background(), "id-foreign", bTask.ID, provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}

	store := NewProviderStore(p, proj)
	ctx := context.Background()

	// The whole-project snapshot is genuinely poisoned by B's bad prerequisite.
	if _, err := store.SnapshotGraph(ctx); err == nil {
		t.Fatalf("full project snapshot must still fail for the genuinely foreign endpoint")
	}

	// A is not connected to it at all: its scoped launch must not be poisoned.
	desiredA := &Provenance{Version: SchemaVersion, TaskRef: "FAC-A", TaskID: TaskID(aTask.ID), Present: true}
	if _, err := RequireTaskLaunch(ctx, store, EntryDispatch, "FAC-A", desiredA, ""); err != nil {
		t.Fatalf("unrelated eligible task must not be poisoned by a foreign endpoint elsewhere in the project: %v", err)
	}

	// B, which is actually connected to the foreign endpoint, still refuses.
	desiredB := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-B", TaskID: TaskID(bTask.ID), Present: true,
		Edges: []DependencyEdge{{SourceRef: "CHA-FOREIGN", TargetRef: "FAC-B", Type: EdgeBlocks}},
	}
	if _, err := RequireTaskLaunch(ctx, store, EntryDispatch, "FAC-B", desiredB, ""); err == nil {
		t.Fatalf("the task actually depending on the foreign endpoint must still refuse before claim")
	}
}
