package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type KaneoProvider struct {
	APIURL    string
	ProjectID string
	Client    *http.Client
}

func NewKaneoProvider(apiURL string, projectID string) *KaneoProvider {
	return &KaneoProvider{
		APIURL:    apiURL,
		ProjectID: projectID,
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
}

type kaneoTaskDTO struct {
	ID          string   `json:"id"`
	Ref         string   `json:"ref"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    string   `json:"priority"`
	ProjectId   string   `json:"projectId"`
	CreatedAt   string   `json:"createdAt"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (k *KaneoProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := k.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute GET /api/task: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kaneo API returned non-200 status: %d", resp.StatusCode)
	}

	var dto kaneoTaskDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		return nil, fmt.Errorf("failed to decode task JSON: %w", err)
	}

	labels := make([]string, 0, len(dto.Labels))
	for _, l := range dto.Labels {
		labels = append(labels, l.Name)
	}

	createdAt, _ := time.Parse(time.RFC3339, dto.CreatedAt)

	return &Task{
		ID:          dto.ID,
		Ref:         dto.Ref,
		Title:       dto.Title,
		Description: dto.Description,
		Status:      dto.Status,
		Priority:    Priority(dto.Priority),
		ProjectID:   dto.ProjectId,
		Labels:      labels,
		CreatedAt:   createdAt,
	}, nil
}

func (k *KaneoProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	url := fmt.Sprintf("%s/api/task?projectId=%s", k.APIURL, projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := k.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute GET /api/task: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kaneo API returned non-200 status: %d", resp.StatusCode)
	}

	var dtos []kaneoTaskDTO
	if err := json.NewDecoder(resp.Body).Decode(&dtos); err != nil {
		return nil, fmt.Errorf("failed to decode tasks JSON: %w", err)
	}

	tasks := make([]*Task, 0, len(dtos))
	for _, dto := range dtos {
		if status != "" && dto.Status != status {
			continue
		}
		labels := make([]string, 0, len(dto.Labels))
		for _, l := range dto.Labels {
			labels = append(labels, l.Name)
		}
		createdAt, _ := time.Parse(time.RFC3339, dto.CreatedAt)

		tasks = append(tasks, &Task{
			ID:          dto.ID,
			Ref:         dto.Ref,
			Title:       dto.Title,
			Description: dto.Description,
			Status:      dto.Status,
			Priority:    Priority(dto.Priority),
			ProjectID:   dto.ProjectId,
			Labels:      labels,
			CreatedAt:   createdAt,
		})
	}

	return tasks, nil
}

func (k *KaneoProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return k.UpdateStatus(ctx, taskID, "in-progress")
}

func (k *KaneoProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, taskID)
	payload := map[string]string{"status": status}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := k.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute PATCH /api/task: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kaneo API returned non-200 status on update: %d", resp.StatusCode)
	}

	return nil
}

func (k *KaneoProvider) AddComment(ctx context.Context, taskID string, body string) error {
	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, taskID)
	payload := map[string]string{"body": body}
	buf, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err != nil {
		return fmt.Errorf("failed to create comment request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := k.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute POST comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("kaneo API returned non-200 status on comment: %d", resp.StatusCode)
	}

	return nil
}
