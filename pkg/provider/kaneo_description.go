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

// UpdateDescription writes one task description through Kaneo's scoped
// updateTaskDescription HTTP operation. Acknowledgement is the PUT response
// itself; callers must not wait on unrelated CLI presentation enrichment.
func (k *KaneoProvider) UpdateDescription(ctx context.Context, taskID, description string) error {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	defer cancel()
	err := k.updateDescriptionOnce(ctx, taskID, description)
	if err != nil {
		return AsTimeout("kaneo", "UpdateDescription", OpMutate, dls.For(OpMutate), err)
	}
	return nil
}

func (k *KaneoProvider) updateDescriptionOnce(ctx context.Context, taskID, description string) error {
	if k == nil {
		return fmt.Errorf("kaneo UpdateDescription: nil provider")
	}
	taskID = strings.TrimSpace(taskID)
	projectID := strings.TrimSpace(k.ProjectID)
	apiURL := strings.TrimRight(strings.TrimSpace(k.APIURL), "/")
	if taskID == "" {
		return fmt.Errorf("kaneo UpdateDescription: task id required")
	}
	if projectID == "" {
		return fmt.Errorf("kaneo UpdateDescription: project identity required")
	}
	if apiURL == "" {
		return fmt.Errorf("kaneo UpdateDescription: APIURL required")
	}
	payload, err := json.Marshal(map[string]string{"description": description})
	if err != nil {
		return fmt.Errorf("kaneo UpdateDescription: marshal: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/task/description/%s", apiURL, url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	k.authorizeKaneo(req)
	AttachFenceHeaders(ctx, req.Header.Set)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_, err = decodeKaneoDescriptionAck(resp.StatusCode, body, taskID, projectID, description, true)
	if pe, ok := err.(*ProviderError); ok {
		pe.Provider, pe.Op = "kaneo", "UpdateDescription"
	}
	return err
}

// ReadDescription returns one task description from the exact HTTP task
// resource. It never waits on CLI --full / project-detail enrichment.
func (k *KaneoProvider) ReadDescription(ctx context.Context, taskID string) (string, error) {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpReadback)
	defer cancel()
	desc, err := k.readDescriptionOnce(ctx, taskID)
	if err != nil {
		return "", AsTimeout("kaneo", "ReadDescription", OpReadback, dls.For(OpReadback), err)
	}
	return desc, nil
}

func (k *KaneoProvider) readDescriptionOnce(ctx context.Context, taskID string) (string, error) {
	if k == nil {
		return "", fmt.Errorf("kaneo ReadDescription: nil provider")
	}
	taskID = strings.TrimSpace(taskID)
	projectID := strings.TrimSpace(k.ProjectID)
	apiURL := strings.TrimRight(strings.TrimSpace(k.APIURL), "/")
	if taskID == "" {
		return "", fmt.Errorf("kaneo ReadDescription: task id required")
	}
	if projectID == "" {
		return "", fmt.Errorf("kaneo ReadDescription: project identity required")
	}
	if apiURL == "" {
		return "", fmt.Errorf("kaneo ReadDescription: APIURL required")
	}
	endpoint := fmt.Sprintf("%s/api/task/%s", apiURL, url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	dto, err := decodeKaneoDescriptionAck(resp.StatusCode, body, taskID, projectID, "", false)
	if pe, ok := err.(*ProviderError); ok {
		pe.Provider, pe.Op = "kaneo", "ReadDescription"
	}
	if err != nil {
		return "", err
	}
	return dto.Description, nil
}

// DecodeKaneoDescription validates a task JSON body for exact task and project
// identity, rejecting HTTP error bodies even under 200.
func DecodeKaneoDescription(statusCode int, body []byte, wantID, wantProject string) (string, error) {
	dto, err := decodeKaneoDescriptionAck(statusCode, body, wantID, wantProject, "", false)
	if err != nil {
		return "", err
	}
	return dto.Description, nil
}

func decodeKaneoDescriptionAck(statusCode int, body []byte, wantID, wantProject, wantDescription string, requireDescription bool) (kaneoTaskDTO, error) {
	if strings.TrimSpace(wantProject) == "" {
		return kaneoTaskDTO{}, fmt.Errorf("kaneo description: project identity required")
	}
	dto, err := decodeKaneoTaskBody(statusCode, body, wantID)
	if err != nil {
		return kaneoTaskDTO{}, err
	}
	if dto.ProjectId != wantProject {
		return kaneoTaskDTO{}, fmt.Errorf("kaneo description project mismatch: requested %q got %q", wantProject, dto.ProjectId)
	}
	if requireDescription && dto.Description != wantDescription {
		return kaneoTaskDTO{}, fmt.Errorf("kaneo description acknowledgement mismatch")
	}
	return dto, nil
}
