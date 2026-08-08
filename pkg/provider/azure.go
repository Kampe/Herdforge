package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type AzureDevOpsProvider struct {
	OrgURL     string
	Project    string
	PAT        string
	HTTPClient *http.Client
	Deadlines  Deadlines
	Retry      RetryPolicy
}

type azureWorkItemDTO struct {
	ID     int `json:"id"`
	Fields struct {
		Title        string    `json:"System.Title"`
		Description  string    `json:"System.Description"`
		State        string    `json:"System.State"`
		WorkItemType string    `json:"System.WorkItemType"`
		Priority     int       `json:"Microsoft.VSTS.Common.Priority"`
		CreatedDate  time.Time `json:"System.CreatedDate"`
	} `json:"fields"`
}

type azureWIQLResponse struct {
	WorkItems []struct {
		ID int `json:"id"`
	} `json:"workItems"`
}

func NewAzureDevOpsProvider(orgURL, project, pat string) *AzureDevOpsProvider {
	return &AzureDevOpsProvider{
		OrgURL:     orgURL,
		Project:    project,
		PAT:        pat,
		HTTPClient: defaultHTTPClient(),
		Deadlines:  DefaultDeadlines(),
		Retry:      DefaultReadRetry(),
	}
}

func (a *AzureDevOpsProvider) deadlines() Deadlines {
	if a == nil {
		return DefaultDeadlines()
	}
	return a.Deadlines.Normalize()
}

func (a *AzureDevOpsProvider) readRetry() RetryPolicy {
	if a == nil {
		return DefaultReadRetry()
	}
	return a.Retry.normalize()
}

func (a *AzureDevOpsProvider) client() *http.Client {
	if a != nil && a.HTTPClient != nil {
		return a.HTTPClient
	}
	return defaultHTTPClient()
}

func (a *AzureDevOpsProvider) doRequest(ctx context.Context, method, urlPath string, body interface{}, contentType string) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("%s/%s%s", a.OrgURL, a.Project, urlPath), reqBody)
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth("", a.PAT)
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := a.client().Do(req)
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
			pe.Provider = "azure"
			if pe.Body == "" {
				pe.Body = truncate(string(respData), 256)
			}
		}
		return nil, err
	}

	return respData, nil
}

func (a *AzureDevOpsProvider) mapAzureToTask(wi *azureWorkItemDTO) *Task {
	p := PriorityMedium
	switch wi.Fields.Priority {
	case 1:
		p = PriorityUrgent
	case 2:
		p = PriorityHigh
	case 3:
		p = PriorityMedium
	case 4:
		p = PriorityLow
	}

	ref := fmt.Sprintf("AZ-%d", wi.ID)
	return DTOToTask(
		fmt.Sprintf("%d", wi.ID),
		ref,
		wi.Fields.Title,
		wi.Fields.Description,
		NormalizeStatus(wi.Fields.State),
		p,
		a.Project,
		[]string{wi.Fields.WorkItemType},
		wi.Fields.CreatedDate,
	)
}

func (a *AzureDevOpsProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	dls := a.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()
	var task *Task
	err := RetryRead(ctx, a.readRetry(), func(rctx context.Context) error {
		t, e := a.getTaskOnce(rctx, id)
		if e != nil {
			return AsTimeout("azure", "GetTask", OpGet, dls.For(OpGet), e)
		}
		task = t
		return nil
	})
	return task, err
}

func (a *AzureDevOpsProvider) getTaskOnce(ctx context.Context, id string) (*Task, error) {
	data, err := a.doRequest(ctx, "GET", fmt.Sprintf("/_apis/wit/workitems/%s?api-version=7.0", id), nil, "")
	if err != nil {
		return nil, fmt.Errorf("azure GetTask failed: %w", err)
	}

	var wi azureWorkItemDTO
	if err := json.Unmarshal(data, &wi); err != nil {
		return nil, fmt.Errorf("azure unmarshal workitem failed: %w", err)
	}

	return a.mapAzureToTask(&wi), nil
}

func (a *AzureDevOpsProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	dls := a.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()
	var tasks []*Task
	err := RetryRead(ctx, a.readRetry(), func(rctx context.Context) error {
		ts, e := a.listTasksOnce(rctx, projectID, status)
		if e != nil {
			return AsTimeout("azure", "ListTasks", OpList, dls.For(OpList), e)
		}
		tasks = ts
		return nil
	})
	return tasks, err
}

func (a *AzureDevOpsProvider) listTasksOnce(ctx context.Context, projectID string, status string) ([]*Task, error) {
	query := fmt.Sprintf("SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = '%s'", a.Project)
	if status != "" {
		query = fmt.Sprintf("%s AND [System.State] = '%s'", query, status)
	}

	wiqlReq := map[string]string{"query": query}
	data, err := a.doRequest(ctx, "POST", "/_apis/wit/wiql?api-version=7.0", wiqlReq, "")
	if err != nil {
		return nil, fmt.Errorf("azure ListTasks WIQL failed: %w", err)
	}

	var wiqlResp azureWIQLResponse
	if err := json.Unmarshal(data, &wiqlResp); err != nil {
		return nil, fmt.Errorf("azure unmarshal WIQL failed: %w", err)
	}

	var tasks []*Task
	for _, item := range wiqlResp.WorkItems {
		// Use getTaskOnce to avoid nested outer Get deadlines under list.
		t, err := a.getTaskOnce(ctx, fmt.Sprintf("%d", item.ID))
		if err != nil {
			// Fail-closed: partial hydration is not success.
			return nil, fmt.Errorf("azure ListTasks hydrate work item %d: %w", item.ID, err)
		}
		tasks = append(tasks, t)
	}

	return tasks, nil
}

func (a *AzureDevOpsProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return a.AddComment(ctx, taskID, fmt.Sprintf("Claimed by agent role [%s]", role))
}

func (a *AzureDevOpsProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	canonical := NormalizeStatus(status)
	dls := a.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := a.updateStatusOnce(writeCtx, taskID, status)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("azure", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	return AfterMutation(ctx, a, dls, "azure", "UpdateStatus", taskID, canonical, writeErr)
}

func (a *AzureDevOpsProvider) updateStatusOnce(ctx context.Context, taskID, status string) error {
	patchBody := []map[string]interface{}{
		{
			"op":    "add",
			"path":  "/fields/System.State",
			"value": status,
		},
	}
	_, err := a.doRequest(ctx, "PATCH", fmt.Sprintf("/_apis/wit/workitems/%s?api-version=7.0", taskID), patchBody, "application/json-patch+json")
	if err != nil {
		return fmt.Errorf("azure UpdateStatus failed: %w", err)
	}
	return nil
}

func (a *AzureDevOpsProvider) AddComment(ctx context.Context, taskID string, body string) error {
	dls := a.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpComment)
	defer cancel()
	patchBody := []map[string]interface{}{
		{
			"op":    "add",
			"path":  "/fields/System.History",
			"value": body,
		},
	}
	_, err := a.doRequest(ctx, "PATCH", fmt.Sprintf("/_apis/wit/workitems/%s?api-version=7.0", taskID), patchBody, "application/json-patch+json")
	if err != nil {
		err = fmt.Errorf("azure AddComment failed: %w", err)
		err = AsTimeout("azure", "AddComment", OpComment, dls.For(OpComment), err)
		if IsTimeout(err) {
			return &AmbiguousMutationError{Provider: "azure", Op: "AddComment", TaskID: taskID, WriteErr: err}
		}
		return err
	}
	return nil
}

// ListComments implements CommentReader (FAC-145 exact effect readback).
// Azure writes annotations to System.History, so a symmetric readback must
// include history revisions as well as the comments API — otherwise a
// delivered effect written to History is invisible and the coordinator
// would re-deliver it. Both sources are paginated/bounded.
func (a *AzureDevOpsProvider) ListComments(ctx context.Context, taskID string) ([]string, error) {
	dls := a.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()

	var out []string
	// 1. The comments API (newer work-item comments), PAGINATED via
	// continuation token so an effect behind many comments is still found.
	skip := 0
	for page := 0; page < maxCommentPages; page++ {
		raw, err := a.doRequest(ctx, "GET",
			fmt.Sprintf("/_apis/wit/workItems/%s/comments?api-version=7.0-preview.3&$top=200&$skip=%d", taskID, skip), nil, "application/json")
		if err != nil {
			break // fall through to History, which AddComment actually writes
		}
		var payload struct {
			TotalCount int `json:"totalCount"`
			Count      int `json:"count"`
			Comments   []struct {
				Text string `json:"text"`
			} `json:"comments"`
		}
		if jErr := json.Unmarshal(raw, &payload); jErr != nil {
			return nil, fmt.Errorf("azure ListComments decode: %w", jErr)
		}
		for _, c := range payload.Comments {
			out = append(out, c.Text)
		}
		skip += len(payload.Comments)
		// A SHORT page is the last page; a full page continues. Without
		// this a server that ignores $skip would loop forever.
		if len(payload.Comments) < 200 || (payload.TotalCount > 0 && skip >= payload.TotalCount) {
			break
		}
		if page == maxCommentPages-1 {
			return nil, fmt.Errorf("azure ListComments: exceeded %d pages — refusing a partial readback (FAC-145)", maxCommentPages)
		}
	}
	// 2. System.History revisions — the field AddComment actually writes.
	raw, err := a.doRequest(ctx, "GET", fmt.Sprintf("/_apis/wit/workItems/%s/updates?api-version=7.0&$top=200", taskID), nil, "application/json")
	if err != nil {
		if len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("azure ListComments (history) failed: %w", err)
	}
	var updates struct {
		Value []struct {
			Fields struct {
				History struct {
					NewValue string `json:"newValue"`
				} `json:"System.History"`
			} `json:"fields"`
		} `json:"value"`
	}
	if jErr := json.Unmarshal(raw, &updates); jErr != nil {
		return nil, fmt.Errorf("azure ListComments history decode: %w", jErr)
	}
	for _, u := range updates.Value {
		if v := u.Fields.History.NewValue; v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}
