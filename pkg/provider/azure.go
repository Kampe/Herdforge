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
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
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

	resp, err := a.HTTPClient.Do(req)
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
		t, err := a.GetTask(ctx, fmt.Sprintf("%d", item.ID))
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
	got, gerr := a.GetTask(ctx, taskID)
	if gerr != nil {
		return fmt.Errorf("azure status readback after write: %w", gerr)
	}
	return VerifyStatusReadback(taskID, canonical, got.Status)
}

func (a *AzureDevOpsProvider) AddComment(ctx context.Context, taskID string, body string) error {
	patchBody := []map[string]interface{}{
		{
			"op":    "add",
			"path":  "/fields/System.History",
			"value": body,
		},
	}
	_, err := a.doRequest(ctx, "PATCH", fmt.Sprintf("/_apis/wit/workitems/%s?api-version=7.0", taskID), patchBody, "application/json-patch+json")
	if err != nil {
		return fmt.Errorf("azure AddComment failed: %w", err)
	}
	return nil
}
