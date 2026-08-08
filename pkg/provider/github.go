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
	Token     string
	Owner     string
	Repo      string
	Client    *http.Client
	Deadlines Deadlines
	Retry     RetryPolicy
}

func NewGitHubProvider(token string, owner string, repo string) *GitHubProvider {
	return &GitHubProvider{
		Token:     token,
		Owner:     owner,
		Repo:      repo,
		Client:    defaultHTTPClient(),
		Deadlines: DefaultDeadlines(),
		Retry:     DefaultReadRetry(),
	}
}

func (g *GitHubProvider) deadlines() Deadlines {
	if g == nil {
		return DefaultDeadlines()
	}
	return g.Deadlines.Normalize()
}

func (g *GitHubProvider) readRetry() RetryPolicy {
	if g == nil {
		return DefaultReadRetry()
	}
	return g.Retry.normalize()
}

func (g *GitHubProvider) httpClient() *http.Client {
	if g != nil && g.Client != nil {
		return g.Client
	}
	return defaultHTTPClient()
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
	dls := g.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()

	var task *Task
	err := RetryRead(ctx, g.readRetry(), func(rctx context.Context) error {
		t, e := g.getTaskOnce(rctx, id)
		if e != nil {
			return AsTimeout("github", "GetTask", OpGet, dls.For(OpGet), e)
		}
		task = t
		return nil
	})
	return task, err
}

func (g *GitHubProvider) getTaskOnce(ctx context.Context, id string) (*Task, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s", g.Owner, g.Repo, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "token "+g.Token)
	}

	resp, err := g.httpClient().Do(req)
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
	dls := g.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()

	var tasks []*Task
	err := RetryRead(ctx, g.readRetry(), func(rctx context.Context) error {
		t, e := g.listTasksOnce(rctx, projectID, status)
		if e != nil {
			return AsTimeout("github", "ListTasks", OpList, dls.For(OpList), e)
		}
		tasks = t
		return nil
	})
	return tasks, err
}

func (g *GitHubProvider) listTasksOnce(ctx context.Context, projectID string, status string) ([]*Task, error) {
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
		if err := ctx.Err(); err != nil {
			return nil, AsTimeout("github", "ListTasks", OpList, g.deadlines().For(OpList), err)
		}
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

		resp, err := g.httpClient().Do(req)
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
	canonical := NormalizeStatus(status)
	state := "open"
	if canonical == StatusDone || canonical == StatusArchived {
		state = "closed"
	}
	// GitHub only exposes open/closed; map expectation to that surface for readback.
	want := StatusToDo
	if state == "closed" {
		want = StatusDone
	}

	dls := g.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := g.updateStatusOnce(writeCtx, taskID, state)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("github", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	return AfterMutation(ctx, g, dls, "github", "UpdateStatus", taskID, want, writeErr)
}

func (g *GitHubProvider) updateStatusOnce(ctx context.Context, taskID, state string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s", g.Owner, g.Repo, taskID)
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

	resp, err := g.httpClient().Do(req)
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
	return nil
}

func (g *GitHubProvider) AddComment(ctx context.Context, taskID string, body string) error {
	dls := g.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpComment)
	defer cancel()

	err := g.addCommentOnce(ctx, taskID, body)
	if err == nil {
		return nil
	}
	err = AsTimeout("github", "AddComment", OpComment, dls.For(OpComment), err)
	if IsTimeout(err) {
		return &AmbiguousMutationError{
			Provider: "github",
			Op:       "AddComment",
			TaskID:   taskID,
			WriteErr: err,
		}
	}
	return err
}

func (g *GitHubProvider) addCommentOnce(ctx context.Context, taskID, body string) error {
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

	resp, err := g.httpClient().Do(req)
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

// ListComments implements CommentReader (FAC-145 exact effect readback).
func (g *GitHubProvider) ListComments(ctx context.Context, taskID string) ([]string, error) {
	dls := g.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()
	// PAGINATED: a verdict effect must be findable no matter how many
	// comments precede it (FAC-145). Pages are walked until short.
	var out []string
	const perPage = 100
	for page := 1; page <= maxCommentPages; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s/comments?per_page=%d&page=%d",
			g.Owner, g.Repo, taskID, perPage, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+g.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := g.httpClient().Do(req)
		if err != nil {
			return nil, err
		}
		var dtos []struct {
			Body string `json:"body"`
		}
		if err := DecodeJSONResponse(resp, &dtos); err != nil {
			if pe, ok := err.(*ProviderError); ok {
				pe.Provider, pe.Op = "github", "ListComments"
			}
			return nil, err
		}
		for _, d := range dtos {
			out = append(out, d.Body)
		}
		if len(dtos) < perPage {
			return out, nil
		}
	}
	return nil, fmt.Errorf("github ListComments: exceeded %d pages — refusing a partial readback (FAC-145)", maxCommentPages)
}
