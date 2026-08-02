package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type KaneoProvider struct {
	APIURL    string
	ProjectID string
	Client    *http.Client
}

type KaneoLinkConfig struct {
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
}

// ResolveKaneoProjectID attempts to read project ID from .herd/kaneo.json, falling back to root .kaneo.json
func ResolveKaneoProjectID(rootDir string) string {
	paths := []string{
		filepath.Join(rootDir, ".herd", "kaneo.json"),
		filepath.Join(rootDir, ".kaneo.json"),
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			var link KaneoLinkConfig
			if err := json.Unmarshal(data, &link); err == nil && link.Project != "" {
				return link.Project
			}
		}
	}
	return ""
}

func NewKaneoProvider(apiURL string, projectID string) *KaneoProvider {
	if projectID == "" {
		projectID = ResolveKaneoProjectID(".")
	}
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
	if err == nil {
		resp, err := k.Client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var dto kaneoTaskDTO
				if err := json.NewDecoder(resp.Body).Decode(&dto); err == nil {
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
			} else {
				return nil, fmt.Errorf("kaneo API returned non-200 status: %d", resp.StatusCode)
			}
		}
	}

	// CLI fallback
	cmd := exec.CommandContext(ctx, "kaneo", "task", "get", id, "--json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil {
		var dto kaneoTaskDTO
		if err := json.Unmarshal(out.Bytes(), &dto); err == nil {
			createdAt, _ := time.Parse(time.RFC3339, dto.CreatedAt)
			return &Task{
				ID:          dto.ID,
				Ref:         dto.Ref,
				Title:       dto.Title,
				Description: dto.Description,
				Status:      dto.Status,
				Priority:    Priority(dto.Priority),
				ProjectID:   dto.ProjectId,
				CreatedAt:   createdAt,
			}, nil
		}
	}

	return nil, fmt.Errorf("failed to get task %s via API or CLI", id)
}

func (k *KaneoProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	if projectID == "" {
		projectID = k.ProjectID
	}
	url := fmt.Sprintf("%s/api/task?projectId=%s", k.APIURL, projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil {
		resp, err := k.Client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var dtos []kaneoTaskDTO
				if err := json.NewDecoder(resp.Body).Decode(&dtos); err == nil {
					var tasks []*Task
					for _, dto := range dtos {
						if status != "" && !strings.EqualFold(dto.Status, status) {
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
			} else {
				return nil, fmt.Errorf("kaneo API returned non-200 status: %d", resp.StatusCode)
			}
		}
	}

	// CLI fallback
	cmd := exec.CommandContext(ctx, "kaneo", "task", "list", "--project", projectID, "--json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil {
		var dtos []kaneoTaskDTO
		if err := json.Unmarshal(out.Bytes(), &dtos); err == nil {
			var tasks []*Task
			for _, dto := range dtos {
				if status != "" && !strings.EqualFold(dto.Status, status) {
					continue
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
					CreatedAt:   createdAt,
				})
			}
			return tasks, nil
		}
	}

	return nil, fmt.Errorf("failed to list tasks for project %s via API or CLI", projectID)
}

func (k *KaneoProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return k.UpdateStatus(ctx, taskID, "in-progress")
}

func (k *KaneoProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, taskID)
	payload := map[string]string{"status": status}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewBuffer(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, err := k.Client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
				return nil
			}
			return fmt.Errorf("kaneo API returned non-200 status on update: %d", resp.StatusCode)
		}
	}

	// CLI fallback
	cmd := exec.CommandContext(ctx, "kaneo", "task", "status", taskID, status, "--project", k.ProjectID)
	if err := cmd.Run(); err == nil {
		return nil
	}

	return fmt.Errorf("failed to update status for task %s via API or CLI", taskID)
}

func (k *KaneoProvider) AddComment(ctx context.Context, taskID string, body string) error {
	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, taskID)
	payload := map[string]string{"body": body}
	buf, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, err := k.Client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
				return nil
			}
			return fmt.Errorf("kaneo API returned non-200 status on comment: %d", resp.StatusCode)
		}
	}

	// CLI fallback
	cmd := exec.CommandContext(ctx, "kaneo", "task", "comment", "add", taskID, body)
	if err := cmd.Run(); err == nil {
		return nil
	}

	return fmt.Errorf("failed to add comment to task %s via API or CLI", taskID)
}
