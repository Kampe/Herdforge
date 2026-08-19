package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
)

const ExternalSourceLabel = "external-source"

// NewTaskCreatedHandler maps a verified task.created event to one backlog
// card. Receiver's durable delivery claim supplies exactly-once invocation.
func NewTaskCreatedHandler(board provider.TaskCreator, defaultProject string) EventHandler {
	return func(event *WebhookEvent) error {
		if event == nil || event.Type != EventTaskCreated {
			return fmt.Errorf("task.created handler: unsupported event")
		}
		if board == nil {
			return fmt.Errorf("task.created handler: task creator is required")
		}
		var in struct {
			Title       string            `json:"title"`
			Body        string            `json:"body"`
			Description string            `json:"description"`
			Priority    provider.Priority `json:"priority"`
			Labels      []string          `json:"labels"`
		}
		payload, err := json.Marshal(event.Payload)
		if err != nil {
			return fmt.Errorf("task.created handler: encode payload: %w", err)
		}
		if err := json.Unmarshal(payload, &in); err != nil {
			return fmt.Errorf("task.created handler: decode payload: %w", err)
		}
		if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(event.TaskRef) == "" {
			return fmt.Errorf("task.created handler: title and task_ref are required")
		}
		project := strings.TrimSpace(event.ProjectID)
		if project == "" {
			project = strings.TrimSpace(defaultProject)
		}
		if project == "" {
			return fmt.Errorf("task.created handler: project is required")
		}
		description := in.Description
		if description == "" {
			description = in.Body
		}
		fence := deps.FormatProvenanceFence(deps.EmptyProvenance(deps.Ref(event.TaskRef)))
		description = strings.TrimRight(description, "\n")
		if description != "" {
			description += "\n\n"
		}
		description += fence
		labels := append([]string(nil), in.Labels...)
		if !hasLabel(labels, ExternalSourceLabel) {
			labels = append(labels, ExternalSourceLabel)
		}
		_, err = board.CreateTask(context.Background(), &provider.Task{
			Title: in.Title, Description: description, Status: provider.StatusToDo,
			Priority: in.Priority, ProjectID: project, Labels: labels,
		})
		if err != nil {
			return fmt.Errorf("task.created handler: create card: %w", err)
		}
		return nil
	}
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}
