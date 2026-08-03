package provider

import (
	"context"
	"time"
)

// RelationType is a board relation kind (Kaneo: blocks|related|subtask).
type RelationType string

const (
	RelationBlocks  RelationType = "blocks"
	RelationRelated RelationType = "related"
	RelationSubtask RelationType = "subtask"
)

// ValidRelationType reports whether t is a known provider relation type.
func ValidRelationType(t RelationType) bool {
	switch t {
	case RelationBlocks, RelationRelated, RelationSubtask:
		return true
	default:
		return false
	}
}

// Relation is one provider relation row (immutable IDs).
type Relation struct {
	ID           string
	SourceTaskID string
	TargetTaskID string
	Type         RelationType
	CreatedAt    time.Time
}

// RelationProvider is the optional dependency surface. Providers that do not
// implement it fail the FAC-159 gate with explicit capability unsupported.
// Kaneo implements full list/create/delete with dual-end readback.
type RelationProvider interface {
	ListRelations(ctx context.Context, taskID string) ([]Relation, error)
	// CreateRelation rejects self-edges and unknown types; readbacks both ends;
	// reconciles ambiguous create so retries do not duplicate.
	CreateRelation(ctx context.Context, sourceID, targetID string, typ RelationType) (*Relation, error)
	// DeleteRelation requires captured endpoints and verifies absence on BOTH
	// source and target listings. Ambiguous delete/timeouts never return success.
	DeleteRelation(ctx context.Context, relationID, sourceID, targetID string) error
}

// BulkRelationProvider is the project-level graph surface (FAC-159 live path).
// SnapshotGraph MUST prefer this over sequential ListRelations-per-task so a
// 166-task board cannot stampede the CLI/API (N×seconds subprocesses).
// Implementations return the FULL project relation multiset (deduped by id)
// with dual-end agreement already enforced, or fail closed.
type BulkRelationProvider interface {
	RelationProvider
	// ListProjectRelations returns every relation in projectID in one logical
	// bulk operation (single RPC or demonstrably bounded concurrent fan-out
	// with cancel). Call count must not grow as O(tasks) sequential subprocesses.
	ListProjectRelations(ctx context.Context, projectID string) ([]Relation, error)
}

// DefaultBulkRelationConcurrency bounds concurrent per-task relation fetches
// when a true single-RPC bulk endpoint is unavailable.
const DefaultBulkRelationConcurrency = 16
