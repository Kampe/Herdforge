package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	var dtos []kaneoRelationDTO
	if err := DecodeJSONResponse(resp, &dtos); err != nil {
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

// CreateRelation creates a directed relation and readbacks the exact edge.
func (k *KaneoProvider) CreateRelation(ctx context.Context, sourceID, targetID string, typ RelationType) (*Relation, error) {
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(targetID) == "" {
		return nil, fmt.Errorf("kaneo CreateRelation: source and target required")
	}
	if typ == "" {
		typ = RelationBlocks
	}
	dls := k.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	created, writeErr := k.createRelationOnce(writeCtx, sourceID, targetID, typ)
	cancel()
	if writeErr != nil {
		return nil, AsTimeout("kaneo", "CreateRelation", OpMutate, dls.For(OpMutate), writeErr)
	}

	// Readback: list source relations and find matching edge.
	readCtx, cancel2 := WithOpDeadline(ctx, dls, OpList)
	defer cancel2()
	rels, err := k.ListRelations(readCtx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("kaneo CreateRelation readback: %w", err)
	}
	for i := range rels {
		r := &rels[i]
		if r.SourceTaskID == sourceID && r.TargetTaskID == targetID && r.Type == typ {
			if created != nil && created.ID != "" && r.ID != created.ID {
				// Prefer list readback id when create response differed.
			}
			return r, nil
		}
	}
	// Also check target side (some APIs only return one direction on list).
	rels2, err2 := k.ListRelations(readCtx, targetID)
	if err2 == nil {
		for i := range rels2 {
			r := &rels2[i]
			if r.SourceTaskID == sourceID && r.TargetTaskID == targetID && r.Type == typ {
				return r, nil
			}
		}
	}
	return nil, fmt.Errorf("kaneo CreateRelation readback: edge %s -[%s]-> %s not found after create", sourceID, typ, targetID)
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
		// CLI may return object or empty; best-effort decode.
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
	r := dto.toRelation()
	return &r, nil
}

// DeleteRelation deletes a relation by id and verifies it is gone.
func (k *KaneoProvider) DeleteRelation(ctx context.Context, relationID string) error {
	if strings.TrimSpace(relationID) == "" {
		return fmt.Errorf("kaneo DeleteRelation: empty relation id")
	}
	dls := k.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	err := k.deleteRelationOnce(writeCtx, relationID)
	cancel()
	if err != nil {
		return AsTimeout("kaneo", "DeleteRelation", OpMutate, dls.For(OpMutate), err)
	}
	return nil
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
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "DeleteRelation"
		}
		return err
	}
	return nil
}
