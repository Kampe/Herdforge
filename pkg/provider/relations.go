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
// Kaneo implements full list/create/delete with readback.
type RelationProvider interface {
	ListRelations(ctx context.Context, taskID string) ([]Relation, error)
	CreateRelation(ctx context.Context, sourceID, targetID string, typ RelationType) (*Relation, error)
	DeleteRelation(ctx context.Context, relationID string) error
}
