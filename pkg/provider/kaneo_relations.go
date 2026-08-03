package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// kaneoRelationDTO matches Kaneo CLI/API relation JSON.
type kaneoRelationDTO struct {
	ID           string `json:"id"`
	SourceTaskID string `json:"sourceTaskId"`
	TargetTaskID string `json:"targetTaskId"`
	RelationType string `json:"relationType"`
	CreatedAt    string `json:"createdAt"`
	// Error field present on some 2xx error bodies.
	Error string `json:"error"`
}

func (d kaneoRelationDTO) toRelation() Relation {
	created, _ := time.Parse(time.RFC3339, d.CreatedAt)
	return Relation{
		ID:           d.ID,
		SourceTaskID: d.SourceTaskID,
		TargetTaskID: d.TargetTaskID,
		Type:         RelationType(strings.ToLower(strings.TrimSpace(d.RelationType))),
		CreatedAt:    created,
	}
}

// ListRelations lists Kaneo relations for a task (as source or target).
func (k *KaneoProvider) ListRelations(ctx context.Context, taskID string) ([]Relation, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("kaneo ListRelations: empty task id")
	}
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()

	var out []Relation
	err := RetryRead(ctx, k.readRetry(), func(rctx context.Context) error {
		rels, e := k.listRelationsOnce(rctx, taskID)
		if e != nil {
			return AsTimeout("kaneo", "ListRelations", OpList, dls.For(OpList), e)
		}
		out = rels
		return nil
	})
	return out, err
}

func (k *KaneoProvider) listRelationsOnce(ctx context.Context, taskID string) ([]Relation, error) {
	if k.UseCLI {
		args := []string{"task", "rel", "list", taskID, "--json"}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			msg := cliErrMsg(res)
			if msg != "" {
				return nil, fmt.Errorf("kaneo task rel list: %s: %w", msg, err)
			}
			return nil, fmt.Errorf("kaneo task rel list: %w", err)
		}
		// Reject 200-shaped error objects even when CLI exits 0.
		if pe := kaneoRelationErrorBody(res.Stdout); pe != nil {
			pe.Provider = "kaneo"
			pe.Op = "ListRelations"
			return nil, pe
		}
		var dtos []kaneoRelationDTO
		if err := DecodeJSONBytes(http.StatusOK, res.Stdout, &dtos); err != nil {
			if pe, ok := err.(*ProviderError); ok {
				pe.Provider = "kaneo"
				pe.Op = "ListRelations"
			}
			return nil, fmt.Errorf("kaneo task rel list: %w", err)
		}
		out := make([]Relation, 0, len(dtos))
		for _, d := range dtos {
			out = append(out, d.toRelation())
		}
		return out, nil
	}

	url := fmt.Sprintf("%s/api/task/%s/relations", k.APIURL, taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if pe := kaneoRelationErrorBody(body); pe != nil {
		pe.Provider = "kaneo"
		pe.Op = "ListRelations"
		pe.StatusCode = resp.StatusCode
		return nil, pe
	}
	// Synthesize a Response-like decode via DecodeJSONBytes (status + body).
	var dtos []kaneoRelationDTO
	if err := DecodeJSONBytes(resp.StatusCode, body, &dtos); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "ListRelations"
		}
		return nil, err
	}
	out := make([]Relation, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, d.toRelation())
	}
	return out, nil
}

// kaneoRelationErrorBody detects {"error":"..."} under HTTP-equivalent 200 CLI output.
func kaneoRelationErrorBody(raw []byte) *ProviderError {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || trim[0] != '{' {
		return nil
	}
	var probe struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(trim, &probe); err != nil {
		return nil
	}
	if strings.TrimSpace(probe.Error) == "" {
		return nil
	}
	return &ProviderError{
		StatusCode: http.StatusOK,
		Message:    probe.Error,
		Body:       string(trim),
	}
}

// CreateRelation creates a directed relation with dual-end readback.
// Rejects self-edges and unknown types. Ambiguous create is reconciled against
// both source and target listings so retries never duplicate edges.
func (k *KaneoProvider) CreateRelation(ctx context.Context, sourceID, targetID string, typ RelationType) (*Relation, error) {
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(targetID) == "" {
		return nil, fmt.Errorf("kaneo CreateRelation: source and target required")
	}
	if sourceID == targetID {
		return nil, fmt.Errorf("kaneo CreateRelation: self-edge rejected")
	}
	if typ == "" {
		return nil, fmt.Errorf("kaneo CreateRelation: relation type required (blocks|related|subtask)")
	}
	if !ValidRelationType(typ) {
		return nil, fmt.Errorf("kaneo CreateRelation: unknown type %q", typ)
	}

	dls := k.deadlines()

	// Fresh bounded pre-check on source: existing edge → return (idempotent).
	if existing, err := k.findRelationFresh(ctx, dls, sourceID, targetID, typ); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	created, writeErr := k.createRelationOnce(writeCtx, sourceID, targetID, typ)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("kaneo", "CreateRelation", OpMutate, dls.For(OpMutate), writeErr)
		if IsTimeout(writeErr) || IsAmbiguous(writeErr) {
			// Reconcile: if the edge landed, return it; else ambiguous.
			if existing, ferr := k.findRelationFresh(ctx, dls, sourceID, targetID, typ); ferr == nil && existing != nil {
				return existing, nil
			}
			return nil, &AmbiguousMutationError{
				Provider: "kaneo",
				Op:       "CreateRelation",
				TaskID:   sourceID + "->" + targetID,
				WriteErr: writeErr,
			}
		}
		// Non-timeout: still reconcile races where create failed because exists.
		if existing, ferr := k.findRelationFresh(ctx, dls, sourceID, targetID, typ); ferr == nil && existing != nil {
			return existing, nil
		}
		return nil, writeErr
	}

	// Dual-end readback with separate fresh bounded contexts.
	srcRel, err := k.findRelationFresh(ctx, dls, sourceID, targetID, typ)
	if err != nil {
		return nil, fmt.Errorf("kaneo CreateRelation source readback: %w", err)
	}
	tgtRel, err := k.findRelationFresh(ctx, dls, targetID, sourceID, typ) // list target side
	if err != nil {
		return nil, fmt.Errorf("kaneo CreateRelation target readback: %w", err)
	}
	// Target listing may include the same edge (source→target); search both.
	var fromTarget *Relation
	if tgtRel != nil && tgtRel.SourceTaskID == sourceID && tgtRel.TargetTaskID == targetID {
		fromTarget = tgtRel
	} else {
		// Re-list target and scan for our directed edge.
		fromTarget, err = k.findRelationOnTask(ctx, dls, targetID, sourceID, targetID, typ)
		if err != nil {
			return nil, fmt.Errorf("kaneo CreateRelation target list: %w", err)
		}
	}
	if srcRel == nil {
		return nil, fmt.Errorf("kaneo CreateRelation readback: edge missing on source after create")
	}
	if fromTarget == nil {
		return nil, fmt.Errorf("kaneo CreateRelation readback: edge missing on target after create")
	}
	if created != nil && created.ID != "" && srcRel.ID != "" && created.ID != srcRel.ID {
		// Prefer list readback.
	}
	return srcRel, nil
}

func (k *KaneoProvider) findRelationFresh(ctx context.Context, dls Deadlines, listTaskID, otherID string, typ RelationType) (*Relation, error) {
	// When listTaskID is source, match source→target; used for pre-check.
	return k.findRelationOnTask(ctx, dls, listTaskID, listTaskID, otherID, typ)
}

func (k *KaneoProvider) findRelationOnTask(ctx context.Context, dls Deadlines, listTaskID, sourceID, targetID string, typ RelationType) (*Relation, error) {
	readCtx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()
	rels, err := k.ListRelations(readCtx, listTaskID)
	if err != nil {
		return nil, AsTimeout("kaneo", "ListRelations", OpList, dls.For(OpList), err)
	}
	for i := range rels {
		r := &rels[i]
		if r.SourceTaskID == sourceID && r.TargetTaskID == targetID && r.Type == typ {
			return r, nil
		}
	}
	return nil, nil
}

func (k *KaneoProvider) createRelationOnce(ctx context.Context, sourceID, targetID string, typ RelationType) (*Relation, error) {
	if k.UseCLI {
		args := []string{"task", "rel", "add", "--type", string(typ), sourceID, targetID, "--json"}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			msg := cliErrMsg(res)
			if msg != "" {
				return nil, fmt.Errorf("kaneo task rel add: %s: %w", msg, err)
			}
			return nil, fmt.Errorf("kaneo task rel add: %w", err)
		}
		if pe := kaneoRelationErrorBody(res.Stdout); pe != nil {
			pe.Provider = "kaneo"
			pe.Op = "CreateRelation"
			return nil, pe
		}
		var dto kaneoRelationDTO
		if len(res.Stdout) > 0 && json.Valid(res.Stdout) {
			if err := json.Unmarshal(res.Stdout, &dto); err == nil && dto.ID != "" {
				r := dto.toRelation()
				return &r, nil
			}
		}
		return &Relation{SourceTaskID: sourceID, TargetTaskID: targetID, Type: typ}, nil
	}

	url := fmt.Sprintf("%s/api/task/relation", k.APIURL)
	payload := map[string]string{
		"sourceTaskId": sourceID,
		"targetTaskId": targetID,
		"relationType": string(typ),
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	var dto kaneoRelationDTO
	if err := DecodeJSONResponse(resp, &dto); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "CreateRelation"
		}
		return nil, err
	}
	if strings.TrimSpace(dto.Error) != "" {
		return nil, &ProviderError{
			Provider:   "kaneo",
			Op:         "CreateRelation",
			StatusCode: resp.StatusCode,
			Message:    dto.Error,
		}
	}
	r := dto.toRelation()
	return &r, nil
}

// DeleteRelation deletes a relation and verifies absence on BOTH source and target.
// sourceID and targetID are the captured endpoints (required).
// Ambiguous delete/timeouts return AmbiguousMutationError — never success.
func (k *KaneoProvider) DeleteRelation(ctx context.Context, relationID, sourceID, targetID string) error {
	if strings.TrimSpace(relationID) == "" {
		return fmt.Errorf("kaneo DeleteRelation: empty relation id")
	}
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(targetID) == "" {
		return fmt.Errorf("kaneo DeleteRelation: source and target endpoints required for dual readback")
	}
	dls := k.deadlines()

	// Capture: confirm relation is visible on source before delete (endpoint truth).
	pre, err := k.relationPresentFresh(ctx, dls, sourceID, relationID)
	if err != nil {
		return fmt.Errorf("kaneo DeleteRelation pre-capture source: %w", err)
	}
	if !pre {
		// Maybe already gone — verify target too; if absent both, treat as success (idempotent).
		onTgt, terr := k.relationPresentFresh(ctx, dls, targetID, relationID)
		if terr != nil {
			return fmt.Errorf("kaneo DeleteRelation pre-capture target: %w", terr)
		}
		if !onTgt {
			return nil // already deleted
		}
		// Present only on target — still delete by id.
	}

	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := k.deleteRelationOnce(writeCtx, relationID)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("kaneo", "DeleteRelation", OpMutate, dls.For(OpMutate), writeErr)
		if IsTimeout(writeErr) || IsAmbiguous(writeErr) {
			// Reconcile: if absent both ends, success; else ambiguous.
			onSrc, e1 := k.relationPresentFresh(ctx, dls, sourceID, relationID)
			onTgt, e2 := k.relationPresentFresh(ctx, dls, targetID, relationID)
			if e1 == nil && e2 == nil && !onSrc && !onTgt {
				return nil
			}
			return &AmbiguousMutationError{
				Provider: "kaneo",
				Op:       "DeleteRelation",
				TaskID:   relationID,
				WriteErr: writeErr,
			}
		}
		return writeErr
	}

	// Dual-end absence verification with fresh contexts.
	onSrc, e1 := k.relationPresentFresh(ctx, dls, sourceID, relationID)
	if e1 != nil {
		return fmt.Errorf("kaneo DeleteRelation source absence readback: %w", e1)
	}
	onTgt, e2 := k.relationPresentFresh(ctx, dls, targetID, relationID)
	if e2 != nil {
		return fmt.Errorf("kaneo DeleteRelation target absence readback: %w", e2)
	}
	if onSrc || onTgt {
		return &AmbiguousMutationError{
			Provider: "kaneo",
			Op:       "DeleteRelation",
			TaskID:   relationID,
			WriteErr: fmt.Errorf("relation still present after delete (source=%v target=%v)", onSrc, onTgt),
		}
	}
	return nil
}

func (k *KaneoProvider) relationPresentFresh(ctx context.Context, dls Deadlines, taskID, relationID string) (bool, error) {
	readCtx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()
	rels, err := k.ListRelations(readCtx, taskID)
	if err != nil {
		return false, AsTimeout("kaneo", "ListRelations", OpList, dls.For(OpList), err)
	}
	for _, r := range rels {
		if r.ID == relationID {
			return true, nil
		}
	}
	return false, nil
}

func (k *KaneoProvider) deleteRelationOnce(ctx context.Context, relationID string) error {
	if k.UseCLI {
		args := []string{"task", "rel", "delete", relationID}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			msg := cliErrMsg(res)
			if msg != "" {
				return fmt.Errorf("kaneo task rel delete: %s: %w", msg, err)
			}
			return fmt.Errorf("kaneo task rel delete: %w", err)
		}
		if pe := kaneoRelationErrorBody(res.Stdout); pe != nil {
			pe.Provider = "kaneo"
			pe.Op = "DeleteRelation"
			return pe
		}
		return nil
	}

	url := fmt.Sprintf("%s/api/task/relation/%s", k.APIURL, relationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	// DecodeJSONResponse rejects error bodies under 2xx.
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "DeleteRelation"
		}
		return err
	}
	return nil
}
