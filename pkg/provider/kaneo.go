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
	UseCLI    bool
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

func NewKaneoProvider(apiURL string, projectID string, useCLI bool) *KaneoProvider {
	if projectID == "" {
		projectID = ResolveKaneoProjectID(".")
	}
	return &KaneoProvider{
		APIURL:    apiURL,
		ProjectID: projectID,
		UseCLI:    useCLI,
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
}

type kaneoTaskDTO struct {
	ID          string `json:"id"`
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	ProjectId   string `json:"projectId"`
	CreatedAt   string `json:"createdAt"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func dtoToTask(dto kaneoTaskDTO) *Task {
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
	}
}

func (k *KaneoProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	if k.UseCLI {
		cmd := exec.CommandContext(ctx, "kaneo", "task", "get", id, "--json")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("kaneo task get: %w", err)
		}
		var dto kaneoTaskDTO
		if err := json.Unmarshal(out.Bytes(), &dto); err != nil {
			return nil, fmt.Errorf("parsing kaneo output: %w", err)
		}
		return dtoToTask(dto), nil
	}

	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kaneo API returned non-200 status: %d", resp.StatusCode)
	}
	var dto kaneoTaskDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		return nil, err
	}
	return dtoToTask(dto), nil
}

func (k *KaneoProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	if projectID == "" {
		projectID = k.ProjectID
	}

	if k.UseCLI {
		// The server caps a page at 100 records regardless of --limit, so a
		// single call silently hides everything past card 100 — including
		// unclaimed to-do cards, which breaks pulse. Page until a short page.
		const pageSize = 100
		var all []kaneoTaskDTO
		for page := 1; page <= 50; page++ { // ponytail: 5000-card ceiling, raise if a board ever gets there
			args := []string{"task", "list", "--project", projectID, "--json",
				"--limit", fmt.Sprint(pageSize), "--page", fmt.Sprint(page)}
			if status != "" {
				args = append(args, "--status", status)
			}
			cmd := exec.CommandContext(ctx, "kaneo", args...)
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, err)
			}
			var dtos []kaneoTaskDTO
			if err := json.Unmarshal(out.Bytes(), &dtos); err != nil {
				return nil, fmt.Errorf("parsing kaneo output (page %d): %w", page, err)
			}
			all = append(all, dtos...)
			if len(dtos) < pageSize {
				break
			}
		}
		return filterTasks(all, status), nil
	}

	url := fmt.Sprintf("%s/api/task?projectId=%s", k.APIURL, projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kaneo API returned non-200 status: %d", resp.StatusCode)
	}
	var dtos []kaneoTaskDTO
	if err := json.NewDecoder(resp.Body).Decode(&dtos); err != nil {
		return nil, err
	}
	return filterTasks(dtos, status), nil
}

func filterTasks(dtos []kaneoTaskDTO, status string) []*Task {
	var tasks []*Task
	for _, dto := range dtos {
		if status != "" && !strings.EqualFold(dto.Status, status) {
			continue
		}
		tasks = append(tasks, dtoToTask(dto))
	}
	return tasks
}

func (k *KaneoProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return k.UpdateStatus(ctx, taskID, "in-progress")
}

func (k *KaneoProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	if k.UseCLI {
		cmd := exec.CommandContext(ctx, "kaneo", "task", "status", taskID, status, "--project", k.ProjectID)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("kaneo task status: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return nil
	}

	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, taskID)
	payload := map[string]string{"status": status}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("kaneo API returned non-200 status on update: %d", resp.StatusCode)
	}
	return nil
}

func (k *KaneoProvider) AddComment(ctx context.Context, taskID string, body string) error {
	if k.UseCLI {
		cmd := exec.CommandContext(ctx, "kaneo", "task", "comment", "add", taskID, body)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("kaneo task comment: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return nil
	}

	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, taskID)
	payload := map[string]string{"body": body}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("kaneo API returned non-200 status on comment: %d", resp.StatusCode)
	}
	return nil
}
