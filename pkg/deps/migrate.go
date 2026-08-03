package deps

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// DescriptionWriter updates task descriptions (migration apply). Optional on providers.
type DescriptionWriter interface {
	SetDescription(ctx context.Context, taskID, description string) error
}

// MigratePlan is the dry-run/apply report for herd-deps-v1 backfill.
type MigratePlan struct {
	ProjectID string             `json:"project_id"`
	Items     []MigrateItem      `json:"items"`
	OK        bool               `json:"ok"`
	Errors    []string           `json:"errors,omitempty"`
}

// MigrateItem is one task's planned provenance backfill.
type MigrateItem struct {
	Ref           string `json:"ref"`
	TaskID        string `json:"task_id"`
	Status        string `json:"status"`
	Action        string `json:"action"` // skip_has_fence | write_empty | write_from_board | error
	AlreadyPresent bool  `json:"already_present"`
	EdgeCount     int    `json:"edge_count"`
	Detail        string `json:"detail,omitempty"`
	Applied       bool   `json:"applied,omitempty"`
	ReadbackOK    bool   `json:"readback_ok,omitempty"`
}

// PlanMigration builds a dry-run plan: every active (non-done) task gets an
// explicit versioned fence (from board blocks or empty). No Markdown inference.
func PlanMigration(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID string) (*MigratePlan, error) {
	plan := &MigratePlan{ProjectID: projectID, OK: true}
	if store == nil || tp == nil {
		return nil, fmt.Errorf("deps migrate: store and task provider required")
	}
	ok, err := store.SupportsRelations(ctx)
	if err != nil || !ok {
		return nil, fmt.Errorf("deps migrate: relation capability required: %v", err)
	}
	snap, err := store.SnapshotGraph(ctx)
	if err != nil {
		return nil, fmt.Errorf("deps migrate: snapshot: %w", err)
	}
	tasks, err := tp.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, err
	}
	sort.Slice(tasks, func(i, j int) bool {
		return provider.CompareRefs(tasks[i].Ref, tasks[j].Ref) < 0
	})
	for _, t := range tasks {
		if t == nil {
			continue
		}
		st := provider.NormalizeStatus(t.Status)
		if st == provider.StatusDone || st == provider.StatusArchived {
			continue
		}
		item := MigrateItem{Ref: t.Ref, TaskID: t.ID, Status: st}
		existing, xerr := ExtractProvenanceFromText(t.Description)
		if xerr != nil {
			item.Action = "error"
			item.Detail = xerr.Error()
			plan.OK = false
			plan.Errors = append(plan.Errors, t.Ref+": "+xerr.Error())
			plan.Items = append(plan.Items, item)
			continue
		}
		if existing != nil && existing.Present {
			if berr := existing.BindAndValidate(Ref(t.Ref), TaskID(t.ID)); berr != nil {
				item.Action = "error"
				item.Detail = "present fence fails bind: " + berr.Error()
				plan.OK = false
				plan.Errors = append(plan.Errors, t.Ref+": "+item.Detail)
			} else {
				item.Action = "skip_has_fence"
				item.AlreadyPresent = true
			}
			plan.Items = append(plan.Items, item)
			continue
		}
		// Build from board blocks involving this task (authoritative, not prose).
		involving := FilterInvolvingTask(snap.Edges, Ref(t.Ref), TaskID(t.ID))
		var edges []DependencyEdge
		for _, e := range involving {
			if e.Type == EdgeBlocks {
				edges = append(edges, DependencyEdge{
					SourceRef: e.SourceRef, TargetRef: e.TargetRef,
					SourceID: e.SourceID, TargetID: e.TargetID,
					Type: EdgeBlocks, RelationID: e.RelationID,
				})
			}
		}
		item.EdgeCount = len(edges)
		if len(edges) == 0 {
			item.Action = "write_empty"
		} else {
			item.Action = "write_from_board"
		}
		plan.Items = append(plan.Items, item)
	}
	return plan, nil
}

// ApplyMigration writes fences for planned items and readbacks each mutation.
// writer must implement DescriptionWriter (or *MemoryProvider via adapter).
func ApplyMigration(ctx context.Context, store RelationStore, tp provider.TaskProvider, projectID string, writer DescriptionWriter) (*MigratePlan, error) {
	if writer == nil {
		return nil, fmt.Errorf("deps migrate apply: DescriptionWriter required")
	}
	plan, err := PlanMigration(ctx, store, tp, projectID)
	if err != nil {
		return nil, err
	}
	snap, err := store.SnapshotGraph(ctx)
	if err != nil {
		return plan, err
	}
	for i := range plan.Items {
		item := &plan.Items[i]
		if item.Action != "write_empty" && item.Action != "write_from_board" {
			continue
		}
		t, gerr := tp.GetTask(ctx, item.TaskID)
		if gerr != nil || t == nil {
			item.Detail = "get task failed"
			plan.OK = false
			continue
		}
		involving := FilterInvolvingTask(snap.Edges, Ref(t.Ref), TaskID(t.ID))
		var edges []DependencyEdge
		for _, e := range involving {
			if e.Type == EdgeBlocks {
				edges = append(edges, DependencyEdge{
					SourceRef: e.SourceRef, TargetRef: e.TargetRef,
					SourceID: e.SourceID, TargetID: e.TargetID,
					Type: EdgeBlocks,
				})
			}
		}
		p := &Provenance{
			Version: SchemaVersion,
			TaskRef: Ref(t.Ref),
			TaskID:  TaskID(t.ID),
			Edges:   edges,
			Present: true,
		}
		newDesc, aerr := AppendOrReplaceFence(t.Description, p)
		if aerr != nil {
			item.Detail = aerr.Error()
			plan.OK = false
			continue
		}
		if err := writer.SetDescription(ctx, t.ID, newDesc); err != nil {
			item.Detail = "set description: " + err.Error()
			plan.OK = false
			continue
		}
		item.Applied = true
		// Readback.
		fresh, ferr := tp.GetTask(ctx, t.ID)
		if ferr != nil || fresh == nil {
			item.Detail = "readback get failed"
			plan.OK = false
			continue
		}
		got, xerr := ExtractProvenanceFromText(fresh.Description)
		if xerr != nil || got == nil || !got.Present {
			item.Detail = "readback extract failed"
			plan.OK = false
			continue
		}
		if berr := got.BindAndValidate(Ref(t.Ref), TaskID(t.ID)); berr != nil {
			item.Detail = "readback bind failed: " + berr.Error()
			plan.OK = false
			continue
		}
		item.ReadbackOK = true
	}
	return plan, nil
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
	// MemoryProvider stores by ID; mutate via UpdateStatus path is insufficient.
	// Use a small reflection-free approach: re-AddTask with new description.
	t.Description = description
	w.MP.AddTask(t)
	// Readback.
	got, err := w.MP.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if got.Description != description {
		return fmt.Errorf("memory description readback mismatch")
	}
	return nil
}

// KaneoDescriptionWriter updates description via kaneo CLI.
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
