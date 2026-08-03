package deps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// LaunchEntrypoint names a production path that may create side effects.
// Every entrypoint must call ValidateLaunch before worktree/status/comment/tab.
type LaunchEntrypoint string

const (
	EntryDispatch  LaunchEntrypoint = "dispatch"
	EntryPulse     LaunchEntrypoint = "pulse"
	EntryWave      LaunchEntrypoint = "wave"
	EntryStanding  LaunchEntrypoint = "standing"
	EntryShot      LaunchEntrypoint = "shot"
	EntryRescue    LaunchEntrypoint = "rescue"
	EntryRecovery  LaunchEntrypoint = "recovery"
	EntryForge     LaunchEntrypoint = "forge"
	EntryClaim     LaunchEntrypoint = "claim"
)

// GateResult is the authoritative pre-side-effect decision for one task.
type GateResult struct {
	Ref            Ref              `json:"ref"`
	TaskID         TaskID           `json:"task_id"`
	Entrypoint     LaunchEntrypoint `json:"entrypoint"`
	OK             bool             `json:"ok"`
	BlockedBy      []string         `json:"blocked_by,omitempty"`
	GraphRevision  string           `json:"graph_revision"`
	Report         *ReconcileReport `json:"report,omitempty"`
	Provenance     *Provenance      `json:"provenance,omitempty"`
	StatusByBlocker map[string]string `json:"status_by_blocker,omitempty"`
}

// ValidateLaunch is the PRE-SIDE-EFFECT gate. Call before any worktree create,
// board status flip, comment, or agent tab. Fail-closed on capability gaps,
// drift, open/unknown blockers, cycles, or unreadable status.
//
// desired may be nil/empty: when empty, board inbound blocks alone govern
// eligibility (no packet drift check). When non-empty, reconcile must be clean.
//
// selectionRevision, when non-empty, is the graph revision observed at
// selection time; a mismatch means TOCTOU (concurrent relation change) → BLOCKED.
func ValidateLaunch(
	ctx context.Context,
	store RelationStore,
	entrypoint LaunchEntrypoint,
	taskRef Ref,
	desired *Provenance,
	selectionRevision string,
) (*GateResult, error) {
	if store == nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "capability",
			Reason: "relation store is nil",
		}
	}
	ok, err := store.SupportsRelations(ctx)
	if err != nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "capability",
			Reason: "relation capability unknown: " + err.Error(),
		}
	}
	if !ok {
		return nil, &BlockedError{
			Ref: taskRef, Code: "capability",
			Reason: ErrCapabilityUnsupported.Error(),
		}
	}

	taskRef = Ref(strings.TrimSpace(string(taskRef)))
	if !taskRef.Valid() {
		return nil, &BlockedError{Ref: taskRef, Code: "unresolved", Reason: "empty task ref"}
	}

	status, taskID, err := store.TaskStatus(ctx, taskRef)
	if err != nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "stale",
			Reason: "task unreadable: " + err.Error(),
		}
	}
	_ = status // status of dependent itself is not a blocker gate

	board, err := store.ListRelations(ctx, taskID)
	if err != nil {
		// List by ID failed — try ref string as some adapters accept refs.
		board, err = store.ListRelations(ctx, TaskID(taskRef))
	}
	if err != nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "stale",
			Reason: "list relations failed: " + err.Error(),
		}
	}

	// Normalize board edges: ensure refs when only IDs present is adapter duty.
	var desiredEdges []DependencyEdge
	if desired != nil {
		d := *desired
		d.TaskRef = taskRef
		d.TaskID = taskID
		d.Normalize()
		desiredEdges = d.DesiredBlocks()
	}

	rep := Reconcile(taskRef, desiredEdges, board)
	if !rep.OK {
		return &GateResult{
				Ref: taskRef, TaskID: taskID, Entrypoint: entrypoint,
				OK: false, Report: rep, GraphRevision: rep.GraphRevision,
			}, &BlockedError{
				Ref: taskRef, Code: "drift",
				Reason: "packet↔board dependency drift",
				Report: rep,
				Details: findingDetails(rep),
			}
	}

	// Open-blocker gate: only VERIFIED Done unlocks.
	blockers := InboundBlockers(board, taskRef)
	statusBy := map[string]string{}
	var open []string
	var details []string
	for _, b := range blockers {
		st, bid, berr := store.TaskStatus(ctx, b)
		if berr != nil {
			return nil, &BlockedError{
				Ref: taskRef, Code: "stale",
				Reason: fmt.Sprintf("blocker %s unreadable: %v", b, berr),
			}
		}
		st = provider.NormalizeStatus(st)
		statusBy[string(b)] = st
		if st != provider.StatusDone {
			open = append(open, string(b))
			details = append(details, fmt.Sprintf("blocker %s status=%s id=%s (need done)", b, st, bid))
		}
	}

	rev := GraphRevision(board, statusBy)
	rep.GraphRevision = rev

	if selectionRevision != "" && selectionRevision != rev {
		return &GateResult{
				Ref: taskRef, TaskID: taskID, Entrypoint: entrypoint,
				OK: false, Report: rep, GraphRevision: rev,
				BlockedBy: open, StatusByBlocker: statusBy,
			}, &BlockedError{
				Ref: taskRef, Code: "toctou",
				Reason: fmt.Sprintf("graph revision changed since selection (was %s now %s); concurrent relation mutation", selectionRevision, rev),
				Details: details,
				Report:  rep,
			}
	}

	if len(open) > 0 {
		return &GateResult{
				Ref: taskRef, TaskID: taskID, Entrypoint: entrypoint,
				OK: false, Report: rep, GraphRevision: rev,
				BlockedBy: open, StatusByBlocker: statusBy,
			}, &BlockedError{
				Ref: taskRef, Code: "open_blocker",
				Reason: fmt.Sprintf("blocked by non-done prerequisites: %s", strings.Join(open, ", ")),
				Details: details,
				Report:  rep,
			}
	}

	// Build authoritative provenance from board readback for packet/lifecycle.
	prov := &Provenance{
		Version:       SchemaVersion,
		TaskRef:       taskRef,
		TaskID:        taskID,
		Edges:         boardBlocksOnly(board),
		GraphRevision: rev,
		RecordedAt:    time.Now().UTC(),
	}
	if desired != nil {
		prov.Holds = desired.Holds
	}

	return &GateResult{
		Ref: taskRef, TaskID: taskID, Entrypoint: entrypoint,
		OK: true, Report: rep, GraphRevision: rev,
		Provenance: prov, StatusByBlocker: statusBy,
	}, nil
}

// ValidateClaim re-reads exact task + blockers inside the claim transition
// (TOCTOU close). selectionRevision is required for a fenced claim.
func ValidateClaim(ctx context.Context, store RelationStore, taskRef Ref, selectionRevision string) (*GateResult, error) {
	return ValidateLaunch(ctx, store, EntryClaim, taskRef, nil, selectionRevision)
}

// SelectEligibleRefs filters candidates by the launch gate, preserving
// priority DESC / ticket ASC order of the input slice (caller must pre-sort).
// Returns eligible refs + per-ref selection revisions for later claim binding.
func SelectEligibleRefs(
	ctx context.Context,
	store RelationStore,
	entrypoint LaunchEntrypoint,
	candidates []*provider.Task,
	desiredByRef map[string]*Provenance,
) (eligible []*provider.Task, revisions map[string]string, blocked []GateResult, err error) {
	revisions = map[string]string{}
	for _, t := range candidates {
		if t == nil {
			continue
		}
		var des *Provenance
		if desiredByRef != nil {
			des = desiredByRef[t.Ref]
		}
		gr, gerr := ValidateLaunch(ctx, store, entrypoint, Ref(t.Ref), des, "")
		if gerr != nil {
			var be *BlockedError
			if errors.As(gerr, &be) {
				// Capability / unreadable provider state fail closed for the
				// whole selection — never convert into "no candidates".
				switch be.Code {
				case "capability", "stale":
					return nil, nil, blocked, gerr
				}
				if gr != nil {
					blocked = append(blocked, *gr)
				} else {
					blocked = append(blocked, GateResult{Ref: Ref(t.Ref), OK: false})
				}
				continue
			}
			// Non-BlockedError hard failures fail the whole selection.
			return nil, nil, blocked, gerr
		}
		if gr == nil || !gr.OK {
			if gr != nil {
				blocked = append(blocked, *gr)
			}
			continue
		}
		eligible = append(eligible, t)
		revisions[t.Ref] = gr.GraphRevision
	}
	return eligible, revisions, blocked, nil
}

func boardBlocksOnly(board []DependencyEdge) []DependencyEdge {
	var out []DependencyEdge
	for _, e := range board {
		if e.Type == EdgeBlocks {
			out = append(out, e)
		}
	}
	return compactEdges(out)
}

func findingDetails(rep *ReconcileReport) []string {
	if rep == nil {
		return nil
	}
	var d []string
	for _, f := range rep.Findings {
		d = append(d, fmt.Sprintf("%s %s→%s %s", f.Class, f.Edge.SourceRef, f.Edge.TargetRef, f.Detail))
	}
	return d
}

// MarshalReport returns stable JSON for a reconcile report (CLI / fixtures).
func MarshalReport(rep *ReconcileReport) ([]byte, error) {
	if rep == nil {
		return []byte("null"), nil
	}
	return json.MarshalIndent(rep, "", "  ")
}

// IsBlocked reports whether err is a typed dependency block.
func IsBlocked(err error) bool {
	var be *BlockedError
	return errors.As(err, &be)
}
