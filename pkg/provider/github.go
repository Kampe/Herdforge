package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type GitHubProvider struct {
	Token  string
	Owner  string
	Repo   string
	Client *http.Client
}

func NewGitHubProvider(token string, owner string, repo string) *GitHubProvider {
	return &GitHubProvider{
		Token:  token,
		Owner:  owner,
		Repo:   repo,
		Client: &http.Client{Timeout: 10 * time.Second},
	}
}

type githubIssueDTO struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	NodeID    string `json:"node_id"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (g *GitHubProvider) mapIssue(dto githubIssueDTO) *Task {
	labels := make([]string, 0, len(dto.Labels))
	p := PriorityMedium
	for _, l := range dto.Labels {
		labels = append(labels, l.Name)
		switch l.Name {
		case "priority:urgent", "urgent":
			p = PriorityUrgent
		case "priority:high", "high":
			p = PriorityHigh
		case "priority:low", "low":
			p = PriorityLow
		}
	}
	createdAt, _ := time.Parse(time.RFC3339, dto.CreatedAt)
	return &Task{
		ID:          fmt.Sprintf("%d", dto.Number),
		Ref:         fmt.Sprintf("#%d", dto.Number),
		Title:       dto.Title,
		Description: dto.Body,
		Status:      NormalizeStatus(dto.State),
		Priority:    p,
		ProjectID:   fmt.Sprintf("%s/%s", g.Owner, g.Repo),
		Labels:      labels,
		CreatedAt:   createdAt,
	}
}

func (g *GitHubProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s", g.Owner, g.Repo, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "token "+g.Token)
	}

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute GET GitHub issue: %w", err)
	}

	var dto githubIssueDTO
	if err := DecodeJSONResponse(resp, &dto); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "github"
			pe.Op = "GetTask"
		}
		return nil, err
	}
	return g.mapIssue(dto), nil
}

func (g *GitHubProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	stateQuery := "open"
	ns := NormalizeStatus(status)
	if ns == StatusDone || ns == StatusArchived || status == "closed" {
		stateQuery = "closed"
	}

	// Paginate until empty page (short page continues). Duplicate page or
	// page-cap without empty termination is a hard error.
	const pageSize = 100
	acc := NewPageAccumulator()
	var tasks []*Task
	for page := 1; page <= DefaultMaxListPages; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=%s&per_page=%d&page=%d",
			g.Owner, g.Repo, stateQuery, pageSize, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create GitHub request: %w", err)
		}

		req.Header.Set("Accept", "application/vnd.github.v3+json")
		if g.Token != "" {
			req.Header.Set("Authorization", "token "+g.Token)
		}

		resp, err := g.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to execute GET GitHub issues: %w", err)
		}

		var dtos []githubIssueDTO
		if err := DecodeJSONResponse(resp, &dtos); err != nil {
			if pe, ok := err.(*ProviderError); ok {
				pe.Provider = "github"
				pe.Op = "ListTasks"
			}
			return nil, err
		}

		fresh := 0
		for _, dto := range dtos {
			id := fmt.Sprintf("%d", dto.Number)
			if !acc.Add(id) {
				continue
			}
			tasks = append(tasks, g.mapIssue(dto))
			fresh++
		}
		dec := DecidePagination(len(dtos), fresh)
		switch dec {
		case PageStopEmpty:
			return tasks, nil
		case PageStopDuplicate:
			return nil, fmt.Errorf("github ListTasks (page %d): %w", page, ErrDuplicatePage)
		}
	}
	return nil, fmt.Errorf("github ListTasks: %w (maxPages=%d)", ErrPaginationCap, DefaultMaxListPages)
}

func (g *GitHubProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return g.AddComment(ctx, taskID, fmt.Sprintf("Task claimed by role `%s`", role))
}

func (g *GitHubProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s", g.Owner, g.Repo, taskID)
	canonical := NormalizeStatus(status)
	state := "open"
	if canonical == StatusDone || canonical == StatusArchived {
		state = "closed"
	}

	payload := map[string]string{"state": state}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create patch request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "token "+g.Token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute PATCH GitHub issue: %w", err)
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "github"
			pe.Op = "UpdateStatus"
		}
		return err
	}

	got, gerr := g.GetTask(ctx, taskID)
	if gerr != nil {
		return fmt.Errorf("github status readback after write: %w", gerr)
	}
	// GitHub only exposes open/closed; map expectation to that surface.
	want := StatusToDo
	if state == "closed" {
		want = StatusDone
	}
	return VerifyStatusReadback(taskID, want, got.Status)
}

func (g *GitHubProvider) AddComment(ctx context.Context, taskID string, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s/comments", g.Owner, g.Repo, taskID)
	payload := map[string]string{"body": body}
	buf, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err != nil {
		return fmt.Errorf("failed to create comment request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "token "+g.Token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute POST GitHub comment: %w", err)
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "github"
			pe.Op = "AddComment"
		}
		return err
	}
	return nil
}
