package provider

import (
	"context"
	"fmt"
	"time"
)

// DTOToTask converts raw API DTO parameters to unified Herdforge Task model
func DTOToTask(id, ref, title, description, status string, p Priority, projectID string, labels []string, createdAt time.Time) *Task {
	return &Task{
		ID:          id,
		Ref:         ref,
		Title:       title,
		Description: description,
		Status:      status,
		Priority:    p,
		ProjectID:   projectID,
		Labels:      labels,
		CreatedAt:   createdAt,
	}
}

// ParsePriorityString maps common priority label strings to domain Priority type
func ParsePriorityString(label string) Priority {
	switch label {
	case "urgent", "priority:urgent", "1":
		return PriorityUrgent
	case "high", "priority:high", "2":
		return PriorityHigh
	case "low", "priority:low", "4":
		return PriorityLow
	default:
		return PriorityMedium
	}
}

// VerifyProviderContract sanity-checks that a TaskProvider implements basic API guarantees
func VerifyProviderContract(ctx context.Context, p TaskProvider, projectID string) error {
	if p == nil {
		return fmt.Errorf("provider cannot be nil")
	}
	_, err := p.ListTasks(ctx, projectID, "to-do")
	if err != nil {
		return fmt.Errorf("provider contract failed on ListTasks: %w", err)
	}
	return nil
}
