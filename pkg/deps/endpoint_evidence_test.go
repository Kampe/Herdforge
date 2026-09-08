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
	// archivedOverride, when non-nil, makes ListTasks(proj, "archived") return
	// exactly this slice -- independently controlled, may not correspond to
	// any real task -- so review-adversarial rows (wrong status, wrong
	// project, colliding ref/id, duplicates) can be injected directly.
	archivedOverride []*provider.Task
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
	if status == provider.StatusArchived && p.archivedOverride != nil {
		return p.archivedOverride, nil
	}
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
	// Fail GetTask by both id and ref: gate.go's prerequisite-closure loop
	// re-resolves T's status via TaskStatus, which re-hydrates (dropping the
	// hidden T) and falls back to a ref-mode lookup for an uncached ref. A
	// plain MemoryProvider.GetTask also matches by ref, which would otherwise
	// resolve T directly and never actually exercise the archived-column
	// fallback this test claims to cover.
	notFoundT := fmt.Errorf("kaneo: GetTask %s: 404 not found", tArchived.ID)
	p.failGetTask[tArchived.ID] = notFoundT
	p.failGetTask[tArchived.Ref] = notFoundT
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

// The following four tests reproduce FAC-777 review R3's HIGH finding: the
// archived-column fallback is an authority-bearing launch input and must not
// trust the listing's own filters. Each independently controls the archived
// query response (archivedOverride) to inject a row that a real Kaneo-shaped
// provider should never legitimately return for the query it answered.

// TestArchivedEvidence_RejectsNonArchivedStatusRow reproduces: an exact-ID
// row in the correct project with status=done returned from the archived
// listing must not authorize the connected dependent task.
func TestArchivedEvidence_RejectsNonArchivedStatusRow(t *testing.T) {
	p := newEndpointFaultProvider()
	proj := "proj"
	tTask := &provider.Task{ID: "id-t", Ref: "CHA-T", Status: "to-do", ProjectID: proj, Title: "T"}
	bTask := &provider.Task{ID: "id-b", Ref: "FAC-B", Status: "to-do", ProjectID: proj, Title: "B"}
	p.AddTask(tTask)
	p.AddTask(bTask)
	p.hideFromUnfiltered[tTask.ID] = true
	// Fail GetTask by both id and ref: gate.go's prerequisite-closure loop
	// re-resolves T's status via TaskStatus, which re-hydrates the project
	// cache (dropping the hidden T) and falls back to a ref-mode lookup. A
	// plain MemoryProvider.GetTask also matches by ref, which would otherwise
	// bypass this adversarial setup by returning the REAL to-do task.
	notFoundT := fmt.Errorf("kaneo: GetTask %s: 404 not found", tTask.ID)
	p.failGetTask[tTask.ID] = notFoundT
	p.failGetTask[tTask.Ref] = notFoundT
	// Adversarial: correct id and project, but status=done -- not archived.
	p.archivedOverride = []*provider.Task{
		{ID: "id-t", Ref: "CHA-T", Status: "done", ProjectID: proj},
	}
	if _, err := p.CreateRelation(context.Background(), tTask.ID, bTask.ID, provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}
	store := NewProviderStore(p, proj)
	ctx := context.Background()
	desiredB := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-B", TaskID: TaskID(bTask.ID), Present: true,
		Edges: []DependencyEdge{{SourceRef: "CHA-T", TargetRef: "FAC-B", Type: EdgeBlocks}},
	}
	if _, err := RequireTaskLaunch(ctx, store, EntryDispatch, "FAC-B", desiredB, ""); err == nil {
		t.Fatalf("a status=done row from the archived query must not authorize the connected dependent task")
	}
}

// TestArchivedEvidence_RejectsForeignProjectRow reproduces: an exact-ID
// archived row whose own ProjectID names a different project must not
// authenticate a project-scoped snapshot.
func TestArchivedEvidence_RejectsForeignProjectRow(t *testing.T) {
	p := newEndpointFaultProvider()
	proj := "proj"
	tTask := &provider.Task{ID: "id-t", Ref: "CHA-T", Status: "to-do", ProjectID: proj, Title: "T"}
	bTask := &provider.Task{ID: "id-b", Ref: "FAC-B", Status: "to-do", ProjectID: proj, Title: "B"}
	p.AddTask(tTask)
	p.AddTask(bTask)
	p.hideFromUnfiltered[tTask.ID] = true
	p.failGetTask[tTask.ID] = fmt.Errorf("kaneo: GetTask %s: 404 not found", tTask.ID)
	// Adversarial: correct id and archived status, but the row's own
	// ProjectID names a different project than the query was scoped to.
	p.archivedOverride = []*provider.Task{
		{ID: "id-t", Ref: "CHA-T", Status: provider.StatusArchived, ProjectID: "other-project"},
	}
	if _, err := p.CreateRelation(context.Background(), tTask.ID, bTask.ID, provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}
	store := NewProviderStore(p, proj)
	ctx := context.Background()
	if _, err := store.SnapshotGraph(ctx); err == nil {
		t.Fatalf("a foreign-project row must not authenticate the full project snapshot")
	}
}

// TestArchivedEvidence_RejectsRefCollisionAgainstIDLookup reproduces: a row
// whose Ref happens to equal the immutable id being looked up, but whose own
// ID differs, must not authenticate an id-mode lookup (relation endpoints are
// always resolved by immutable id).
func TestArchivedEvidence_RejectsRefCollisionAgainstIDLookup(t *testing.T) {
	p := newEndpointFaultProvider()
	proj := "proj"
	xTask := &provider.Task{ID: "id-x", Ref: "CHA-X", Status: "to-do", ProjectID: proj, Title: "X"}
	bTask := &provider.Task{ID: "id-b", Ref: "FAC-B", Status: "to-do", ProjectID: proj, Title: "B"}
	p.AddTask(xTask)
	p.AddTask(bTask)
	p.hideFromUnfiltered[xTask.ID] = true
	p.failGetTask[xTask.ID] = fmt.Errorf("kaneo: GetTask %s: 404 not found", xTask.ID)
	// Adversarial: a DISTINCT row (own id "id-other") whose Ref collides with
	// the immutable id "id-x" actually being looked up.
	p.archivedOverride = []*provider.Task{
		{ID: "id-other", Ref: "id-x", Status: provider.StatusArchived, ProjectID: proj},
	}
	if _, err := p.CreateRelation(context.Background(), xTask.ID, bTask.ID, provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}
	store := NewProviderStore(p, proj)
	ctx := context.Background()
	if _, err := store.SnapshotGraph(ctx); err == nil {
		t.Fatalf("a ref collision must not authenticate an immutable-id lookup")
	}
}

// TestArchivedEvidence_RejectsAmbiguousDuplicateRows reproduces the "add a
// duplicate case" requirement: two DISTINCT archived rows both matching the
// same lookup key (same ref, different immutable ids) is conflicting
// evidence and must refuse -- never resolved by silently picking the first
// row.
func TestArchivedEvidence_RejectsAmbiguousDuplicateRows(t *testing.T) {
	p := newEndpointFaultProvider()
	proj := "proj"
	tTask := &provider.Task{ID: "id-t", Ref: "CHA-T", Status: "to-do", ProjectID: proj, Title: "T"}
	bTask := &provider.Task{ID: "id-b", Ref: "FAC-B", Status: "to-do", ProjectID: proj, Title: "B"}
	p.AddTask(tTask)
	p.AddTask(bTask)
	p.hideFromUnfiltered[tTask.ID] = true
	// Fail GetTask by both id and ref -- exercises the ref-mode lookup used
	// by SnapshotGraphForTask's desired-edge seeding (a plain MemoryProvider
	// GetTask falls back to matching by ref, which would otherwise bypass
	// this adversarial setup entirely).
	notFound := fmt.Errorf("kaneo: GetTask %s: 404 not found", tTask.ID)
	p.failGetTask[tTask.ID] = notFound
	p.failGetTask[tTask.Ref] = notFound
	// Adversarial: two conflicting archived rows for the same ref "CHA-T",
	// each with a different immutable id.
	p.archivedOverride = []*provider.Task{
		{ID: "id-t", Ref: "CHA-T", Status: provider.StatusArchived, ProjectID: proj},
		{ID: "id-t-dup", Ref: "CHA-T", Status: provider.StatusArchived, ProjectID: proj},
	}
	if _, err := p.CreateRelation(context.Background(), tTask.ID, bTask.ID, provider.RelationBlocks); err != nil {
		t.Fatalf("seed relation: %v", err)
	}
	store := NewProviderStore(p, proj)
	ctx := context.Background()
	desiredB := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-B", TaskID: TaskID(bTask.ID), Present: true,
		Edges: []DependencyEdge{{SourceRef: "CHA-T", TargetRef: "FAC-B", Type: EdgeBlocks}},
	}
	if _, err := RequireTaskLaunch(ctx, store, EntryDispatch, "FAC-B", desiredB, ""); err == nil {
		t.Fatalf("ambiguous/conflicting archived evidence must refuse, not pick the first row")
	}
}
