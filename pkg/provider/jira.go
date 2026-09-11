package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type JiraProvider struct {
	BaseURL string
	// ProjectKey is the configured board this provider is bound to. ListTasks
	// falls back to it when the caller passes no project, mirroring Linear, so
	// a bound provider can never fan out across every project on the site.
	ProjectKey string
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
		Summary     string  `json:"summary"`
		Description adfText `json:"description"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Labels  []string `json:"labels"`
		Created jiraTime `json:"created"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
	} `json:"fields"`
}

type jiraSearchDTO struct {
	Issues []jiraIssueDTO `json:"issues"`
	Total  int            `json:"total"`
}

const (
	// jiraSearchPageSize is Jira's documented maximum page for issue search.
	jiraSearchPageSize = 100
	// maxJiraSearchPages bounds the walk. Exceeding it is an explicit refusal,
	// never a silently partial board.
	maxJiraSearchPages = 50
)

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
		issue.Fields.Description.Text,
		NormalizeStatus(issue.Fields.Status.Name),
		p,
		issue.Fields.Project.Key,
		issue.Fields.Labels,
		issue.Fields.Created.Time,
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
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = strings.TrimSpace(j.ProjectKey)
	}
	if projectID == "" {
		return nil, fmt.Errorf("jira ListTasks: no project key configured or supplied")
	}
	if err := checkJQLIdentifier("project key", projectID); err != nil {
		return nil, err
	}
	jql := fmt.Sprintf("project = %q", projectID)
	if status = strings.TrimSpace(status); status != "" {
		if err := checkJQLIdentifier("status", status); err != nil {
			return nil, err
		}
		jql = fmt.Sprintf("%s AND status = %q", jql, status)
	}

	// Jira pages results and defaults maxResults to 50. Returning the first
	// page as if it were the board is a silent truncation: the fleet would
	// simply never see the 51st card. Walk every page, bounded.
	var tasks []*Task
	startAt := 0
	for page := 0; page < maxJiraSearchPages; page++ {
		urlPath := fmt.Sprintf("/rest/api/3/search?jql=%s&startAt=%d&maxResults=%d",
			url.QueryEscape(jql), startAt, jiraSearchPageSize)
		data, err := j.doRequest(ctx, "GET", urlPath, nil)
		if err != nil {
			return nil, fmt.Errorf("jira ListTasks failed: %w", err)
		}
		var search jiraSearchDTO
		if err := json.Unmarshal(data, &search); err != nil {
			return nil, fmt.Errorf("jira unmarshal search failed: %w", err)
		}
		for i := range search.Issues {
			tasks = append(tasks, j.mapJiraToTask(&search.Issues[i]))
		}
		startAt += len(search.Issues)
		if len(search.Issues) == 0 || startAt >= search.Total {
			return tasks, nil
		}
	}
	return nil, fmt.Errorf("jira ListTasks: exceeded %d pages for project %q — refusing a partial board", maxJiraSearchPages, projectID)
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

// updateStatusOnce applies a transition by its ID. Jira's transition API keys
// on transition id, not on a status name, and it is per-issue and per-workflow:
// the set of legal moves depends on where the issue currently sits. Posting a
// name is not a slightly-wrong request, it is a request the board never
// offered, so the id is resolved from the issue's own available transitions and
// a status with no matching transition is refused rather than guessed at.
func (j *JiraProvider) updateStatusOnce(ctx context.Context, taskID, status string) error {
	transitionID, err := j.resolveTransitionID(ctx, taskID, status)
	if err != nil {
		return err
	}
	payload := map[string]interface{}{
		"transition": map[string]string{"id": transitionID},
	}
	if _, err := j.doRequest(ctx, "POST", fmt.Sprintf("/rest/api/3/issue/%s/transitions", taskID), payload); err != nil {
		return fmt.Errorf("jira UpdateStatus failed: %w", err)
	}
	return nil
}

func (j *JiraProvider) resolveTransitionID(ctx context.Context, taskID, status string) (string, error) {
	raw, err := j.doRequest(ctx, "GET", fmt.Sprintf("/rest/api/3/issue/%s/transitions", taskID), nil)
	if err != nil {
		return "", fmt.Errorf("jira UpdateStatus: read available transitions: %w", err)
	}
	var payload struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("jira UpdateStatus: decode transitions: %w", err)
	}
	// Match the request against the transition name, its destination status, and
	// the canonical form of both, so "done" reaches a "Done" column whichever
	// vocabulary the board uses.
	wanted := NormalizeStatus(status)
	available := make([]string, 0, len(payload.Transitions))
	for _, t := range payload.Transitions {
		available = append(available, fmt.Sprintf("%s->%s", t.Name, t.To.Name))
		for _, candidate := range []string{t.Name, t.To.Name} {
			if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(status)) ||
				NormalizeStatus(candidate) == wanted {
				return t.ID, nil
			}
		}
	}
	return "", fmt.Errorf("jira UpdateStatus: issue %s offers no transition to %q (available: %s)",
		taskID, status, strings.Join(available, ", "))
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

// ListComments implements CommentReader (FAC-145 exact effect readback).
// Atlassian document bodies are flattened to their text runs.
func (j *JiraProvider) ListComments(ctx context.Context, taskID string) ([]string, error) {
	dls := j.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()
	var out []string
	startAt := 0
	for page := 0; page < maxCommentPages; page++ {
		raw, err := j.doRequest(ctx, "GET", fmt.Sprintf("/rest/api/3/issue/%s/comment?startAt=%d&maxResults=100", taskID, startAt), nil)
		if err != nil {
			return nil, fmt.Errorf("jira ListComments failed: %w", err)
		}
		page, pErr := parseJiraCommentPage(raw)
		if pErr != nil {
			return nil, pErr
		}
		out = append(out, page.bodies...)
		startAt += len(page.bodies)
		if len(page.bodies) == 0 || startAt >= page.total {
			return out, nil
		}
	}
	return nil, fmt.Errorf("jira ListComments: exceeded %d pages — refusing a partial readback (FAC-145)", maxCommentPages)
}

type jiraCommentPage struct {
	bodies []string
	total  int
}

func parseJiraCommentPage(raw []byte) (jiraCommentPage, error) {
	var payload struct {
		Total    int `json:"total"`
		Comments []struct {
			Body struct {
				Content []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"content"`
			} `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return jiraCommentPage{}, fmt.Errorf("jira ListComments decode: %w", err)
	}
	bodies := make([]string, 0, len(payload.Comments))
	for _, c := range payload.Comments {
		var b strings.Builder
		for _, para := range c.Body.Content {
			for _, run := range para.Content {
				b.WriteString(run.Text)
			}
		}
		bodies = append(bodies, b.String())
	}
	total := payload.Total
	if total == 0 {
		total = len(bodies)
	}
	return jiraCommentPage{bodies: bodies, total: total}, nil
}
