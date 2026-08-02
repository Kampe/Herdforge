package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type JiraProvider struct {
	BaseURL    string
	APIToken   string
	UserEmail  string
	HTTPClient *http.Client
}

type jiraIssueDTO struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Labels    []string  `json:"labels"`
		Created   time.Time `json:"created"`
		Project   struct {
			Key string `json:"key"`
		} `json:"project"`
	} `json:"fields"`
}

type jiraSearchDTO struct {
	Issues []jiraIssueDTO `json:"issues"`
}

func NewJiraProvider(baseURL, userEmail, apiToken string) *JiraProvider {
	return &JiraProvider{
		BaseURL:    baseURL,
		UserEmail:  userEmail,
		APIToken:   apiToken,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (j *JiraProvider) doRequest(ctx context.Context, method, urlPath string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("%s%s", j.BaseURL, urlPath), reqBody)
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(j.UserEmail, j.APIToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := j.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API error HTTP %d: %s", resp.StatusCode, string(respData))
	}

	return respData, nil
}

func (j *JiraProvider) mapJiraToTask(issue *jiraIssueDTO) *Task {
	p := ParsePriorityString(issue.Fields.Priority.Name)
	return DTOToTask(
		issue.ID,
		issue.Key,
		issue.Fields.Summary,
		issue.Fields.Description,
		issue.Fields.Status.Name,
		p,
		issue.Fields.Project.Key,
		issue.Fields.Labels,
		issue.Fields.Created,
	)
}

func (j *JiraProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	data, err := j.doRequest(ctx, "GET", fmt.Sprintf("/rest/api/3/issue/%s", id), nil)
	if err != nil {
		return nil, fmt.Errorf("jira GetTask failed: %w", err)
	}

	var issue jiraIssueDTO
	if err := json.Unmarshal(data, &issue); err != nil {
		return nil, fmt.Errorf("jira unmarshal issue failed: %w", err)
	}

	return j.mapJiraToTask(&issue), nil
}

func (j *JiraProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	jql := fmt.Sprintf("project = '%s'", projectID)
	if status != "" {
		jql = fmt.Sprintf("%s AND status = '%s'", jql, status)
	}

	urlPath := fmt.Sprintf("/rest/api/3/search?jql=%s", url.QueryEscape(jql))
	data, err := j.doRequest(ctx, "GET", urlPath, nil)
	if err != nil {
		return nil, fmt.Errorf("jira ListTasks failed: %w", err)
	}

	var search jiraSearchDTO
	if err := json.Unmarshal(data, &search); err != nil {
		return nil, fmt.Errorf("jira unmarshal search failed: %w", err)
	}

	var tasks []*Task
	for i := range search.Issues {
		tasks = append(tasks, j.mapJiraToTask(&search.Issues[i]))
	}

	return tasks, nil
}

func (j *JiraProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return j.AddComment(ctx, taskID, fmt.Sprintf("Claimed by agent role [%s]", role))
}

func (j *JiraProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	payload := map[string]interface{}{
		"transition": map[string]string{
			"name": status,
		},
	}
	_, err := j.doRequest(ctx, "POST", fmt.Sprintf("/rest/api/3/issue/%s/transitions", taskID), payload)
	if err != nil {
		return fmt.Errorf("jira UpdateStatus failed: %w", err)
	}
	return nil
}

func (j *JiraProvider) AddComment(ctx context.Context, taskID string, body string) error {
	reqPayload := map[string]interface{}{
		"body": map[string]interface{}{
			"type":    "doc",
			"version": 1,
			"content": []interface{}{
				map[string]interface{}{
					"type": "paragraph",
					"content": []interface{}{
						map[string]interface{}{
							"type": "text",
							"text": body,
						},
					},
				},
			},
		},
	}

	_, err := j.doRequest(ctx, "POST", fmt.Sprintf("/rest/api/3/issue/%s/comment", taskID), reqPayload)
	if err != nil {
		return fmt.Errorf("jira AddComment failed: %w", err)
	}
	return nil
}
