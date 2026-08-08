package provider

import (
	"context"
	"time"
)

type Priority string

const (
	PriorityUrgent Priority = "urgent"
	PriorityHigh   Priority = "high"
	PriorityMedium Priority = "medium"
	PriorityLow    Priority = "low"
)

type Task struct {
	ID          string    `json:"id"`
	Ref         string    `json:"ref"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Priority    Priority  `json:"priority"`
	ProjectID   string    `json:"project_id"`
	Labels      []string  `json:"labels"`
	CreatedAt   time.Time `json:"created_at"`
	// UpdatedAt is the provider's last-mutation timestamp when available
	// (Kaneo updatedAt, GitHub updated_at). Used as part of the opaque
	// ProviderCAS revision token (FAC-147). Zero means the provider did
	// not supply one; revision encoding falls back to status+id+createdAt.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// StatusReceipt is the signed receipt extracted from the task description
	// footer after an atomic status PUT (FAC-147). Not a native Kaneo field.
	StatusReceipt string `json:"-"`
	// Position is Kaneo's board rank; required for full-schema PUT rebuilds.
	// HasPosition is true only when the provider returned a position field —
	// zero is a valid board rank and must not be confused with "unknown".
	Position    float64 `json:"-"`
	HasPosition bool    `json:"-"`
}

// TaskProvider defines the interface for task tracking backends (Kaneo, GitHub, Linear)
type TaskProvider interface {
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error)
	ClaimTask(ctx context.Context, taskID string, role string) error
	UpdateStatus(ctx context.Context, taskID string, status string) error
	AddComment(ctx context.Context, taskID string, body string) error
}

// CommentReader is the OPTIONAL provider capability that makes verdict
// delivery genuinely exactly-once (FAC-145): the coordinator can read
// comments back and confirm whether an exact effect id was already
// delivered. Adapters that cannot expose comments make confirmed delivery
// impossible, and authority-bearing consumers must fail closed rather than
// publish an unverifiable verdict.
// maxCommentPages bounds pagination walks; exceeding it is an explicit
// refusal, never a silently partial readback (FAC-145).
const maxCommentPages = 50

type CommentReader interface {
	// ListComments returns the comment bodies currently visible on taskID.
	ListComments(ctx context.Context, taskID string) ([]string, error)
}
