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
	Deadlines  Deadlines
	Retry      RetryPolicy
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
		Labels  []string  `json:"labels"`
		Created time.Time `json:"created"`
		Project struct {
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
		HTTPClient: defaultHTTPClient(),
		Deadlines:  DefaultDeadlines(),
		Retry:      DefaultReadRetry(),
	}
}

func (j *JiraProvider) deadlines() Deadlines {
	if j == nil {
		return DefaultDeadlines()
	}
	return j.Deadlines.Normalize()
}

func (j *JiraProvider) readRetry() RetryPolicy {
	if j == nil {
		return DefaultReadRetry()
	}
	return j.Retry.normalize()
}

func (j *JiraProvider) client() *http.Client {
	if j != nil && j.HTTPClient != nil {
		return j.HTTPClient
	}
	return defaultHTTPClient()
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

	resp, err := j.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Fail-closed: non-2xx and 2xx bodies carrying structured error payloads.
	if err := DecodeJSONBytes(resp.StatusCode, respData, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "jira"
			if pe.Message == fmt.Sprintf("HTTP %d", resp.StatusCode) || pe.Body == "" {
				// Preserve body snippet for non-JSON error pages.
				if pe.Body == "" {
					pe.Body = truncate(string(respData), 256)
				}
			}
		}
		return nil, err
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
		NormalizeStatus(issue.Fields.Status.Name),
		p,
		issue.Fields.Project.Key,
		issue.Fields.Labels,
		issue.Fields.Created,
	)
}

func (j *JiraProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	dls := j.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()
	var task *Task
	err := RetryRead(ctx, j.readRetry(), func(rctx context.Context) error {
		t, e := j.getTaskOnce(rctx, id)
		if e != nil {
			return AsTimeout("jira", "GetTask", OpGet, dls.For(OpGet), e)
		}
		task = t
		return nil
	})
	return task, err
}

func (j *JiraProvider) getTaskOnce(ctx context.Context, id string) (*Task, error) {
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
	dls := j.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()
	var tasks []*Task
	err := RetryRead(ctx, j.readRetry(), func(rctx context.Context) error {
		ts, e := j.listTasksOnce(rctx, projectID, status)
		if e != nil {
			return AsTimeout("jira", "ListTasks", OpList, dls.For(OpList), e)
		}
		tasks = ts
		return nil
	})
	return tasks, err
}

func (j *JiraProvider) listTasksOnce(ctx context.Context, projectID string, status string) ([]*Task, error) {
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
	canonical := NormalizeStatus(status)
	dls := j.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := j.updateStatusOnce(writeCtx, taskID, status)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("jira", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	return AfterMutation(ctx, j, dls, "jira", "UpdateStatus", taskID, canonical, writeErr)
}

func (j *JiraProvider) updateStatusOnce(ctx context.Context, taskID, status string) error {
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
	dls := j.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpComment)
	defer cancel()
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
		err = fmt.Errorf("jira AddComment failed: %w", err)
		err = AsTimeout("jira", "AddComment", OpComment, dls.For(OpComment), err)
		if IsTimeout(err) {
			return &AmbiguousMutationError{Provider: "jira", Op: "AddComment", TaskID: taskID, WriteErr: err}
		}
		return err
	}
	return nil
}
