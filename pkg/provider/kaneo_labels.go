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

var ErrRedundantLabelAttach = fmt.Errorf("redundant label attach returned null success")

func (k *KaneoProvider) LabelMutationAuthority() (string, error) {
	if k == nil || strings.TrimSpace(k.ProjectID) == "" {
		return "", fmt.Errorf("kaneo label authority: immutable project identity required")
	}
	origin := strings.TrimSpace(k.KeyTrustedOrigin)
	if origin == "" {
		origin = ResolveKaneoProfileCred().TrustedOrigin
	}
	if origin == "" {
		var err error
		origin, err = canonicalizeHTTPOrigin(k.APIURL)
		if err != nil {
			return "", fmt.Errorf("kaneo label authority: authenticated origin unavailable: %w", err)
		}
	}
	return "kaneo|" + origin + "|project|" + k.ProjectID, nil
}

func decodeKaneoLabels(status int, body []byte) ([]TaskLabel, error) {
	if err := DecodeJSONBytes(status, body, nil); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var rows []TaskLabel
	if len(bytes.TrimSpace(body)) > 0 && bytes.TrimSpace(body)[0] == '[' {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("decode labels: %w", err)
		}
		return rows, nil
	}
	var one TaskLabel
	if err := json.Unmarshal(body, &one); err != nil {
		return nil, fmt.Errorf("decode label: %w", err)
	}
	if one.ID != "" {
		rows = append(rows, one)
	}
	return rows, nil
}

func (k *KaneoProvider) ListTaskLabels(ctx context.Context, taskID string) ([]TaskLabel, error) {
	if k.UseCLI {
		args := []string{"task", "label", "list", taskID, "--json"}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			return nil, fmt.Errorf("kaneo task label list: %w", err)
		}
		return decodeKaneoLabels(http.StatusOK, res.Stdout)
	}
	return nil, fmt.Errorf("kaneo task label list over HTTP is unsupported without a proven contract")
}

// ListTaskLabelsBulk uses the labels already projected by task list. It is a
// single provider operation instead of one task label command per identity.
// Missing identities are returned as an explicit truncated result so callers
// cannot mistake a partial board scan for a complete one.
func (k *KaneoProvider) ListTaskLabelsBulk(ctx context.Context, taskIDs []string) (BulkTaskLabels, error) {
	result := BulkTaskLabels{
		Labels:    make(map[string][]TaskLabel, len(taskIDs)),
		Requested: len(taskIDs),
	}
	seen := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return BulkTaskLabels{}, fmt.Errorf("kaneo bulk task label list: task identity required")
		}
		if _, ok := seen[id]; ok {
			return BulkTaskLabels{}, fmt.Errorf("kaneo bulk task label list: duplicate task identity %q", id)
		}
		seen[id] = struct{}{}
		result.Labels[id] = nil
	}
	if len(taskIDs) == 0 {
		result.Complete = true
		return result, nil
	}

	tasks, err := k.ListTasks(ctx, "", "")
	if err != nil {
		return BulkTaskLabels{}, fmt.Errorf("kaneo bulk task label list: %w", err)
	}
	for _, task := range tasks {
		if task == nil {
			continue
		}
		var requested string
		if _, ok := seen[task.ID]; ok {
			requested = task.ID
		} else if _, ok := seen[task.Ref]; ok {
			requested = task.Ref
		}
		if requested == "" {
			continue
		}
		labels := make([]TaskLabel, 0, len(task.Labels))
		for _, name := range task.Labels {
			labels = append(labels, TaskLabel{Name: name, TaskID: task.ID})
		}
		result.Labels[requested] = labels
		result.Retrieved++
	}
	result.Complete = result.Retrieved == result.Requested
	result.Truncated = !result.Complete
	return result, nil
}

func (k *KaneoProvider) CreateTaskLabel(ctx context.Context, taskID, name string) (TaskLabel, error) {
	if name == "" {
		return TaskLabel{}, fmt.Errorf("label name required")
	}
	payload, _ := json.Marshal(map[string]string{"name": name, "taskId": taskID})
	if k.UseCLI {
		before, err := k.listWorkspaceLabels(ctx)
		if err != nil {
			return TaskLabel{}, fmt.Errorf("kaneo label create precondition: %w", err)
		}
		args := []string{"label", "create", "--color", "#808080", name, "--json"}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			return TaskLabel{}, fmt.Errorf("kaneo task label add: %w", err)
		}
		rows, err := decodeKaneoLabels(http.StatusOK, res.Stdout)
		if err != nil || len(rows) != 1 {
			if err == nil {
				err = fmt.Errorf("label create returned no single label")
			}
			return TaskLabel{}, err
		}
		if rows[0].ID == "" || rows[0].Name != name {
			return TaskLabel{}, fmt.Errorf("label create identity mismatch")
		}
		k.proofMu.Lock()
		if k.pendingCreates == nil {
			k.pendingCreates = make(map[string]map[string]TaskLabel)
		}
		k.pendingCreates[rows[0].ID] = before
		k.proofMu.Unlock()
		return rows[0], nil
	}
	_ = payload
	return TaskLabel{}, fmt.Errorf("kaneo label create over HTTP is unsupported without a proven contract")
}

func (k *KaneoProvider) listWorkspaceLabels(ctx context.Context) (map[string]TaskLabel, error) {
	args := []string{"label", "list", "--json"}
	if k.ProjectID != "" {
		args = append(args, "--project", k.ProjectID)
	}
	res, err := kaneoRunCLI(ctx, "kaneo", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeKaneoLabels(http.StatusOK, res.Stdout)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TaskLabel, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			out[row.ID] = row
		}
	}
	return out, nil
}

// ProveLabelCreation binds compensation to the workspace pre/post identity:
// the row must have been absent before this provider's create and present with
// the exact name and source-free state afterward. An existing Kaneo orphan is
// therefore never detached or deleted by the transaction.
func (k *KaneoProvider) ProveLabelCreation(ctx context.Context, created TaskLabel, targetID, name string, opts LabelRepairOptions) error {
	if !k.UseCLI {
		return fmt.Errorf("kaneo label creation proof over HTTP is unsupported")
	}
	k.proofMu.Lock()
	before, ok := k.pendingCreates[created.ID]
	delete(k.pendingCreates, created.ID)
	k.proofMu.Unlock()
	if !ok {
		if reader, readable := opts.Evidence.(LabelEvidenceReader); readable {
			if evidence, err := reader.ReadLabelRepairEvidence(opts.TransactionID, opts.Generation, "created"); err == nil && evidence.CreatedLabelID == created.ID {
				return fmt.Errorf("kaneo label creation proof: crash-recovery orphan %s preserved; workspace preimage must be re-established", created.ID)
			}
		}
		return fmt.Errorf("kaneo label creation proof: unknown transaction identity; orphan %s preserved", created.ID)
	}
	if _, existed := before[created.ID]; existed {
		return fmt.Errorf("kaneo label creation proof: identity existed before create")
	}
	after, err := k.listWorkspaceLabels(ctx)
	if err != nil {
		return err
	}
	row, present := after[created.ID]
	if !present || created.ID == "" || row.Name != name || row.TaskID != "" || created.Name != name || created.TaskID != "" || targetID == "" {
		return fmt.Errorf("kaneo label creation proof: post-create identity mismatch")
	}
	return nil
}

func (k *KaneoProvider) AttachTaskLabel(ctx context.Context, taskID, labelID string) error {
	if k.UseCLI {
		args := []string{"task", "label", "add", taskID, labelID}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			return fmt.Errorf("kaneo task label attach: %w", err)
		}
		trimmed := bytes.TrimSpace(res.Stdout)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return ErrRedundantLabelAttach
		}
		rows, err := decodeKaneoLabels(http.StatusOK, trimmed)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != labelID || rows[0].TaskID != taskID {
			return fmt.Errorf("kaneo label attach identity mismatch")
		}
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"taskId": taskID})
	rows, err := k.kaneoLabelHTTP(ctx, http.MethodPost, fmt.Sprintf("%s/api/label/%s/task", k.APIURL, url.PathEscape(labelID)), payload)
	if err != nil {
		return err
	}
	if len(rows) != 1 || rows[0].ID != labelID || rows[0].TaskID != taskID {
		return fmt.Errorf("kaneo label attach identity/readback mismatch")
	}
	return nil
}

func (k *KaneoProvider) DetachTaskLabel(ctx context.Context, labelID string) error {
	if k.UseCLI {
		args := []string{"task", "label", "delete", labelID}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			return fmt.Errorf("kaneo task label delete: %w", err)
		}
		rows, err := decodeKaneoLabels(http.StatusOK, res.Stdout)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != labelID {
			return fmt.Errorf("kaneo label detach identity mismatch")
		}
		return nil
	}
	rows, err := k.kaneoLabelHTTP(ctx, http.MethodDelete, fmt.Sprintf("%s/api/label/%s/task", k.APIURL, url.PathEscape(labelID)), nil)
	if err != nil {
		return err
	}
	if len(rows) != 1 || rows[0].ID != labelID {
		return fmt.Errorf("kaneo label detach identity/readback mismatch")
	}
	return nil
}

func (k *KaneoProvider) DeleteTaskLabel(ctx context.Context, labelID string) error {
	if k.UseCLI {
		args := []string{"label", "delete", labelID}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			return fmt.Errorf("kaneo task label delete: %w", err)
		}
		rows, err := decodeKaneoLabels(http.StatusOK, res.Stdout)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != labelID {
			return fmt.Errorf("kaneo label delete identity mismatch")
		}
		return nil
	}
	return fmt.Errorf("kaneo workspace label delete over HTTP is unsupported without a proven contract")
}

func (k *KaneoProvider) kaneoLabelHTTP(ctx context.Context, method, endpoint string, payload []byte) ([]TaskLabel, error) {
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	return decodeKaneoLabels(resp.StatusCode, raw)
}
