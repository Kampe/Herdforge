package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type KaneoProvider struct {
	APIURL    string
	ProjectID string
	UseCLI    bool
	// APIKey authenticates HTTP calls (Bearer). Loaded from api_key_env / KANEO_API_KEY.
	// Bulk project graph snapshots prefer HTTP fan-out even when UseCLI is true
	// to avoid N CLI subprocesses (FAC-159 live-path stampede).
	APIKey string
	Client *http.Client
	// Deadlines bound every op; zero fields resolve to DefaultDeadlines.
	Deadlines Deadlines
	// Retry applies to idempotent reads only (GetTask/ListTasks).
	Retry RetryPolicy
	// BulkConcurrency bounds concurrent relation fetches in ListProjectRelations.
	// Zero => DefaultBulkRelationConcurrency.
	BulkConcurrency int
}

type KaneoLinkConfig struct {
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
}

// ResolveKaneoProjectID attempts to read project ID from .herd/kaneo.json, falling back to root .kaneo.json
func ResolveKaneoProjectID(rootDir string) string {
	paths := []string{
		filepath.Join(rootDir, ".herd", "kaneo.json"),
		filepath.Join(rootDir, ".kaneo.json"),
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			var link KaneoLinkConfig
			if err := json.Unmarshal(data, &link); err == nil && link.Project != "" {
				return link.Project
			}
		}
	}
	return ""
}

func NewKaneoProvider(apiURL string, projectID string, useCLI bool) *KaneoProvider {
	if projectID == "" {
		projectID = ResolveKaneoProjectID(".")
	}
	key := strings.TrimSpace(os.Getenv("KANEO_API_KEY"))
	return &KaneoProvider{
		APIURL:    apiURL,
		ProjectID: projectID,
		UseCLI:    useCLI,
		APIKey:    key,
		Client:    defaultHTTPClient(),
		Deadlines: DefaultDeadlines(),
		Retry:     DefaultReadRetry(),
	}
}

// authorizeKaneo sets Bearer auth when an API key is configured.
func (k *KaneoProvider) authorizeKaneo(req *http.Request) {
	if k == nil || req == nil {
		return
	}
	key := strings.TrimSpace(k.APIKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("KANEO_API_KEY"))
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func (k *KaneoProvider) deadlines() Deadlines {
	if k == nil {
		return DefaultDeadlines()
	}
	return k.Deadlines.Normalize()
}

func (k *KaneoProvider) readRetry() RetryPolicy {
	if k == nil {
		return DefaultReadRetry()
	}
	return k.Retry.normalize()
}

func (k *KaneoProvider) httpClient() *http.Client {
	if k != nil && k.Client != nil {
		return k.Client
	}
	return defaultHTTPClient()
}

// kaneoLabel accepts both API object form {"name":"x"} and CLI string form "x".
type kaneoLabel struct {
	Name string
}

func (l *kaneoLabel) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		l.Name = s
		return nil
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	l.Name = obj.Name
	return nil
}

type kaneoTaskDTO struct {
	ID          string       `json:"id"`
	Ref         string       `json:"ref"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Status      string       `json:"status"`
	Priority    string       `json:"priority"`
	ProjectId   string       `json:"projectId"`
	CreatedAt   string       `json:"createdAt"`
	Labels      []kaneoLabel `json:"labels"`
}

func dtoToTask(dto kaneoTaskDTO) *Task {
	labels := make([]string, 0, len(dto.Labels))
	for _, l := range dto.Labels {
		labels = append(labels, l.Name)
	}
	createdAt, _ := time.Parse(time.RFC3339, dto.CreatedAt)
	return &Task{
		ID:          dto.ID,
		Ref:         dto.Ref,
		Title:       dto.Title,
		Description: dto.Description,
		Status:      NormalizeStatus(dto.Status),
		Priority:    Priority(dto.Priority),
		ProjectID:   dto.ProjectId,
		Labels:      labels,
		CreatedAt:   createdAt,
	}
}

// kaneoTaskMatches reports whether dto is exactly the requested task by id or ref.
// Fail-closed: empty wantID never matches (callers must request a concrete id).
func kaneoTaskMatches(dto kaneoTaskDTO, wantID string) bool {
	if wantID == "" {
		return false
	}
	return dto.ID == wantID || dto.Ref == wantID
}

// decodeKaneoTaskBody accepts a single task object or a JSON array of tasks.
// Both shapes require an exact match on the requested id or ref — a sole
// nonmatching array element or an object for a different task is a hard error
// so status readback cannot confirm the wrong card.
func decodeKaneoTaskBody(statusCode int, body []byte, wantID string) (kaneoTaskDTO, error) {
	// Shared fail-closed gate for non-2xx and structured error payloads.
	if err := DecodeJSONBytes(statusCode, body, nil); err != nil {
		return kaneoTaskDTO{}, err
	}
	if wantID == "" {
		return kaneoTaskDTO{}, fmt.Errorf("kaneo task decode: requested id is required")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return kaneoTaskDTO{}, fmt.Errorf("empty task body")
	}
	if trimmed[0] == '[' {
		var dtos []kaneoTaskDTO
		if err := json.Unmarshal(trimmed, &dtos); err != nil {
			return kaneoTaskDTO{}, fmt.Errorf("decode JSON: %w", err)
		}
		if len(dtos) == 0 {
			return kaneoTaskDTO{}, fmt.Errorf("kaneo task not found: empty list")
		}
		for _, d := range dtos {
			if kaneoTaskMatches(d, wantID) {
				return d, nil
			}
		}
		// Sole nonmatching element is still a hard error (not implicit accept).
		return kaneoTaskDTO{}, fmt.Errorf("kaneo task %q not found in list of %d", wantID, len(dtos))
	}
	var dto kaneoTaskDTO
	if err := json.Unmarshal(trimmed, &dto); err != nil {
		return kaneoTaskDTO{}, fmt.Errorf("decode JSON: %w", err)
	}
	if !kaneoTaskMatches(dto, wantID) {
		return kaneoTaskDTO{}, fmt.Errorf("kaneo task id mismatch: requested %q got id=%q ref=%q",
			wantID, dto.ID, dto.Ref)
	}
	return dto, nil
}

func (k *KaneoProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()

	var task *Task
	err := RetryRead(ctx, k.readRetry(), func(rctx context.Context) error {
		t, e := k.getTaskOnce(rctx, id)
		if e != nil {
			return AsTimeout("kaneo", "GetTask", OpGet, dls.For(OpGet), e)
		}
		task = t
		return nil
	})
	return task, err
}

// kaneoRunCLI is the CLI runner for Kaneo production UseCLI mode. Tests may
// swap it for a hermetic counter; production uses process-group RunCLI.
var kaneoRunCLI = RunCLI

func (k *KaneoProvider) getTaskOnce(ctx context.Context, id string) (*Task, error) {
	if k.UseCLI {
		res, err := kaneoRunCLI(ctx, "kaneo", "task", "get", id, "--json")
		if err != nil {
			return nil, fmt.Errorf("kaneo task get: %w", err)
		}
		dto, err := decodeKaneoTaskBody(http.StatusOK, res.Stdout, id)
		if err != nil {
			if pe, ok := err.(*ProviderError); ok {
				pe.Provider = "kaneo"
				pe.Op = "GetTask"
			}
			return nil, fmt.Errorf("kaneo task get: %w", err)
		}
		return dtoToTask(dto), nil
	}

	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	dto, err := decodeKaneoTaskBody(resp.StatusCode, body, id)
	if err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "GetTask"
		}
		return nil, err
	}
	return dtoToTask(dto), nil
}

func (k *KaneoProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	if projectID == "" {
		projectID = k.ProjectID
	}
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()

	var tasks []*Task
	err := RetryRead(ctx, k.readRetry(), func(rctx context.Context) error {
		t, e := k.listTasksOnce(rctx, projectID, status)
		if e != nil {
			return AsTimeout("kaneo", "ListTasks", OpList, dls.For(OpList), e)
		}
		tasks = t
		return nil
	})
	return tasks, err
}

func (k *KaneoProvider) listTasksOnce(ctx context.Context, projectID, status string) ([]*Task, error) {
	if k.UseCLI {
		// Terminate only on EMPTY page; short pages continue. Duplicate pages
		// and the page cap without empty termination are hard errors.
		// Server may cap below --limit (observed 99/100), so short-page stop
		// hides later cards (FAC-106 / board-done regressions).
		const pageSize = 100
		var all []kaneoTaskDTO
		acc := NewPageAccumulator()
		for page := 1; page <= DefaultMaxListPages; page++ {
			if err := ctx.Err(); err != nil {
				return nil, AsTimeout("kaneo", "ListTasks", OpList, k.deadlines().For(OpList), err)
			}
			args := []string{"task", "list", "--project", projectID, "--json",
				"--limit", fmt.Sprint(pageSize), "--page", fmt.Sprint(page)}
			if status != "" {
				args = append(args, "--status", status)
			}
			res, err := kaneoRunCLI(ctx, "kaneo", args...)
			if err != nil {
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, err)
			}
			var dtos []kaneoTaskDTO
			if err := DecodeJSONBytes(http.StatusOK, res.Stdout, &dtos); err != nil {
				if pe, ok := err.(*ProviderError); ok {
					pe.Provider = "kaneo"
					pe.Op = "ListTasks"
				}
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, err)
			}
			fresh := 0
			for _, d := range dtos {
				if !acc.Add(d.ID) {
					continue
				}
				all = append(all, d)
				fresh++
			}
			dec := DecidePagination(len(dtos), fresh)
			switch dec {
			case PageStopEmpty:
				return filterTasks(all, status), nil
			case PageStopDuplicate:
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, ErrDuplicatePage)
			}
		}
		return nil, fmt.Errorf("kaneo task list: %w (maxPages=%d)", ErrPaginationCap, DefaultMaxListPages)
	}

	url := fmt.Sprintf("%s/api/task?projectId=%s", k.APIURL, projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	var dtos []kaneoTaskDTO
	if err := DecodeJSONResponse(resp, &dtos); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "ListTasks"
		}
		return nil, err
	}
	return filterTasks(dtos, status), nil
}

func filterTasks(dtos []kaneoTaskDTO, status string) []*Task {
	var tasks []*Task
	want := ""
	if status != "" {
		want = NormalizeStatus(status)
	}
	for _, dto := range dtos {
		t := dtoToTask(dto)
		if want != "" && t.Status != want {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks
}

func (k *KaneoProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return k.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (k *KaneoProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	// Write the canonical lifecycle status so readback compares like-for-like
	// against dtoToTask/NormalizeStatus (production Kaneo CLI + HTTP).
	canonical := NormalizeStatus(status)
	dls := k.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := k.updateStatusOnce(writeCtx, taskID, canonical)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("kaneo", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	// Parent ctx (not the expired write child) for readback / reconcile.
	return AfterMutation(ctx, k, dls, "kaneo", "UpdateStatus", taskID, canonical, writeErr)
}

func (k *KaneoProvider) updateStatusOnce(ctx context.Context, taskID, status string) error {
	if k.UseCLI {
		res, err := kaneoRunCLI(ctx, "kaneo", "task", "status", taskID, status, "--project", k.ProjectID)
		if err != nil {
			msg := cliErrMsg(res)
			if msg != "" {
				return fmt.Errorf("kaneo task status: %s: %w", msg, err)
			}
			return fmt.Errorf("kaneo task status: %w", err)
		}
		return nil
	}

	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, taskID)
	payload := map[string]string{"status": status}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "UpdateStatus"
		}
		return err
	}
	return nil
}

func (k *KaneoProvider) AddComment(ctx context.Context, taskID string, body string) error {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpComment)
	defer cancel()

	err := k.addCommentOnce(ctx, taskID, body)
	if err == nil {
		return nil
	}
	err = AsTimeout("kaneo", "AddComment", OpComment, dls.For(OpComment), err)
	if IsTimeout(err) {
		// Comments are not status-reconcilable; never blind-retry.
		return &AmbiguousMutationError{
			Provider: "kaneo",
			Op:       "AddComment",
			TaskID:   taskID,
			WriteErr: err,
		}
	}
	return err
}

func (k *KaneoProvider) addCommentOnce(ctx context.Context, taskID, body string) error {
	if k.UseCLI {
		// Production Kaneo is multi-project; pin --project when configured
		// (matches status/list CLI paths).
		args := []string{"task", "comment", "add", taskID, body}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			msg := cliErrMsg(res)
			if msg != "" {
				return fmt.Errorf("kaneo task comment: %s: %w", msg, err)
			}
			return fmt.Errorf("kaneo task comment: %w", err)
		}
		return nil
	}

	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, taskID)
	payload := map[string]string{"body": body}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "AddComment"
		}
		return err
	}
	return nil
}

func cliErrMsg(res *CLIResult) string {
	if res == nil {
		return ""
	}
	msg := strings.TrimSpace(string(res.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(res.Stdout))
	}
	return msg
}

// defaultHTTPClient returns a client whose transport timeout is independent of
// any single caller context — a safety net for hung bodies/connections.
// Per-op bounds still come from WithOpDeadline on the request context.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: DefaultDeadlines().Max() + 5*time.Second}
}
