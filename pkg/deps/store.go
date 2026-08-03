package deps

import (
	"context"
	"errors"
	"fmt"
)

// ErrCapabilityUnsupported is returned when the provider has no relation API.
var ErrCapabilityUnsupported = errors.New("deps: provider relation capability unsupported")

// ErrCapabilityUnknown is returned when relation capability cannot be determined.
var ErrCapabilityUnknown = errors.New("deps: provider relation capability unknown")

// ErrUnresolvedRef means a ticket ref could not be resolved to an immutable TaskID.
var ErrUnresolvedRef = errors.New("deps: unresolved task ref")

// ErrDeletedTask means a referenced task is missing/deleted on the board.
var ErrDeletedTask = errors.New("deps: referenced task deleted or missing")

// ErrMissingProvenance means launch was attempted without a versioned provenance record.
var ErrMissingProvenance = errors.New("deps: missing structured provenance")

// ErrUnsupportedProvenance means provenance version is not SchemaVersion.
var ErrUnsupportedProvenance = errors.New("deps: unsupported provenance version")

// ErrSelfEdge means create rejected a self-edge.
var ErrSelfEdge = errors.New("deps: self-edge rejected")

// ErrUnknownEdgeType means create saw an unknown relation type.
var ErrUnknownEdgeType = errors.New("deps: unknown edge type")

// ErrAmbiguousMutation means create/delete timed out or left indeterminate state;
// callers must enter reconciliation and never treat as success.
var ErrAmbiguousMutation = errors.New("deps: ambiguous relation mutation")

// ErrClaimFence means ValidateClaim lacked a bound selection/provider revision.
var ErrClaimFence = errors.New("deps: claim fence revision required")

// ErrPostClaimDrift means the graph changed after the claim mutation.
var ErrPostClaimDrift = errors.New("deps: post-claim graph drift (compensation required)")

// GraphSnapshot is the authoritative full relation closure for cycle detection
// and SHA-256 revision binding.
type GraphSnapshot struct {
	Edges            []DependencyEdge
	ProviderRevision string
}

// RelationStore is the provider-neutral dependency surface.
// Implementations must support list/create/delete with readback after mutation.
// Providers without relation APIs must return ErrCapabilityUnsupported explicitly
// (never silent empty success).
type RelationStore interface {
	// SupportsRelations reports explicit capability. false + nil means unsupported.
	// Unknown must return an error (ErrCapabilityUnknown).
	SupportsRelations(ctx context.Context) (bool, error)

	// ListRelations returns all relations involving taskID (as source or target).
	// taskID is the immutable provider id (or ref when the adapter accepts refs).
	ListRelations(ctx context.Context, taskID TaskID) ([]DependencyEdge, error)

	// SnapshotGraph returns the authoritative FULL relation closure for the
	// project (not a single-task listing). Required for global cycle detection.
	SnapshotGraph(ctx context.Context) (*GraphSnapshot, error)

	// CreateRelation creates a directed edge and MUST read it back on both ends.
	// Rejects self-edges and unknown types. Ambiguous create errors must be
	// reconciled (existing edge returned) so retries cannot duplicate.
	CreateRelation(ctx context.Context, edge DependencyEdge) (DependencyEdge, error)

	// DeleteRelation deletes by provider relation id. Implementations MUST
	// capture endpoints, delete, and verify absence on BOTH source and target
	// list readbacks. Ambiguous delete/timeouts return ErrAmbiguousMutation
	// (never success).
	DeleteRelation(ctx context.Context, relationID string, sourceID, targetID TaskID) error

	// ResolveRef maps a ticket ref to an immutable TaskID. Missing => ErrUnresolvedRef / ErrDeletedTask.
	ResolveRef(ctx context.Context, ref Ref) (TaskID, error)

	// TaskStatus returns canonical lifecycle status for a ref (to-do/in-progress/done/...).
	// Unknown/unreadable statuses must surface as errors — never "eligible".
	TaskStatus(ctx context.Context, ref Ref) (status string, taskID TaskID, err error)
}

// AsRelationStore extracts a RelationStore from a TaskProvider when it implements
// the optional adapter interface, or returns ErrCapabilityUnsupported.
type relationAdapter interface {
	AsDepsStore() RelationStore
}

// FromProvider returns a RelationStore. Prefer explicit AsDepsStore(); otherwise
// unsupported.
func FromProvider(tp any) (RelationStore, error) {
	if tp == nil {
		return nil, fmt.Errorf("%w: nil provider", ErrCapabilityUnknown)
	}
	if a, ok := tp.(relationAdapter); ok {
		if s := a.AsDepsStore(); s != nil {
			return s, nil
		}
	}
	if s, ok := tp.(RelationStore); ok && s != nil {
		return s, nil
	}
	return nil, ErrCapabilityUnsupported
}
