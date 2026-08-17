package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
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
	// Closure refreshes call this per-task surface after the graph credential
	// preflight. Prefer exact-origin HTTP whenever authorized, even with
	// use_cli=true, so post-fence closure proof never incurs 4s CLI subprocesses.
	// Single-card callers without authorized HTTP retain the CLI fallback.
	if k.UseCLI && (!k.preferHTTPForRelations() || strings.TrimSpace(os.Getenv(EnvKaneoRelationsCLI)) == "1") {
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

	// HTTP mode: GET /api/task-relation/{taskId}
	return k.listRelationsHTTPOnly(ctx, taskID)
}

// preferHTTPForRelations is true when origin-bound HTTP credentials exist for
// this provider's APIURL (profile origin match or explicit key bound to APIURL).
// Project graph snapshot requires this — CLI fan-out is never used silently.
func (k *KaneoProvider) preferHTTPForRelations() bool {
	if k == nil || strings.TrimSpace(k.APIURL) == "" {
		return false
	}
	return k.credentialForAPIURL() != ""
}

// ErrGraphCredentialsRequired is returned when ListProjectRelations would
// otherwise fall through to N CLI subprocesses (use_cli without API key).
var ErrGraphCredentialsRequired = errors.New("kaneo: project graph snapshot requires HTTP credentials (KANEO_API_KEY or kaneo profile api_key); refusing silent CLI relation fan-out")

// EnvKaneoRelationsCLI is an explicit local-development escape hatch for
// boards whose HTTP relation endpoint is unavailable while the authenticated
// Kaneo CLI remains healthy. It is intentionally opt-in: production keeps the
// origin-bound HTTP graph path and still fails closed when credentials are not
// available.
const EnvKaneoRelationsCLI = "HERD_KANEO_RELATIONS_CLI"

const (
	KaneoGraphDeadlineThreshold = 64
	KaneoGraphMinDeadline       = 2 * time.Minute
)

func (k *KaneoProvider) listRelationsHTTPOnly(ctx context.Context, taskID string) ([]Relation, error) {
	// Single-task HTTP list: credentials optional (authorize when present).
	// Project fan-out credentials are enforced in ListProjectRelations only.
	if k == nil || strings.TrimSpace(k.APIURL) == "" {
		return nil, fmt.Errorf("kaneo listRelationsHTTP: api_url required")
	}
	url := fmt.Sprintf("%s/api/task-relation/%s", strings.TrimRight(k.APIURL, "/"), taskID)
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
	if pe := kaneoRelationErrorBody(body); pe != nil {
		pe.Provider = "kaneo"
		pe.Op = "ListProjectRelations"
		pe.StatusCode = resp.StatusCode
		return nil, pe
	}
	var dtos []kaneoRelationDTO
	if err := DecodeJSONBytes(resp.StatusCode, body, &dtos); err != nil {
		return nil, err
	}
	out := make([]Relation, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, d.toRelation())
	}
	return out, nil
}

// ListProjectRelations builds the project relation multiset for SnapshotGraph.
//
// Honest budget (Kaneo 0.11.x / upstream): only GET task-relation/:taskId exists.
// This is O(board) concurrent HTTP fan-out (not O(1) bulk), measured ~4s for
// ~164 IDs at concurrency 16 — fits DefaultListDeadline (30s) when credentialed.
// Without origin-bound HTTP credentials it FAILS CLOSED before any fan-out
// (never silent CLI N-way stampede).
func (k *KaneoProvider) ListProjectRelations(ctx context.Context, projectID string) ([]Relation, error) {
	if projectID == "" {
		projectID = k.ProjectID
	}
	if projectID == "" {
		return nil, fmt.Errorf("kaneo ListProjectRelations: project id required")
	}
	// Credential preflight BEFORE any ListTasks / fan-out work.
	useHTTP := k.preferHTTPForRelations()
	useCLI := k.UseCLI && strings.TrimSpace(os.Getenv(EnvKaneoRelationsCLI)) == "1"
	if !useHTTP && !useCLI {
		return nil, fmt.Errorf("%w (use_cli=%v api_url=%q)", ErrGraphCredentialsRequired, k.UseCLI, k.APIURL)
	}
	dls := k.deadlines()
	// Keep ordinary task listing on the configured list deadline. Relation
	// snapshots are a separate, bounded graph operation and retain the
	// large-board extension: boards with 64+ tasks get at least two minutes
	// for the relation fan-out, while a caller deadline still wins.
	listCtx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()

	tasks, err := k.ListTasks(listCtx, projectID, "")
	if err != nil {
		return nil, fmt.Errorf("kaneo ListProjectRelations list tasks: %w", err)
	}
	ids := make([]string, 0, len(tasks))
	idSet := map[string]struct{}{}
	for _, t := range tasks {
		if t == nil || t.ID == "" {
			continue
		}
		if _, ok := idSet[t.ID]; ok {
			continue
		}
		idSet[t.ID] = struct{}{}
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	graphDeadline := dls.For(OpList)
	if len(ids) >= KaneoGraphDeadlineThreshold && graphDeadline < KaneoGraphMinDeadline {
		graphDeadline = KaneoGraphMinDeadline
	}
	graphCtx, graphCancel := context.WithTimeout(ctx, graphDeadline)
	defer graphCancel()

	conc := k.BulkConcurrency
	if conc <= 0 {
		conc = DefaultBulkRelationConcurrency
		if len(ids) > KaneoLargeBoardThreshold {
			conc = DefaultKaneoLargeBoardConcurrency
		}
	}
	if conc > MaxKaneoGraphConcurrency {
		conc = MaxKaneoGraphConcurrency
	}
	if conc > len(ids) && len(ids) > 0 {
		conc = len(ids)
	}

	type result struct {
		taskID string
		rels   []Relation
		err    error
	}
	// Process bounded batches instead of placing the entire board on one work
	// queue. This keeps cancellation and in-flight request cardinality bounded
	// for boards with thousands of tasks while preserving deterministic result
	// assembly below.
	byTask := map[string][]Relation{}
	for start := 0; start < len(ids); start += KaneoGraphBatchSize {
		end := start + KaneoGraphBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batchCtx, batchCancel := context.WithCancel(graphCtx)
		jobs := make(chan string)
		outCh := make(chan result, end-start)
		var wg sync.WaitGroup

		worker := func() {
			defer wg.Done()
			for {
				select {
				case <-batchCtx.Done():
					return
				case id, ok := <-jobs:
					if !ok {
						return
					}
					var rels []Relation
					var e error
					if useCLI {
						rels, e = k.ListRelations(batchCtx, id)
					} else {
						rels, e = k.listRelationsHTTPOnly(batchCtx, id)
					}
					if e != nil {
						outCh <- result{taskID: id, err: AsTimeout("kaneo", "ListProjectRelations", OpList, graphDeadline, e)}
						batchCancel()
						return
					}
					outCh <- result{taskID: id, rels: rels}
				}
			}
		}
		for i := 0; i < conc; i++ {
			wg.Add(1)
			go worker()
		}
		go func() {
			defer close(jobs)
			for _, id := range ids[start:end] {
				select {
				case <-batchCtx.Done():
					return
				case jobs <- id:
				}
			}
		}()
		wg.Wait()
		batchCancel()
		close(outCh)
		for r := range outCh {
			if r.err != nil {
				return nil, fmt.Errorf("kaneo ListProjectRelations task %s: %w", r.taskID, r.err)
			}
			byTask[r.taskID] = r.rels
		}
		if err := graphCtx.Err(); err != nil {
			return nil, AsTimeout("kaneo", "ListProjectRelations", OpList, graphDeadline, err)
		}
	}
	if err := graphCtx.Err(); err != nil {
		return nil, AsTimeout("kaneo", "ListProjectRelations", OpList, graphDeadline, err)
	}

	// Dual-end agreement: every relation id must appear with identical fields
	// on both source and target listings when both endpoints are in-project.
	type acc struct {
		src *Relation
		tgt *Relation
	}
	accs := map[string]*acc{}
	for taskID, rels := range byTask {
		for i := range rels {
			rel := rels[i]
			if rel.ID == "" {
				return nil, fmt.Errorf("kaneo ListProjectRelations: empty relation id on task %s", taskID)
			}
			a := accs[rel.ID]
			if a == nil {
				a = &acc{}
				accs[rel.ID] = a
			}
			if rel.SourceTaskID == taskID {
				if a.src != nil && !relationFieldsEqual(*a.src, rel) {
					return nil, fmt.Errorf("kaneo ListProjectRelations: relation %s inconsistent on source", rel.ID)
				}
				cp := rel
				a.src = &cp
			}
			if rel.TargetTaskID == taskID {
				if a.tgt != nil && !relationFieldsEqual(*a.tgt, rel) {
					return nil, fmt.Errorf("kaneo ListProjectRelations: relation %s inconsistent on target", rel.ID)
				}
				cp := rel
				a.tgt = &cp
			}
		}
	}
	out := make([]Relation, 0, len(accs))
	var keys []string
	for id := range accs {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		a := accs[id]
		if a.src == nil || a.tgt == nil {
			// Endpoint outside project set: require at least one side and
			// accept if the missing end is not in idSet.
			if a.src == nil && a.tgt == nil {
				return nil, fmt.Errorf("kaneo ListProjectRelations: relation %s missing both ends", id)
			}
			var r Relation
			if a.src != nil {
				r = *a.src
			} else {
				r = *a.tgt
			}
			_, srcIn := idSet[r.SourceTaskID]
			_, tgtIn := idSet[r.TargetTaskID]
			if srcIn && tgtIn {
				return nil, fmt.Errorf("kaneo ListProjectRelations: relation %s not dual-listed for in-project endpoints", id)
			}
			out = append(out, r)
			continue
		}
		if !relationFieldsEqual(*a.src, *a.tgt) {
			return nil, fmt.Errorf("kaneo ListProjectRelations: relation %s field disagreement", id)
		}
		out = append(out, *a.src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func relationFieldsEqual(a, b Relation) bool {
	return a.ID == b.ID && a.SourceTaskID == b.SourceTaskID &&
		a.TargetTaskID == b.TargetTaskID && a.Type == b.Type
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

	// Fresh dual-end pre-check: existing edge on BOTH ends → return (idempotent).
	if existing, err := k.findRelationBothEnds(ctx, dls, sourceID, targetID, typ); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	_, writeErr := k.createRelationOnce(writeCtx, sourceID, targetID, typ)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("kaneo", "CreateRelation", OpMutate, dls.For(OpMutate), writeErr)
		if IsTimeout(writeErr) || IsAmbiguous(writeErr) {
			if existing, ferr := k.findRelationBothEnds(ctx, dls, sourceID, targetID, typ); ferr == nil && existing != nil {
				return existing, nil
			}
			return nil, &AmbiguousMutationError{
				Provider: "kaneo",
				Op:       "CreateRelation",
				TaskID:   sourceID + "->" + targetID,
				WriteErr: writeErr,
			}
		}
		if existing, ferr := k.findRelationBothEnds(ctx, dls, sourceID, targetID, typ); ferr == nil && existing != nil {
			return existing, nil
		}
		return nil, writeErr
	}

	// Dual-end readback (separate fresh contexts inside findRelationBothEnds).
	srcRel, err := k.findRelationBothEnds(ctx, dls, sourceID, targetID, typ)
	if err != nil {
		return nil, fmt.Errorf("kaneo CreateRelation dual readback: %w", err)
	}
	if srcRel == nil {
		return nil, fmt.Errorf("kaneo CreateRelation readback: edge missing on source or target after create")
	}
	return srcRel, nil
}

// findRelationBothEnds lists BOTH endpoints independently first. One-sided
// visibility (source-only OR target-only) is a hard error (never silent nil
// that would allow a duplicate create). Matching fields on both ends required.
func (k *KaneoProvider) findRelationBothEnds(ctx context.Context, dls Deadlines, sourceID, targetID string, typ RelationType) (*Relation, error) {
	fromSrc, err := k.findRelationOnTask(ctx, dls, sourceID, sourceID, targetID, typ)
	if err != nil {
		return nil, err
	}
	fromTgt, err := k.findRelationOnTask(ctx, dls, targetID, sourceID, targetID, typ)
	if err != nil {
		return nil, err
	}
	if fromSrc == nil && fromTgt == nil {
		return nil, nil
	}
	if fromSrc == nil && fromTgt != nil {
		return nil, fmt.Errorf("kaneo: one-sided relation (target-only) %s -[%s]-> %s", sourceID, typ, targetID)
	}
	if fromSrc != nil && fromTgt == nil {
		return nil, fmt.Errorf("kaneo: one-sided relation (source-only) %s -[%s]-> %s", sourceID, typ, targetID)
	}
	if fromSrc.ID != fromTgt.ID || fromSrc.SourceTaskID != fromTgt.SourceTaskID ||
		fromSrc.TargetTaskID != fromTgt.TargetTaskID || fromSrc.Type != fromTgt.Type {
		return nil, fmt.Errorf("kaneo: relation field disagreement between endpoints")
	}
	return fromSrc, nil
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
	k.authorizeKaneo(req)
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

// DeleteRelation deletes a relation and verifies absence on BOTH true endpoints.
// Caller endpoints are a hint; authoritative source/target/type are captured from
// readback before delete. Ambiguous delete/timeouts never succeed.
func (k *KaneoProvider) DeleteRelation(ctx context.Context, relationID, sourceID, targetID string) error {
	if strings.TrimSpace(relationID) == "" {
		return fmt.Errorf("kaneo DeleteRelation: empty relation id")
	}
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(targetID) == "" {
		return fmt.Errorf("kaneo DeleteRelation: source and target endpoints required for dual readback")
	}
	dls := k.deadlines()

	// Capture actual relation endpoints/type from source listing (not trust alone).
	captured, err := k.captureRelationFresh(ctx, dls, sourceID, relationID)
	if err != nil {
		return fmt.Errorf("kaneo DeleteRelation pre-capture source: %w", err)
	}
	if captured == nil {
		// Try target side.
		captured, err = k.captureRelationFresh(ctx, dls, targetID, relationID)
		if err != nil {
			return fmt.Errorf("kaneo DeleteRelation pre-capture target: %w", err)
		}
		if captured == nil {
			// Absent both ends already.
			return nil
		}
	}
	// Authoritative endpoints from readback.
	trueSrc, trueTgt := captured.SourceTaskID, captured.TargetTaskID
	if trueSrc == "" || trueTgt == "" {
		return fmt.Errorf("kaneo DeleteRelation: captured relation missing endpoints")
	}

	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := k.deleteRelationOnce(writeCtx, relationID)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("kaneo", "DeleteRelation", OpMutate, dls.For(OpMutate), writeErr)
		if IsTimeout(writeErr) || IsAmbiguous(writeErr) {
			onSrc, e1 := k.relationPresentFresh(ctx, dls, trueSrc, relationID)
			onTgt, e2 := k.relationPresentFresh(ctx, dls, trueTgt, relationID)
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

	onSrc, e1 := k.relationPresentFresh(ctx, dls, trueSrc, relationID)
	if e1 != nil {
		return fmt.Errorf("kaneo DeleteRelation source absence readback: %w", e1)
	}
	onTgt, e2 := k.relationPresentFresh(ctx, dls, trueTgt, relationID)
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

func (k *KaneoProvider) captureRelationFresh(ctx context.Context, dls Deadlines, taskID, relationID string) (*Relation, error) {
	readCtx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()
	rels, err := k.ListRelations(readCtx, taskID)
	if err != nil {
		return nil, AsTimeout("kaneo", "ListRelations", OpList, dls.For(OpList), err)
	}
	for i := range rels {
		if rels[i].ID == relationID {
			r := rels[i]
			return &r, nil
		}
	}
	return nil, nil
}

func (k *KaneoProvider) relationPresentFresh(ctx context.Context, dls Deadlines, taskID, relationID string) (bool, error) {
	r, err := k.captureRelationFresh(ctx, dls, taskID, relationID)
	if err != nil {
		return false, err
	}
	return r != nil, nil
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
	k.authorizeKaneo(req)
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
