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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned non-200 status: %d", resp.StatusCode)
	}

	var dto githubIssueDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub issue JSON: %w", err)
	}

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
		Status:      dto.State,
		Priority:    p,
		ProjectID:   fmt.Sprintf("%s/%s", g.Owner, g.Repo),
		Labels:      labels,
		CreatedAt:   createdAt,
	}, nil
}

func (g *GitHubProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	stateQuery := "open"
	if status == "closed" || status == "done" {
		stateQuery = "closed"
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=%s", g.Owner, g.Repo, stateQuery)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned non-200 status: %d", resp.StatusCode)
	}

	var dtos []githubIssueDTO
	if err := json.NewDecoder(resp.Body).Decode(&dtos); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub issues JSON: %w", err)
	}

	tasks := make([]*Task, 0, len(dtos))
	for _, dto := range dtos {
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

		tasks = append(tasks, &Task{
			ID:          fmt.Sprintf("%d", dto.Number),
			Ref:         fmt.Sprintf("#%d", dto.Number),
			Title:       dto.Title,
			Description: dto.Body,
			Status:      dto.State,
			Priority:    p,
			ProjectID:   fmt.Sprintf("%s/%s", g.Owner, g.Repo),
			Labels:      labels,
			CreatedAt:   createdAt,
		})
	}

	return tasks, nil
}

func (g *GitHubProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return g.AddComment(ctx, taskID, fmt.Sprintf("Task claimed by role `%s`", role))
}

func (g *GitHubProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s", g.Owner, g.Repo, taskID)
	state := "open"
	if status == "closed" || status == "done" {
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned non-200 status on update: %d", resp.StatusCode)
	}

	return nil
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("GitHub API returned non-200 status on comment: %d", resp.StatusCode)
	}

	return nil
}
