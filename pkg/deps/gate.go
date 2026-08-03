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
	EntryDispatch LaunchEntrypoint = "dispatch"
	EntryPulse    LaunchEntrypoint = "pulse"
	EntryWave     LaunchEntrypoint = "wave"
	EntryStanding LaunchEntrypoint = "standing"
	EntryShot     LaunchEntrypoint = "shot"
	EntryRescue   LaunchEntrypoint = "rescue"
	EntryRecovery LaunchEntrypoint = "recovery"
	EntryForge    LaunchEntrypoint = "forge"
	EntryClaim    LaunchEntrypoint = "claim"
)

// GateResult is the authoritative pre-side-effect decision for one task.
type GateResult struct {
	Ref              Ref              `json:"ref"`
	TaskID           TaskID           `json:"task_id"`
	Entrypoint       LaunchEntrypoint `json:"entrypoint"`
	OK               bool             `json:"ok"`
	BlockedBy        []string         `json:"blocked_by,omitempty"`
	GraphRevision    string           `json:"graph_revision"`
	ProviderRevision string           `json:"provider_revision,omitempty"`
	Report           *ReconcileReport `json:"report,omitempty"`
	Provenance       *Provenance      `json:"provenance,omitempty"`
	StatusByBlocker  map[string]string `json:"status_by_blocker,omitempty"`
}

// hardSelectionCodes fail the whole SelectEligibleRefs run (never "zero eligible").
var hardSelectionCodes = map[string]bool{
	"capability": true,
	"stale":      true,
	"unresolved": true, // store-level when task itself unreadable mid-select
}

// ValidateLaunch is the PRE-SIDE-EFFECT gate. Call before any worktree create,
// board status flip, comment, or agent tab.
//
// desired MUST be a Present versioned provenance record. Empty/missing
// provenance is never treated as OK.
//
// selectionRevision, when non-empty, is the graph revision observed at
// selection time; a mismatch means TOCTOU → BLOCKED.
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

	// Provenance is mandatory for launch eligibility.
	if desired == nil || !desired.Present {
		return nil, &BlockedError{
			Ref: taskRef, Code: "missing_provenance",
			Reason: ErrMissingProvenance.Error() + "; attach versioned herd-deps-v1 record",
		}
	}

	status, taskID, err := store.TaskStatus(ctx, taskRef)
	if err != nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "stale",
			Reason: "task unreadable: " + err.Error(),
		}
	}

	// Bind fence to this exact task (reject replay; require immutable task_id).
	if err := desired.BindAndValidate(taskRef, taskID); err != nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "missing_provenance",
			Reason: err.Error(),
		}
	}

	// Authoritative full project closure for cycle detection + revision.
	// Fence reuses the first snapshot; post-side-effect checks do NOT re-fanout
	// the whole board (AssertIncidentEdgesFresh is O(1) on the target).
	snap, err := store.SnapshotGraph(ctx)
	if err != nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "stale",
			Reason: "full graph snapshot failed: " + err.Error(),
		}
	}
	if snap == nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "stale",
			Reason: "full graph snapshot returned nil",
		}
	}

	// Post-selection TOCTOU on the launch target: one ListRelations, not N.
	if selectionRevision != "" {
		if ps, ok := store.(*ProviderStore); ok {
			if ferr := ps.AssertIncidentEdgesFresh(ctx, taskRef, taskID, snap); ferr != nil {
				return nil, &BlockedError{
					Ref: taskRef, Code: "toctou",
					Reason: ferr.Error(),
				}
			}
		}
	}

	// Per-task board edges by ref AND immutable ID (never drop ID-only matches).
	board := FilterInvolvingTask(snap.Edges, taskRef, taskID)

	desiredEdges, derr := desired.DesiredBlocks()
	if derr != nil {
		return nil, &BlockedError{
			Ref: taskRef, Code: "missing_provenance",
			Reason: derr.Error(),
		}
	}

	rep := Reconcile(taskRef, desiredEdges, board, ReconcileOpts{
		FullClosure:        snap.Edges,
		ProviderRevision:   snap.ProviderRevision,
		RequireFullClosure: true,
	})
	if !rep.OK {
		return &GateResult{
				Ref: taskRef, TaskID: taskID, Entrypoint: entrypoint,
				OK: false, Report: rep, GraphRevision: rep.GraphRevision,
				ProviderRevision: snap.ProviderRevision,
			}, &BlockedError{
				Ref: taskRef, Code: "drift",
				Reason:  "packet↔board dependency drift",
				Report:  rep,
				Details: findingDetails(rep),
			}
	}

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

	// Relation-graph revision only (edges + blocker statuses). Target task
	// status is ownership via claim lease generation — including ToDo→InProgress
	// here would make every successful post-claim check mismatch deterministically.
	_ = status
	_ = taskID
	rev := GraphRevision(snap.Edges, statusBy, snap.ProviderRevision)
	rep.GraphRevision = rev
	rep.ProviderRevision = snap.ProviderRevision

	if selectionRevision != "" && selectionRevision != rev {
		return &GateResult{
				Ref: taskRef, TaskID: taskID, Entrypoint: entrypoint,
				OK: false, Report: rep, GraphRevision: rev,
				ProviderRevision: snap.ProviderRevision,
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
				ProviderRevision: snap.ProviderRevision,
				BlockedBy: open, StatusByBlocker: statusBy,
			}, &BlockedError{
				Ref: taskRef, Code: "open_blocker",
				Reason:  fmt.Sprintf("blocked by non-done prerequisites: %s", strings.Join(open, ", ")),
				Details: details,
				Report:  rep,
			}
	}

	prov := &Provenance{
		Version:          SchemaVersion,
		TaskRef:          taskRef,
		TaskID:           taskID,
		Edges:            boardBlocksOnly(board),
		Holds:            desired.Holds,
		GraphRevision:    rev,
		ProviderRevision: snap.ProviderRevision,
		RecordedAt:       time.Now().UTC(),
		Present:          true,
	}

	return &GateResult{
		Ref: taskRef, TaskID: taskID, Entrypoint: entrypoint,
		OK: true, Report: rep, GraphRevision: rev,
		ProviderRevision: snap.ProviderRevision,
		Provenance: prov, StatusByBlocker: statusBy,
	}, nil
}

// ValidateClaim re-reads exact task + blockers bound to selectionRevision.
// selectionRevision is REQUIRED (fenced claim) — empty is ErrClaimFence / TOCTOU.
// desired may be nil only when the claim path reuses selection-time provenance
// already validated; when nil, EmptyProvenance is NOT assumed — missing fails.
func ValidateClaim(ctx context.Context, store RelationStore, taskRef Ref, desired *Provenance, selectionRevision string) (*GateResult, error) {
	if strings.TrimSpace(selectionRevision) == "" {
		return nil, &BlockedError{
			Ref: taskRef, Code: "toctou",
			Reason: ErrClaimFence.Error() + "; bind selection graph/provider revision before claim",
		}
	}
	return ValidateLaunch(ctx, store, EntryClaim, taskRef, desired, selectionRevision)
}

// ClaimCompensateFunc undoes a claim mutation when post-claim graph drift is detected.
type ClaimCompensateFunc func(ctx context.Context, taskID TaskID, reason string) error

// FencedClaim runs pre-claim gate, claimFn, then post-claim re-validation.
// On post-claim drift, compensateFn is invoked (fail-closed if compensation fails).
// Atomic claim+graph is unavailable on most providers; this is the TOCTOU close.
// selectionRevision is REQUIRED (no optional/no-op fence).
func FencedClaim(
	ctx context.Context,
	store RelationStore,
	taskRef Ref,
	taskID TaskID,
	desired *Provenance,
	selectionRevision string,
	claimFn func(ctx context.Context) error,
	compensateFn ClaimCompensateFunc,
) (*GateResult, error) {
	if strings.TrimSpace(selectionRevision) == "" {
		return nil, &BlockedError{
			Ref: taskRef, Code: "toctou",
			Reason: ErrClaimFence.Error() + "; selection revision required",
		}
	}
	if claimFn == nil {
		return nil, fmt.Errorf("deps: claimFn required")
	}
	if compensateFn == nil {
		return nil, fmt.Errorf("deps: compensateFn required (no no-op fence)")
	}
	pre, err := ValidateClaim(ctx, store, taskRef, desired, selectionRevision)
	if err != nil {
		return pre, err
	}
	if err := claimFn(ctx); err != nil {
		return pre, err
	}
	// Post-claim: revision must still match; open blockers still closed.
	post, perr := ValidateClaim(ctx, store, taskRef, desired, pre.GraphRevision)
	if perr != nil {
		reason := "post_claim_graph_drift"
		if cErr := compensateFn(ctx, taskID, reason); cErr != nil {
			return post, fmt.Errorf("%w: %v; compensate failed: %w", ErrPostClaimDrift, perr, cErr)
		}
		return post, fmt.Errorf("%w: %v", ErrPostClaimDrift, perr)
	}
	return post, nil
}

// FencedLaunch binds selection→side-effect→post-check for dispatch (and any
// path that mutates worktree/status without a separate claim lease).
//
//  1. ValidateLaunch (selection) → revision R
//  2. ValidateLaunch with R (re-read before first side effect)
//  3. sideEffectFn
//  4. ValidateLaunch with R again; on drift run compensateFn
//
// selectionRevision may be empty on step 1; step 2 always binds R. No optional fence.
func FencedLaunch(
	ctx context.Context,
	store RelationStore,
	entrypoint LaunchEntrypoint,
	taskRef Ref,
	taskID TaskID,
	desired *Provenance,
	sideEffectFn func(ctx context.Context, pre *GateResult) error,
	compensateFn ClaimCompensateFunc,
) (*GateResult, error) {
	if sideEffectFn == nil {
		return nil, fmt.Errorf("deps: sideEffectFn required")
	}
	if compensateFn == nil {
		return nil, fmt.Errorf("deps: compensateFn required (no no-op fence)")
	}
	// Selection snapshot.
	sel, err := ValidateLaunch(ctx, store, entrypoint, taskRef, desired, "")
	if err != nil {
		return sel, err
	}
	// Re-read immediately before first side effect, bound to selection revision.
	pre, err := ValidateLaunch(ctx, store, entrypoint, taskRef, desired, sel.GraphRevision)
	if err != nil {
		return pre, err
	}
	if err := sideEffectFn(ctx, pre); err != nil {
		if cErr := compensateFn(ctx, taskID, "side_effect_failed"); cErr != nil {
			return pre, errors.Join(err, fmt.Errorf("compensate failed: %w", cErr))
		}
		return pre, err
	}
	post, perr := ValidateLaunch(ctx, store, entrypoint, taskRef, desired, pre.GraphRevision)
	if perr != nil {
		if cErr := compensateFn(ctx, taskID, "post_launch_graph_drift"); cErr != nil {
			return post, fmt.Errorf("%w: %v; compensate failed: %w", ErrPostClaimDrift, perr, cErr)
		}
		return post, fmt.Errorf("%w: %v", ErrPostClaimDrift, perr)
	}
	return post, nil
}

// RequireTaskLaunch is the ONLY public production entry for task-scoped launch
// eligibility checks used by cmd/herd entrypoints. It is a thin alias so
// standing/shot/lost cannot "pass" by blank-identifier assignment.
func RequireTaskLaunch(
	ctx context.Context,
	store RelationStore,
	entrypoint LaunchEntrypoint,
	taskRef Ref,
	desired *Provenance,
	selectionRevision string,
) (*GateResult, error) {
	return ValidateLaunch(ctx, store, entrypoint, taskRef, desired, selectionRevision)
}

// SelectEligibleRefs filters candidates by the launch gate, preserving
// priority DESC / ticket ASC order of the input slice (caller must pre-sort).
// Capability/unknown/stale failures fail the WHOLE selection (nonzero), never
// collapse into zero eligible cards.
func SelectEligibleRefs(
	ctx context.Context,
	store RelationStore,
	entrypoint LaunchEntrypoint,
	candidates []*provider.Task,
	desiredByRef map[string]*Provenance,
) (eligible []*provider.Task, revisions map[string]string, blocked []GateResult, err error) {
	// Fail whole selection if store capability is broken before iterating cards.
	if store == nil {
		return nil, nil, nil, &BlockedError{Code: "capability", Reason: "relation store is nil"}
	}
	ok, serr := store.SupportsRelations(ctx)
	if serr != nil {
		return nil, nil, nil, &BlockedError{Code: "capability", Reason: "relation capability unknown: " + serr.Error()}
	}
	if !ok {
		return nil, nil, nil, &BlockedError{Code: "capability", Reason: ErrCapabilityUnsupported.Error()}
	}

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
				if hardSelectionCodes[be.Code] {
					return nil, nil, blocked, gerr
				}
				if gr != nil {
					blocked = append(blocked, *gr)
				} else {
					blocked = append(blocked, GateResult{Ref: Ref(t.Ref), OK: false})
				}
				continue
			}
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

// IsHardSelectionFailure reports capability/stale class failures.
func IsHardSelectionFailure(err error) bool {
	var be *BlockedError
	if !errors.As(err, &be) {
		return false
	}
	return hardSelectionCodes[be.Code]
}
