package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type LinearProvider struct {
	APIKey    string
	Client    *http.Client
	BaseURL   string
	Deadlines Deadlines
	Retry     RetryPolicy
}

func NewLinearProvider(apiKey string) *LinearProvider {
	return &LinearProvider{
		APIKey:    apiKey,
		Client:    defaultHTTPClient(),
		BaseURL:   "https://api.linear.app/graphql",
		Deadlines: DefaultDeadlines(),
		Retry:     DefaultReadRetry(),
	}
}

func (l *LinearProvider) deadlines() Deadlines {
	if l == nil {
		return DefaultDeadlines()
	}
	return l.Deadlines.Normalize()
}

func (l *LinearProvider) readRetry() RetryPolicy {
	if l == nil {
		return DefaultReadRetry()
	}
	return l.Retry.normalize()
}

func (l *LinearProvider) httpClient() *http.Client {
	if l != nil && l.Client != nil {
		return l.Client
	}
	return defaultHTTPClient()
}

type linearGraphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type linearIssueDTO struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    int    `json:"priority"` // 1=Urgent, 2=High, 3=Medium, 4=Low
	State       struct {
		Name string `json:"name"`
	} `json:"state"`
	Project struct {
		ID string `json:"id"`
	} `json:"project"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

func (l *LinearProvider) doGraphQL(ctx context.Context, query string, vars map[string]interface{}, out interface{}) error {
	reqBody, err := json.Marshal(linearGraphQLRequest{Query: query, Variables: vars})
	if err != nil {
		return fmt.Errorf("failed to marshal GraphQL request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return fmt.Errorf("failed to create GraphQL request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", l.APIKey)

	resp, err := l.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GraphQL request: %w", err)
	}

	// DecodeJSONResponse rejects non-2xx and 200 bodies with errors/error keys
	// (GraphQL commonly returns HTTP 200 with {"errors":[...]}).
	if err := DecodeJSONResponse(resp, out); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "linear"
			pe.Op = "graphql"
		}
		return err
	}
	return nil
}

func (l *LinearProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	dls := l.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()
	var task *Task
	err := RetryRead(ctx, l.readRetry(), func(rctx context.Context) error {
		t, e := l.getTaskOnce(rctx, id)
		if e != nil {
			return AsTimeout("linear", "GetTask", OpGet, dls.For(OpGet), e)
		}
		task = t
		return nil
	})
	return task, err
}

func (l *LinearProvider) getTaskOnce(ctx context.Context, id string) (*Task, error) {
	query := `query GetIssue($id: String!) { issue(id: $id) { id identifier title description priority state { name } project { id } labels { nodes { name } } } }`
	var res struct {
		Data struct {
			Issue linearIssueDTO `json:"issue"`
		} `json:"data"`
	}

	if err := l.doGraphQL(ctx, query, map[string]interface{}{"id": id}, &res); err != nil {
		return nil, err
	}

	dto := res.Data.Issue
	labels := make([]string, 0, len(dto.Labels.Nodes))
	for _, node := range dto.Labels.Nodes {
		labels = append(labels, node.Name)
	}

	p := PriorityMedium
	switch dto.Priority {
	case 1:
		p = PriorityUrgent
	case 2:
		p = PriorityHigh
	case 3:
		p = PriorityMedium
	case 4:
		p = PriorityLow
	}

	return &Task{
		ID:          dto.ID,
		Ref:         dto.Identifier,
		Title:       dto.Title,
		Description: dto.Description,
		Status:      NormalizeStatus(dto.State.Name),
		Priority:    p,
		ProjectID:   dto.Project.ID,
		Labels:      labels,
	}, nil
}

func (l *LinearProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	dls := l.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()
	var tasks []*Task
	err := RetryRead(ctx, l.readRetry(), func(rctx context.Context) error {
		ts, e := l.listTasksOnce(rctx, projectID, status)
		if e != nil {
			return AsTimeout("linear", "ListTasks", OpList, dls.For(OpList), e)
		}
		tasks = ts
		return nil
	})
	return tasks, err
}

func (l *LinearProvider) listTasksOnce(ctx context.Context, projectID string, status string) ([]*Task, error) {
	query := `query { issues { nodes { id identifier title description priority state { name } project { id } labels { nodes { name } } } } }`
	var res struct {
		Data struct {
			Issues struct {
				Nodes []linearIssueDTO `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}

	if err := l.doGraphQL(ctx, query, nil, &res); err != nil {
		return nil, err
	}

	want := ""
	if status != "" {
		want = NormalizeStatus(status)
	}

	var tasks []*Task
	for _, dto := range res.Data.Issues.Nodes {
		if projectID != "" && dto.Project.ID != projectID {
			continue
		}
		canon := NormalizeStatus(dto.State.Name)
		if want != "" && canon != want {
			continue
		}

		labels := make([]string, 0, len(dto.Labels.Nodes))
		for _, node := range dto.Labels.Nodes {
			labels = append(labels, node.Name)
		}

		p := PriorityMedium
		switch dto.Priority {
		case 1:
			p = PriorityUrgent
		case 2:
			p = PriorityHigh
		case 3:
			p = PriorityMedium
		case 4:
			p = PriorityLow
		}

		tasks = append(tasks, &Task{
			ID:          dto.ID,
			Ref:         dto.Identifier,
			Title:       dto.Title,
			Description: dto.Description,
			Status:      canon,
			Priority:    p,
			ProjectID:   dto.Project.ID,
			Labels:      labels,
		})
	}

	return tasks, nil
}

func (l *LinearProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return l.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (l *LinearProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	canonical := NormalizeStatus(status)
	dls := l.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := l.updateStatusOnce(writeCtx, taskID, status)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("linear", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	return AfterMutation(ctx, l, dls, "linear", "UpdateStatus", taskID, canonical, writeErr)
}

func (l *LinearProvider) updateStatusOnce(ctx context.Context, taskID, status string) error {
	query := `mutation UpdateIssueState($id: String!, $state: String!) { issueUpdate(id: $id, input: { stateId: $state }) { success } }`
	var res struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	return l.doGraphQL(ctx, query, map[string]interface{}{"id": taskID, "state": status}, &res)
}

func (l *LinearProvider) AddComment(ctx context.Context, taskID string, body string) error {
	dls := l.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpComment)
	defer cancel()
	query := `mutation CreateComment($issueId: String!, $body: String!) { commentCreate(input: { issueId: $issueId, body: $body }) { success } }`
	var res struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	err := l.doGraphQL(ctx, query, map[string]interface{}{"issueId": taskID, "body": body}, &res)
	if err == nil {
		return nil
	}
	err = AsTimeout("linear", "AddComment", OpComment, dls.For(OpComment), err)
	if IsTimeout(err) {
		return &AmbiguousMutationError{Provider: "linear", Op: "AddComment", TaskID: taskID, WriteErr: err}
	}
	return err
}
