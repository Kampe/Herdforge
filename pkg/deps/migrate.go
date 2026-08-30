package deps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// DescriptionWriter updates task descriptions. Coordinator-run migrate --apply
// uses this; workers must never invoke apply against live Kaneo.
type DescriptionWriter interface {
	SetDescription(ctx context.Context, taskID, description string) error
	GetDescription(ctx context.Context, taskID string) (string, error)
}

// MigratePlan is the dry-run/apply report for herd-deps-v1 backfill.
// Authority remains description fences + Kaneo relations only (no sidecar).
type MigratePlan struct {
	ProjectID        string        `json:"project_id"`
	ProviderRevision string        `json:"provider_revision"`
	Items            []MigrateItem `json:"items"`
	OK               bool          `json:"ok"`
	Errors           []string      `json:"errors,omitempty"`
	Mode             string        `json:"mode"` // dry-run | apply-description
	JournalPath      string        `json:"journal_path,omitempty"`
}

// MigrateItem is one task's planned provenance backfill.
type MigrateItem struct {
	Ref            string           `json:"ref"`
	TaskID         string           `json:"task_id"`
	Status         string           `json:"status"`
	Action         string           `json:"action"` // skip_fresh | repair_stale | write_empty | write_from_board | skip_container | error
	AlreadyPresent bool             `json:"already_present"`
	EdgeCount      int              `json:"edge_count"`
	IntendedEdges  []DependencyEdge `json:"intended_edges,omitempty"`
	Detail         string           `json:"detail,omitempty"`
	Applied        bool             `json:"applied,omitempty"`
	ReadbackOK     bool             `json:"readback_ok,omitempty"`
	SnapshotRev    string           `json:"snapshot_rev,omitempty"`
	RolledBack     bool             `json:"rolled_back,omitempty"`
}

// JournalEntry is a before-image for coordinator rollback after partial apply.
type JournalEntry struct {
	TaskID     string `json:"task_id"`
	Ref        string `json:"ref"`
	BeforeDesc string `json:"before_description"`
	AfterDesc  string `json:"after_description,omitempty"`
	AppliedAt  string `json:"applied_at,omitempty"`
	RolledBack bool   `json:"rolled_back,omitempty"`
}

// Journal is the per-apply rollback log (coordinator-owned file under .herd/).
type Journal struct {
	ProviderRevision string         `json:"provider_revision"`
	StartedAt        string         `json:"started_at"`
	Entries          []JournalEntry `json:"entries"`
}

const migrationProgressEvery = 10

// MigrationProgress receives one item as soon as it has been planned. The
// callback is deliberately per-card so long migrations expose forward
// progress instead of appearing idle while a project-wide snapshot runs.
type MigrationProgress func(item MigrateItem, processed, total int)

type scopedSnapshotter interface {
	SnapshotGraphForTask(context.Context, Ref, TaskID, []DependencyEdge) (*GraphSnapshot, error)
}

type migrationScopedSnapshotKey struct{}

func migrationScopedContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, migrationScopedSnapshotKey{}, true)
}

func migrationScopedSnapshot(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(migrationScopedSnapshotKey{}).(bool)
	return value
}

// PlanMigration builds a revision-fenced dry-run from a fresh snapshot.
// Description fences + board relations only. Stale fences (missing task_id,
// edge multiset mismatch) are repair_stale, not skip.
func PlanMigration(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID string) (*MigratePlan, error) {
	return PlanMigrationWithProgress(ctx, store, tp, projectID, nil)
}

// PlanMigrationForRef builds a migration plan for exactly one named task.
// Unlike the board-wide planner, it never lists active tasks and requires the
// store's scoped graph surface so a missing capability cannot fall back to a
// project-wide relation scan.
func PlanMigrationForRef(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID, taskRef string) (*MigratePlan, error) {
	return PlanMigrationForRefWithProgress(ctx, store, tp, projectID, taskRef, nil)
}

// PlanMigrationForRefWithProgress is the progress-reporting exact-task
// planner. Identity is resolved by the provider's exact GetTask surface, then
// verified again by immutable ID inside SnapshotGraphForTask.
func PlanMigrationForRefWithProgress(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID, taskRef string, progress MigrationProgress) (*MigratePlan, error) {
	plan := &MigratePlan{ProjectID: projectID, OK: true, Mode: "dry-run"}
	if store == nil || tp == nil {
		return nil, fmt.Errorf("deps migrate --ref: store and task provider required")
	}
	ref := strings.TrimSpace(taskRef)
	if ref == "" {
		return nil, fmt.Errorf("deps migrate --ref: task ref required")
	}
	ok, err := store.SupportsRelations(ctx)
	if err != nil || !ok {
		return nil, fmt.Errorf("deps migrate --ref %s: relation capability required: %v", ref, err)
	}
	scoped, ok := store.(scopedSnapshotter)
	if !ok {
		return nil, fmt.Errorf("deps migrate --ref %s: scoped relation snapshot capability required", ref)
	}

	task, err := resolveMigrationTask(ctx, tp, projectID, ref, "")
	if err != nil {
		return nil, err
	}
	ctx, _ = WithSnapshotFence(ctx)
	snap, err := snapshotForTaskMigration(ctx, scoped, task)
	if err != nil {
		return nil, fmt.Errorf("deps migrate --ref %s: snapshot: %w", ref, err)
	}
	plan.ProviderRevision = snap.ProviderRevision
	item := planOne(task, snap)
	if item.Action == "error" {
		plan.OK = false
		plan.Errors = append(plan.Errors, task.Ref+": "+item.Detail)
	}
	plan.Items = append(plan.Items, item)
	if progress != nil {
		progress(item, 1, 1)
	}
	return plan, nil
}

func resolveMigrationTask(ctx context.Context, tp provider.TaskProvider, projectID, taskRef, taskID string) (*provider.Task, error) {
	lookup := strings.TrimSpace(taskID)
	if lookup == "" {
		lookup = strings.TrimSpace(taskRef)
	}
	if lookup == "" {
		return nil, fmt.Errorf("deps migrate --ref: task identity required")
	}
	task, err := tp.GetTask(ctx, lookup)
	if err != nil {
		return nil, fmt.Errorf("deps migrate --ref %s: get exact task: %w", strings.TrimSpace(taskRef), err)
	}
	if task == nil {
		return nil, fmt.Errorf("deps migrate --ref %s: exact task is missing", strings.TrimSpace(taskRef))
	}
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Ref) == "" {
		return nil, fmt.Errorf("deps migrate --ref %s: exact task identity is incomplete (id=%q ref=%q)", strings.TrimSpace(taskRef), task.ID, task.Ref)
	}
	if taskID != "" && task.ID != taskID {
		return nil, fmt.Errorf("deps migrate --ref %s: task id mismatch: requested %q got %q", strings.TrimSpace(taskRef), taskID, task.ID)
	}
	if taskRef != "" && !strings.EqualFold(strings.TrimSpace(task.Ref), strings.TrimSpace(taskRef)) {
		return nil, fmt.Errorf("deps migrate --ref %s: task ref mismatch: got %q", strings.TrimSpace(taskRef), task.Ref)
	}
	if strings.TrimSpace(projectID) != "" && task.ProjectID != projectID {
		return nil, fmt.Errorf("deps migrate --ref %s: task project mismatch: want %q got %q", strings.TrimSpace(taskRef), projectID, task.ProjectID)
	}
	status := provider.NormalizeStatus(task.Status)
	if status == provider.StatusDone || status == provider.StatusArchived {
		return nil, fmt.Errorf("deps migrate --ref %s: terminal task status %q is not eligible", strings.TrimSpace(taskRef), status)
	}
	if status == provider.StatusUnknown || strings.HasPrefix(status, provider.StatusUnknown+":") {
		return nil, fmt.Errorf("deps migrate --ref %s: unreadable task status %q", strings.TrimSpace(taskRef), task.Status)
	}
	return task, nil
}

// PlanMigrationWithProgress builds a migration plan without requiring a
// whole-project relation snapshot when the adapter supports scoped reads.
// The returned plan is non-nil when work was completed before a timeout; the
// error remains non-nil so callers cannot mistake partial output for success.
func PlanMigrationWithProgress(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID string, progress MigrationProgress) (*MigratePlan, error) {
	plan := &MigratePlan{ProjectID: projectID, OK: true, Mode: "dry-run"}
	if store == nil || tp == nil {
		return nil, fmt.Errorf("deps migrate: store and task provider required")
	}
	ok, err := store.SupportsRelations(ctx)
	if err != nil || !ok {
		return nil, fmt.Errorf("deps migrate: relation capability required: %v", err)
	}
	ctx, _ = WithSnapshotFence(ctx)
	// Active columns only: every terminal card is discarded just below, and the
	// board's done column alone exceeded the list deadline.
	tasks, err := provider.ListActiveTasks(ctx, tp, projectID)
	if err != nil {
		return nil, err
	}
	sort.Slice(tasks, func(i, j int) bool {
		return provider.CompareRefs(tasks[i].Ref, tasks[j].Ref) < 0
	})
	active := make([]*provider.Task, 0, len(tasks))
	for _, t := range tasks {
		if t != nil && provider.NormalizeStatus(t.Status) != provider.StatusDone && provider.NormalizeStatus(t.Status) != provider.StatusArchived {
			active = append(active, t)
		}
	}
	scoped, hasScoped := store.(scopedSnapshotter)
	var fatalErr error
	for i, t := range active {
		var snap *GraphSnapshot
		if hasScoped {
			snap, err = snapshotForTaskMigration(ctx, scoped, t)
		} else {
			// Compatibility fallback for stores without the scoped adapter. Live
			// ProviderStore implements SnapshotGraphForTask; this path is for
			// in-process/test stores and legacy adapters only.
			snap, err = snapshotForMigration(ctx, store)
		}
		if err != nil {
			item := MigrateItem{Ref: t.Ref, TaskID: t.ID, Status: provider.NormalizeStatus(t.Status), Action: "error", Detail: "snapshot: " + err.Error()}
			plan.OK = false
			plan.Errors = append(plan.Errors, t.Ref+": "+item.Detail)
			plan.Items = append(plan.Items, item)
			if progress != nil {
				progress(item, i+1, len(active))
			}
			if len(plan.Items) == 1 {
				return nil, fmt.Errorf("deps migrate: snapshot: %w", err)
			}
			fatalErr = fmt.Errorf("deps migrate: snapshot for %s: %w", t.Ref, err)
			break
		}
		if plan.ProviderRevision == "" {
			plan.ProviderRevision = snap.ProviderRevision
		}
		item := planOne(t, snap)
		if item.Action == "error" {
			plan.OK = false
			plan.Errors = append(plan.Errors, t.Ref+": "+item.Detail)
		}
		plan.Items = append(plan.Items, item)
		if progress != nil {
			progress(item, i+1, len(active))
		}
	}
	return plan, fatalErr
}

func snapshotForTaskMigration(ctx context.Context, store scopedSnapshotter, task *provider.Task) (*GraphSnapshot, error) {
	const maxAttempts = 2
	var snap *GraphSnapshot
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		snap, err = store.SnapshotGraphForTask(migrationScopedContext(ctx), Ref(task.Ref), TaskID(task.ID), nil)
		if err == nil || provider.ClassifyOpError(err) != provider.OpTimeout || attempt == maxAttempts {
			return snap, err
		}
		timer := time.NewTimer(migrationSnapshotRetryBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return snap, err
}

const migrationSnapshotRetryBackoff = 100 * time.Millisecond

// snapshotForMigration tolerates one transient provider read timeout. A
// second failed snapshot remains a hard error: migration must never infer an
// empty or partial graph from an unavailable relation surface.
func snapshotForMigration(ctx context.Context, store RelationStore) (*GraphSnapshot, error) {
	const maxAttempts = 2
	var snap *GraphSnapshot
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		snap, err = store.SnapshotGraph(ctx)
		if err == nil || provider.ClassifyOpError(err) != provider.OpTimeout {
			return snap, err
		}
		if attempt == maxAttempts {
			break
		}
		timer := time.NewTimer(migrationSnapshotRetryBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, err
}

func planOne(t *provider.Task, snap *GraphSnapshot) MigrateItem {
	item := MigrateItem{
		Ref: t.Ref, TaskID: t.ID, Status: provider.NormalizeStatus(t.Status),
		SnapshotRev: snap.ProviderRevision,
	}
	intended := intendedBoardEdges(snap.Edges, Ref(t.Ref), TaskID(t.ID))
	item.IntendedEdges = intended
	item.EdgeCount = len(intended)

	existing, xerr := ExtractProvenanceFromText(t.Description)
	if xerr != nil {
		item.Action = "repair_stale"
		item.Detail = xerr.Error()
		return item
	}
	if existing != nil && existing.Present {
		if berr := existing.BindAndValidate(Ref(t.Ref), TaskID(t.ID)); berr != nil {
			item.Action = "repair_stale"
			item.Detail = "bind: " + berr.Error()
			return item
		}
		got, gerr := existing.DesiredBlocks()
		if gerr != nil {
			item.Action = "repair_stale"
			item.Detail = gerr.Error()
			return item
		}
		if !EdgeMultisetEqual(got, intended) {
			item.Action = "repair_stale"
			item.Detail = "fence edge multiset != current board blocks"
			return item
		}
		item.Action = "skip_fresh"
		item.AlreadyPresent = true
		return item
	}
	if len(intended) == 0 {
		if isContainerCard(t.Labels) {
			// FAC-458: a container/epic-scoping card is unclaimable by
			// design; writing an empty herd-deps-v1 fence onto it removes
			// the "missing structured provenance" refusal that keeps it
			// unclaimable, silently unblocking governance-critical or
			// hard-invariant-adjacent epics. Skip instead of writing.
			item.Action = "skip_container"
			return item
		}
		item.Action = "write_empty"
	} else {
		item.Action = "write_from_board"
	}
	return item
}

// isContainerCard reports whether labels mark a card as intentionally
// unclaimable by design (children get claimed individually; the container
// itself never should be). Mirrors pkg/lifecycle's isBounded label set.
func isContainerCard(labels []string) bool {
	for _, l := range labels {
		switch l {
		case "epic", "epic-needs-scoping", "standing-epic":
			return true
		}
	}
	return false
}

func intendedBoardEdges(all []DependencyEdge, ref Ref, id TaskID) []DependencyEdge {
	var edges []DependencyEdge
	for _, e := range FilterInvolvingTask(all, ref, id) {
		if e.Type == EdgeBlocks {
			edges = append(edges, DependencyEdge{
				SourceRef: e.SourceRef, TargetRef: e.TargetRef,
				SourceID: e.SourceID, TargetID: e.TargetID,
				Type: EdgeBlocks, RelationID: e.RelationID,
			})
		}
	}
	return edges
}

// EdgeMultisetEqual compares blocks edges as a multiset (counts duplicates).
// Set semantics are intentionally rejected — duplicate authority must not hide.
func EdgeMultisetEqual(a, b []DependencyEdge) bool {
	ca := edgeCounts(a)
	cb := edgeCounts(b)
	if len(ca) != len(cb) {
		return false
	}
	for k, n := range ca {
		if cb[k] != n {
			return false
		}
	}
	return true
}

func edgeCounts(edges []DependencyEdge) map[string]int {
	m := map[string]int{}
	for _, e := range edges {
		if e.Type != EdgeBlocks {
			continue
		}
		m[e.Key()]++
	}
	return m
}

// ApplyMigration is coordinator-run only. Per-card: fresh snapshot, journal
// before-image, write description fence, semantic multiset readback; on
// failure after write, restore before-image from journal. Workers must not
// invoke this against live Kaneo (CLI refuses apply without HERD_DEPS_MIGRATE_APPLY=1
// from the coordinator harness; tests use memory DescriptionWriter).
func ApplyMigration(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID string, writer DescriptionWriter, journalDir string) (*MigratePlan, error) {
	return applyMigration(ctx, store, tp, projectID, "", writer, journalDir)
}

// ApplyMigrationForRef applies migration to exactly one named task. It
// re-plans that ref from a fresh scoped snapshot, journals its before-image,
// and performs both description and provider-revision readback before success.
func ApplyMigrationForRef(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID, taskRef string, writer DescriptionWriter, journalDir string) (*MigratePlan, error) {
	ref := strings.TrimSpace(taskRef)
	if ref == "" {
		return nil, fmt.Errorf("deps migrate --ref apply: task ref required")
	}
	return applyMigration(ctx, store, tp, projectID, ref, writer, journalDir)
}

func applyMigration(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID, taskRef string, writer DescriptionWriter, journalDir string) (*MigratePlan, error) {
	if writer == nil {
		return nil, fmt.Errorf("deps migrate apply: DescriptionWriter required (description fences are authority; no sidecar)")
	}
	ctx, _ = WithSnapshotFence(ctx)
	plan := &MigratePlan{ProjectID: projectID, OK: true, Mode: "apply-description"}
	var base *MigratePlan
	var err error
	if taskRef == "" {
		base, err = PlanMigration(ctx, store, tp, projectID)
	} else {
		base, err = PlanMigrationForRef(ctx, store, tp, projectID, taskRef)
	}
	if err != nil {
		return nil, err
	}
	plan.ProviderRevision = base.ProviderRevision

	journal := Journal{
		ProviderRevision: base.ProviderRevision,
		StartedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	if journalDir == "" {
		journalDir = filepath.Join(".herd", "migrate-journal")
	}
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		return nil, err
	}
	jPath := filepath.Join(journalDir, fmt.Sprintf("apply-%d.json", time.Now().UnixNano()))
	plan.JournalPath = jPath

	for processed, planned := range base.Items {
		processed++
		if processed == 1 || processed%migrationProgressEvery == 0 || processed == len(base.Items) {
			fmt.Fprintf(os.Stderr, "herd deps migrate apply: processed %d/%d cards (current=%s)\n", processed, len(base.Items), planned.Ref)
		}
		item := planned
		if item.Action != "write_empty" && item.Action != "write_from_board" && item.Action != "repair_stale" {
			plan.Items = append(plan.Items, item)
			continue
		}
		// Fresh scoped snapshot per card (revision fence). Live providers must
		// not re-fan-out the whole project for every apply item either.
		var snap *GraphSnapshot
		var serr error
		if scoped, ok := store.(scopedSnapshotter); ok {
			snap, serr = snapshotForTaskMigration(ctx, scoped, &provider.Task{ID: item.TaskID, Ref: item.Ref})
		} else {
			snap, serr = snapshotForMigration(ctx, store)
		}
		if serr != nil {
			item.Action = "error"
			item.Detail = "fresh snapshot: " + serr.Error()
			plan.OK = false
			plan.Errors = append(plan.Errors, item.Ref+": "+item.Detail)
			plan.Items = append(plan.Items, item)
			continue
		}
		if plan.ProviderRevision != "" && snap.ProviderRevision != plan.ProviderRevision {
			item.Detail = "provider revision moved mid-apply; re-planned from fresh snapshot"
		}
		t, gerr := tp.GetTask(ctx, item.TaskID)
		if taskRef != "" {
			t, gerr = resolveMigrationTask(ctx, tp, projectID, taskRef, item.TaskID)
		}
		if gerr != nil || t == nil {
			item.Action = "error"
			item.Detail = "get task failed"
			if gerr != nil {
				item.Detail += ": " + gerr.Error()
			}
			plan.OK = false
			plan.Errors = append(plan.Errors, item.Ref+": "+item.Detail)
			plan.Items = append(plan.Items, item)
			continue
		}
		if taskRef != "" {
			// The exact-ref apply path re-plans from the fresh snapshot rather
			// than carrying a stale action or description decision forward.
			item = planOne(t, snap)
			if item.Action == "error" {
				plan.OK = false
				plan.Errors = append(plan.Errors, item.Ref+": "+item.Detail)
				plan.Items = append(plan.Items, item)
				continue
			}
			if item.Action != "write_empty" && item.Action != "write_from_board" && item.Action != "repair_stale" {
				plan.Items = append(plan.Items, item)
				continue
			}
			plan.ProviderRevision = snap.ProviderRevision
			journal.ProviderRevision = snap.ProviderRevision
		}
		before, berr := writer.GetDescription(ctx, t.ID)
		if berr != nil {
			// Fallback to task description field.
			before = t.Description
		}
		intended := intendedBoardEdges(snap.Edges, Ref(t.Ref), TaskID(t.ID))
		p := &Provenance{
			Version:          SchemaVersion,
			TaskRef:          Ref(t.Ref),
			TaskID:           TaskID(t.ID),
			Edges:            intended,
			Present:          true,
			ProviderRevision: snap.ProviderRevision,
			GraphRevision:    GraphRevision(snap.Edges, nil, snap.ProviderRevision),
		}
		item.IntendedEdges = intended
		item.EdgeCount = len(intended)
		item.SnapshotRev = snap.ProviderRevision

		newDesc, aerr := AppendOrReplaceFence(before, p)
		if aerr != nil {
			item.Action = "error"
			item.Detail = aerr.Error()
			plan.OK = false
			plan.Items = append(plan.Items, item)
			continue
		}

		// Journal before-image before mutation.
		je := JournalEntry{TaskID: t.ID, Ref: t.Ref, BeforeDesc: before}
		journal.Entries = append(journal.Entries, je)
		if jerr := writeJournal(jPath, journal); jerr != nil && taskRef != "" {
			item.Action = "error"
			item.Detail = "journal before-image: " + jerr.Error()
			plan.OK = false
			plan.Errors = append(plan.Errors, item.Ref+": "+item.Detail)
			plan.Items = append(plan.Items, item)
			continue
		}

		if err := writer.SetDescription(ctx, t.ID, newDesc); err != nil {
			item.Action = "error"
			item.Detail = "set description: " + err.Error()
			plan.OK = false
			plan.Errors = append(plan.Errors, item.Ref+": "+item.Detail)
			plan.Items = append(plan.Items, item)
			continue
		}
		item.Applied = true
		journal.Entries[len(journal.Entries)-1].AfterDesc = newDesc
		journal.Entries[len(journal.Entries)-1].AppliedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if jerr := writeJournal(jPath, journal); jerr != nil && taskRef != "" {
			item.Detail = "journal applied image: " + jerr.Error()
			if rbErr := writer.SetDescription(ctx, t.ID, before); rbErr != nil {
				item.Detail += "; rollback failed: " + rbErr.Error()
			} else {
				item.RolledBack = true
			}
			plan.OK = false
			plan.Errors = append(plan.Errors, item.Ref+": "+item.Detail)
			plan.Items = append(plan.Items, item)
			continue
		}

		// Semantic readback: Present + bind + exact multiset edges.
		after, rerr := writer.GetDescription(ctx, t.ID)
		if rerr != nil {
			after = ""
		}
		if after == "" {
			// re-get task
			if fresh, ferr := tp.GetTask(ctx, t.ID); ferr == nil && fresh != nil {
				after = fresh.Description
			}
		}
		got, xerr := ExtractProvenanceFromText(after)
		okRB := false
		if xerr != nil {
			item.Detail = "readback extract: " + xerr.Error()
		} else if got == nil || !got.Present {
			item.Detail = "readback missing fence"
		} else if berr := got.BindAndValidate(Ref(t.Ref), TaskID(t.ID)); berr != nil {
			item.Detail = "readback bind: " + berr.Error()
		} else if gotEdges, gerr := got.DesiredBlocks(); gerr != nil {
			item.Detail = "readback desired: " + gerr.Error()
		} else if !EdgeMultisetEqual(gotEdges, intended) {
			item.Detail = "readback edge multiset != intended board edges"
		} else {
			okRB = true
			item.ReadbackOK = true
		}
		if okRB && taskRef != "" {
			scoped, scopedOK := store.(scopedSnapshotter)
			if !scopedOK {
				okRB = false
				item.ReadbackOK = false
				item.Detail = "provider readback: scoped relation snapshot capability missing"
			} else if confirm, cerr := snapshotForTaskMigration(ctx, scoped, t); cerr != nil {
				okRB = false
				item.ReadbackOK = false
				item.Detail = "provider readback: " + cerr.Error()
			} else if confirm.ProviderRevision != snap.ProviderRevision {
				okRB = false
				item.ReadbackOK = false
				item.Detail = fmt.Sprintf("provider readback revision moved: planned=%s readback=%s", snap.ProviderRevision, confirm.ProviderRevision)
			} else if confirmEdges := intendedBoardEdges(confirm.Edges, Ref(t.Ref), TaskID(t.ID)); !EdgeMultisetEqual(confirmEdges, intended) {
				okRB = false
				item.ReadbackOK = false
				item.Detail = "provider readback edge multiset != intended board edges"
			}
		}
		if !okRB {
			// Rollback this card from journal before-image.
			if rbErr := writer.SetDescription(ctx, t.ID, before); rbErr != nil {
				item.Detail += "; rollback failed: " + rbErr.Error()
			} else {
				item.RolledBack = true
				journal.Entries[len(journal.Entries)-1].RolledBack = true
				if jerr := writeJournal(jPath, journal); jerr != nil && taskRef != "" {
					item.Detail += "; rollback journal: " + jerr.Error()
				}
			}
			plan.OK = false
			plan.Errors = append(plan.Errors, item.Ref+": "+item.Detail)
		}
		plan.Items = append(plan.Items, item)
	}
	return plan, nil
}

func writeJournal(path string, j Journal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MemoryDescriptionWriter adapts MemoryProvider for tests.
type MemoryDescriptionWriter struct {
	MP *provider.MemoryProvider
}

func (w MemoryDescriptionWriter) SetDescription(ctx context.Context, taskID, description string) error {
	if w.MP == nil {
		return fmt.Errorf("nil memory provider")
	}
	t, err := w.MP.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	t.Description = description
	w.MP.AddTask(t)
	got, err := w.MP.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if got.Description != description {
		return fmt.Errorf("memory description readback mismatch")
	}
	return nil
}

func (w MemoryDescriptionWriter) GetDescription(ctx context.Context, taskID string) (string, error) {
	if w.MP == nil {
		return "", fmt.Errorf("nil memory provider")
	}
	t, err := w.MP.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	return t.Description, nil
}

// KaneoDescriptionWriter updates description via kaneo CLI (coordinator only).
type KaneoDescriptionWriter struct {
	ProjectID string
	Run       func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (w KaneoDescriptionWriter) SetDescription(ctx context.Context, taskID, description string) error {
	run := w.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			res, err := provider.RunCLI(ctx, name, args...)
			if res == nil {
				return nil, err
			}
			return res.Stdout, err
		}
	}
	args := []string{"task", "description", taskID, description}
	if strings.TrimSpace(w.ProjectID) != "" {
		args = append(args, "--project", w.ProjectID)
	}
	_, err := run(ctx, "kaneo", args...)
	return err
}

func (w KaneoDescriptionWriter) GetDescription(ctx context.Context, taskID string) (string, error) {
	// Caller should use TaskProvider.GetTask; this is a minimal CLI get.
	run := w.Run
	if run == nil {
		return "", fmt.Errorf("kaneo get description: no runner; use TaskProvider.GetTask")
	}
	args := []string{"task", "get", taskID, "--json"}
	if strings.TrimSpace(w.ProjectID) != "" {
		args = append(args, "--project", w.ProjectID)
	}
	out, err := run(ctx, "kaneo", args...)
	if err != nil {
		return "", err
	}
	var dto struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &dto); err != nil {
		return "", err
	}
	return dto.Description, nil
}
