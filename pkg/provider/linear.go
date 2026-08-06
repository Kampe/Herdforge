package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type LinearProvider struct {
	APIKey    string
	ProjectID string
	Client    *http.Client
	BaseURL   string
	Deadlines Deadlines
	Retry     RetryPolicy
	// BulkConcurrency bounds concurrent relation fetches in ListProjectRelations.
	// Zero => DefaultBulkRelationConcurrency. Honest O(board) fan-out — not O(1).
	BulkConcurrency int

	// relationMu serializes CreateRelation/DeleteRelation on this instance so
	// concurrent identical creates cannot both pass precheck and issue duplicate
	// mutations. Process-local only; durable task lease remains the cross-process
	// authority for multi-writer fleets.
	relationMu sync.Mutex
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

type linearWorkflowStateDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type linearIssueDTO struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    int    `json:"priority"` // 1=Urgent, 2=High, 3=Medium, 4=Low
	State       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"state"`
	Team struct {
		States struct {
			Nodes []linearWorkflowStateDTO `json:"nodes"`
		} `json:"states"`
	} `json:"team"`
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
	projectID = strings.TrimSpace(projectID)
	if projectID == "" && l != nil {
		projectID = strings.TrimSpace(l.ProjectID)
	}
	if projectID == "" {
		return nil, fmt.Errorf("linear: project id is required")
	}
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
	const pageSize = 100
	// $projectID is ID! — ProjectFilter.id is IdComparator; IdComparator.eq is Scalars["ID"]
	// (evidence: @linear/sdk@37.0.0 dist/_generated_documents.d.ts ProjectFilter/IdComparator).
	const query = `query ListIssues($projectID: ID!, $after: String) {
		issues(first: 100, after: $after, filter: { project: { id: { eq: $projectID } } }) {
			nodes { id identifier title description priority state { name } project { id } labels { nodes { name } } }
			pageInfo { hasNextPage endCursor }
		}
	}`

	want := ""
	if status != "" {
		want = NormalizeStatus(status)
	}

	acc := NewPageAccumulator()
	seenCursors := make(map[string]struct{})
	var tasks []*Task
	var after interface{}
	for page := 1; page <= DefaultMaxListPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, AsTimeout("linear", "ListTasks", OpList, l.deadlines().For(OpList), err)
		}
		var res struct {
			Data struct {
				Issues struct {
					Nodes    []linearIssueDTO `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"issues"`
			} `json:"data"`
		}
		vars := map[string]interface{}{"projectID": projectID, "after": after}
		if err := l.doGraphQL(ctx, query, vars, &res); err != nil {
			return nil, fmt.Errorf("linear ListTasks (page %d): %w", page, err)
		}

		fresh := 0
		for _, dto := range res.Data.Issues.Nodes {
			if dto.Project.ID != projectID {
				return nil, fmt.Errorf("linear ListTasks (page %d): unexpected project %q", page, dto.Project.ID)
			}
			if !acc.Add(dto.ID) {
				continue
			}
			fresh++
			canon := NormalizeStatus(dto.State.Name)
			if want != "" && canon != want {
				continue
			}
			tasks = append(tasks, linearIssueToTask(dto, canon))
		}
		if len(res.Data.Issues.Nodes) > 0 && fresh == 0 {
			return nil, fmt.Errorf("linear ListTasks (page %d): %w", page, ErrDuplicatePage)
		}
		if !res.Data.Issues.PageInfo.HasNextPage {
			sortLinearTasks(tasks)
			return tasks, nil
		}

		nextCursor := strings.TrimSpace(res.Data.Issues.PageInfo.EndCursor)
		if nextCursor == "" {
			return nil, fmt.Errorf("linear ListTasks (page %d): next page missing cursor", page)
		}
		if _, seen := seenCursors[nextCursor]; seen {
			return nil, fmt.Errorf("linear ListTasks (page %d): repeated cursor %q: %w", page, nextCursor, ErrDuplicatePage)
		}
		seenCursors[nextCursor] = struct{}{}
		after = nextCursor
	}
	return nil, fmt.Errorf("linear ListTasks: %w (maxPages=%d pageSize=%d)", ErrPaginationCap, DefaultMaxListPages, pageSize)
}

func linearIssueToTask(dto linearIssueDTO, status string) *Task {
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
		Status:      status,
		Priority:    p,
		ProjectID:   dto.Project.ID,
		Labels:      labels,
	}
}

func sortLinearTasks(tasks []*Task) {
	priorityRank := map[Priority]int{
		PriorityUrgent: 4,
		PriorityHigh:   3,
		PriorityMedium: 2,
		PriorityLow:    1,
	}
	sort.Slice(tasks, func(i, j int) bool {
		if priorityRank[tasks[i].Priority] != priorityRank[tasks[j].Priority] {
			return priorityRank[tasks[i].Priority] > priorityRank[tasks[j].Priority]
		}
		if refOrder := CompareRefs(tasks[i].Ref, tasks[j].Ref); refOrder != 0 {
			return refOrder < 0
		}
		return tasks[i].ID < tasks[j].ID
	})
}

func (l *LinearProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return l.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (l *LinearProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	canonical := NormalizeStatus(status)
	if !isCanonicalLinearStatus(canonical) {
		return fmt.Errorf("linear: unsupported canonical status %q", canonical)
	}
	dls := l.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	stateID, writeErr := l.resolveWorkflowStateID(writeCtx, taskID, canonical)
	if writeErr == nil {
		writeErr = l.updateStatusOnce(writeCtx, taskID, stateID)
	}
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("linear", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	return AfterMutation(ctx, l, dls, "linear", "UpdateStatus", taskID, canonical, writeErr)
}

func (l *LinearProvider) resolveWorkflowStateID(ctx context.Context, taskID, canonical string) (string, error) {
	query := `query ResolveIssueWorkflowStates($id: String!) { issue(id: $id) { id team { states { nodes { id name type } } } } }`
	var res struct {
		Data struct {
			Issue linearIssueDTO `json:"issue"`
		} `json:"data"`
	}
	if err := l.doGraphQL(ctx, query, map[string]interface{}{"id": taskID}, &res); err != nil {
		return "", err
	}
	if res.Data.Issue.ID == "" {
		return "", fmt.Errorf("linear: issue %q missing from workflow state resolution", taskID)
	}

	var exact, typed []linearWorkflowStateDTO
	for _, state := range res.Data.Issue.Team.States.Nodes {
		if strings.TrimSpace(state.ID) == "" {
			continue
		}
		if NormalizeStatus(state.Name) == canonical {
			exact = append(exact, state)
		} else if linearWorkflowStateCanonical(state.Type) == canonical {
			typed = append(typed, state)
		}
	}
	if len(exact) == 1 {
		return exact[0].ID, nil
	}
	if len(exact) > 1 {
		return "", fmt.Errorf("linear: issue %q has %d workflow states matching %q", taskID, len(exact), canonical)
	}
	if len(typed) == 1 {
		return typed[0].ID, nil
	}
	if len(typed) > 1 {
		return "", fmt.Errorf("linear: issue %q has %d workflow states of type %q", taskID, len(typed), canonical)
	}
	return "", fmt.Errorf("linear: issue %q has no workflow state for %q", taskID, canonical)
}

func isCanonicalLinearStatus(status string) bool {
	switch status {
	case StatusToDo, StatusInProgress, StatusInReview, StatusDone:
		return true
	default:
		return false
	}
}

func linearWorkflowStateCanonical(typ string) string {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "triage", "backlog", "unstarted":
		return StatusToDo
	case "started":
		return StatusInProgress
	case "completed":
		return StatusDone
	default:
		return StatusUnknown
	}
}

func (l *LinearProvider) updateStatusOnce(ctx context.Context, taskID, stateID string) error {
	query := `mutation UpdateIssueState($id: String!, $state: String!) { issueUpdate(id: $id, input: { stateId: $state }) { success } }`
	var res struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := l.doGraphQL(ctx, query, map[string]interface{}{"id": taskID, "state": stateID}, &res); err != nil {
		return err
	}
	if !res.Data.IssueUpdate.Success {
		return fmt.Errorf("linear: issueUpdate reported success=false")
	}
	return nil
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
		if !res.Data.CommentCreate.Success {
			return fmt.Errorf("linear: commentCreate reported success=false")
		}
		return nil
	}
	err = AsTimeout("linear", "AddComment", OpComment, dls.For(OpComment), err)
	if IsTimeout(err) {
		return &AmbiguousMutationError{Provider: "linear", Op: "AddComment", TaskID: taskID, WriteErr: err}
	}
	return err
}
