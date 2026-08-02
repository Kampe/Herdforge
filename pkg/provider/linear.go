package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type LinearProvider struct {
	APIKey  string
	Client  *http.Client
	BaseURL string
}

func NewLinearProvider(apiKey string) *LinearProvider {
	return &LinearProvider{
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 10 * time.Second},
		BaseURL: "https://api.linear.app/graphql",
	}
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

	resp, err := l.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GraphQL request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear API returned non-200 status: %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (l *LinearProvider) GetTask(ctx context.Context, id string) (*Task, error) {
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
		Status:      dto.State.Name,
		Priority:    p,
		ProjectID:   dto.Project.ID,
		Labels:      labels,
	}, nil
}

func (l *LinearProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
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

	var tasks []*Task
	for _, dto := range res.Data.Issues.Nodes {
		if projectID != "" && dto.Project.ID != projectID {
			continue
		}
		if status != "" && dto.State.Name != status {
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
			Status:      dto.State.Name,
			Priority:    p,
			ProjectID:   dto.Project.ID,
			Labels:      labels,
		})
	}

	return tasks, nil
}

func (l *LinearProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return l.UpdateStatus(ctx, taskID, "In Progress")
}

func (l *LinearProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
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
	query := `mutation CreateComment($issueId: String!, $body: String!) { commentCreate(input: { issueId: $issueId, body: $body }) { success } }`
	var res struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	return l.doGraphQL(ctx, query, map[string]interface{}{"issueId": taskID, "body": body}, &res)
}
