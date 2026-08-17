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

// BulkRelationProvider is the project graph surface for SnapshotGraph (FAC-159).
// Kaneo 0.11.x exposes only GET /api/task-relation/:taskId (no project-level
// relation RPC). Production ListProjectRelations is therefore an honest
// O(board) credentialed concurrent HTTP fan-out under the list deadline —
// never silent CLI fan-out, never mislabeled as O(1) bulk.
type BulkRelationProvider interface {
	RelationProvider
	// ListProjectRelations returns the full project relation multiset (deduped
	// by id, dual-end agreement). May be O(board) concurrent requests when the
	// provider has no single project-relation endpoint; must honor ctx deadline
	// and fail closed without credentials.
	ListProjectRelations(ctx context.Context, projectID string) ([]Relation, error)
}

// DefaultBulkRelationConcurrency bounds concurrent per-task relation fetches
// for ordinary O(board) project graph snapshots (measured ~4s for 164 tasks
// @16). Kaneo uses a larger, still bounded pool for genuinely large boards;
// see the Kaneo-specific constants below.
const DefaultBulkRelationConcurrency = 16

// Large-board Kaneo snapshots need more parallelism because Kaneo exposes no
// project-level relation endpoint. Keep the increase provider-specific and
// capped: ordinary list deadlines remain unchanged and a large board cannot
// turn into an unbounded request storm.
const (
	KaneoLargeBoardThreshold          = 500
	DefaultKaneoLargeBoardConcurrency = 64
	MaxKaneoGraphConcurrency          = 128
	KaneoGraphBatchSize               = 256
)
