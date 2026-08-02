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
}

// TaskProvider defines the interface for task tracking backends (Kaneo, GitHub, Linear)
type TaskProvider interface {
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error)
	ClaimTask(ctx context.Context, taskID string, role string) error
	UpdateStatus(ctx context.Context, taskID string, status string) error
	AddComment(ctx context.Context, taskID string, body string) error
}
