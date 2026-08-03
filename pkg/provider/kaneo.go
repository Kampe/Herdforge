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

// kaneoLabel accepts both API object form {"name":"x"} and CLI string form "x".
type kaneoLabel struct {
	Name string
}

func (l *kaneoLabel) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		l.Name = s
		return nil
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	l.Name = obj.Name
	return nil
}

type kaneoTaskDTO struct {
	ID          string       `json:"id"`
	Ref         string       `json:"ref"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Status      string       `json:"status"`
	Priority    string       `json:"priority"`
	ProjectId   string       `json:"projectId"`
	CreatedAt   string       `json:"createdAt"`
	Labels      []kaneoLabel `json:"labels"`
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
		Status:      NormalizeStatus(dto.Status),
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
		if err := DecodeJSONBytes(http.StatusOK, out.Bytes(), &dto); err != nil {
			if pe, ok := err.(*ProviderError); ok {
				pe.Provider = "kaneo"
				pe.Op = "GetTask"
			}
			return nil, fmt.Errorf("kaneo task get: %w", err)
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
	var dto kaneoTaskDTO
	if err := DecodeJSONResponse(resp, &dto); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "GetTask"
		}
		return nil, err
	}
	return dtoToTask(dto), nil
}

func (k *KaneoProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	if projectID == "" {
		projectID = k.ProjectID
	}

	if k.UseCLI {
		// Terminate only on EMPTY page; short pages continue. Duplicate pages
		// and the page cap without empty termination are hard errors.
		// Server may cap below --limit (observed 99/100), so short-page stop
		// hides later cards (FAC-106 / board-done regressions).
		const pageSize = 100
		var all []kaneoTaskDTO
		acc := NewPageAccumulator()
		for page := 1; page <= DefaultMaxListPages; page++ {
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
			if err := DecodeJSONBytes(http.StatusOK, out.Bytes(), &dtos); err != nil {
				if pe, ok := err.(*ProviderError); ok {
					pe.Provider = "kaneo"
					pe.Op = "ListTasks"
				}
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, err)
			}
			fresh := 0
			for _, d := range dtos {
				if !acc.Add(d.ID) {
					continue
				}
				all = append(all, d)
				fresh++
			}
			dec := DecidePagination(len(dtos), fresh)
			switch dec {
			case PageStopEmpty:
				return filterTasks(all, status), nil
			case PageStopDuplicate:
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, ErrDuplicatePage)
			}
		}
		return nil, fmt.Errorf("kaneo task list: %w (maxPages=%d)", ErrPaginationCap, DefaultMaxListPages)
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
	var dtos []kaneoTaskDTO
	if err := DecodeJSONResponse(resp, &dtos); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "ListTasks"
		}
		return nil, err
	}
	return filterTasks(dtos, status), nil
}

func filterTasks(dtos []kaneoTaskDTO, status string) []*Task {
	var tasks []*Task
	want := ""
	if status != "" {
		want = NormalizeStatus(status)
	}
	for _, dto := range dtos {
		t := dtoToTask(dto)
		if want != "" && t.Status != want {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks
}

func (k *KaneoProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return k.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (k *KaneoProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	canonical := NormalizeStatus(status)
	if k.UseCLI {
		cmd := exec.CommandContext(ctx, "kaneo", "task", "status", taskID, status, "--project", k.ProjectID)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("kaneo task status: %s: %w", strings.TrimSpace(string(out)), err)
		}
		// Fail-closed readback: mutation is not success until read agrees.
		got, gerr := k.GetTask(ctx, taskID)
		if gerr != nil {
			return fmt.Errorf("kaneo status readback after write: %w", gerr)
		}
		return VerifyStatusReadback(taskID, canonical, got.Status)
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
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "UpdateStatus"
		}
		return err
	}
	got, gerr := k.GetTask(ctx, taskID)
	if gerr != nil {
		return fmt.Errorf("kaneo status readback after write: %w", gerr)
	}
	return VerifyStatusReadback(taskID, canonical, got.Status)
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
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "AddComment"
		}
		return err
	}
	return nil
}
