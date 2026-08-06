package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Linear GraphQL dependency-graph contract (public SDK / schema):
//
//	issue.relations        = source-side edges (this issue is issue/source)
//	issue.inverseRelations = target-side edges (this issue is relatedIssue/target)
//
// Relation node fields: id, type, createdAt, issue{id}, relatedIssue{id}.
// Mutations (public SDK):
//
//	issueRelationCreate(input: IssueRelationCreateInput!): IssueRelationPayload
//	issueRelationDelete(id: String!): DeletePayload
//
// Supported types only: blocks → RelationBlocks, related → RelationRelated.
// duplicate / aliases / blank / unknown / subtask create FAIL closed.

const (
	linearListOutgoingRelationsQuery = `
query LinearListOutgoingRelations($id: String!, $after: String) {
  issue(id: $id) {
    relations(first: 50, after: $after) {
      nodes {
        id
        type
        createdAt
        issue { id }
        relatedIssue { id }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`

	linearListIncomingRelationsQuery = `
query LinearListIncomingRelations($id: String!, $after: String) {
  issue(id: $id) {
    inverseRelations(first: 50, after: $after) {
      nodes {
        id
        type
        createdAt
        issue { id }
        relatedIssue { id }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`

	// Matches Linear public SDK: issueRelationCreate(input: IssueRelationCreateInput!)
	linearCreateRelationMutation = `
mutation LinearCreateRelation($issueId: String!, $relatedIssueId: String!, $type: IssueRelationType!) {
  issueRelationCreate(input: {issueId: $issueId, relatedIssueId: $relatedIssueId, type: $type}) {
    success
    issueRelation {
      id
      type
      createdAt
      issue { id }
      relatedIssue { id }
    }
  }
}`

	// Matches Linear public SDK: issueRelationDelete(id: String!)
	linearDeleteRelationMutation = `
mutation LinearDeleteRelation($id: String!) {
  issueRelationDelete(id: $id) {
    success
  }
}`
)

// Compile-time interface assertions.
var (
	_ RelationProvider     = (*LinearProvider)(nil)
	_ BulkRelationProvider = (*LinearProvider)(nil)
)

// linearRelationNode is one Linear issueRelation / inverseRelation row.
// Canonical direction: issue/source → relatedIssue/target.
type linearRelationNode struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CreatedAt string `json:"createdAt"`
	Issue     *struct {
		ID string `json:"id"`
	} `json:"issue"`
	RelatedIssue *struct {
		ID string `json:"id"`
	} `json:"relatedIssue"`
}

type linearRelationConnection struct {
	Nodes    []linearRelationNode `json:"nodes"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

// mapLinearRelationType maps a Linear relation type string to the Herdforge model.
// Only "blocks" and "related" are supported. Everything else fails explicitly —
// never silently mapped or dropped.
func mapLinearRelationType(raw string) (RelationType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "blocks":
		return RelationBlocks, nil
	case "related":
		return RelationRelated, nil
	case "":
		return "", fmt.Errorf("blank relation type")
	case "duplicate", "duplicate_of", "subtask", "parent", "relates", "relates_to", "related_to", "blocked_by":
		return "", fmt.Errorf("unsupported linear relation type %q", raw)
	default:
		return "", fmt.Errorf("unsupported linear relation type %q", raw)
	}
}

func linearRelationTypeWire(t RelationType) (string, error) {
	switch t {
	case RelationBlocks:
		return "blocks", nil
	case RelationRelated:
		return "related", nil
	case RelationSubtask:
		return "", fmt.Errorf("subtask relations are not supported by Linear issueRelationCreate")
	default:
		return "", fmt.Errorf("unsupported relation type %q", t)
	}
}

// parseRelationNode validates one Linear relation node. Malformed rows fail
// closed (never silently skipped).
func parseRelationNode(n linearRelationNode) (Relation, error) {
	id := strings.TrimSpace(n.ID)
	if id == "" {
		return Relation{}, fmt.Errorf("relation missing immutable id")
	}
	if n.Issue == nil || strings.TrimSpace(n.Issue.ID) == "" {
		return Relation{}, fmt.Errorf("relation %s missing source issue endpoint", id)
	}
	if n.RelatedIssue == nil || strings.TrimSpace(n.RelatedIssue.ID) == "" {
		return Relation{}, fmt.Errorf("relation %s missing relatedIssue endpoint", id)
	}
	sourceID := strings.TrimSpace(n.Issue.ID)
	targetID := strings.TrimSpace(n.RelatedIssue.ID)
	if sourceID == targetID {
		return Relation{}, fmt.Errorf("relation %s is a self-edge (%s)", id, sourceID)
	}
	relType, err := mapLinearRelationType(n.Type)
	if err != nil {
		return Relation{}, fmt.Errorf("relation %s: %w", id, err)
	}
	createdRaw := strings.TrimSpace(n.CreatedAt)
	if createdRaw == "" {
		return Relation{}, fmt.Errorf("relation %s missing createdAt", id)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		createdAt, err = time.Parse(time.RFC3339, createdRaw)
		if err != nil {
			return Relation{}, fmt.Errorf("relation %s malformed createdAt %q: %w", id, n.CreatedAt, err)
		}
	}
	return Relation{
		ID:           id,
		SourceTaskID: sourceID,
		TargetTaskID: targetID,
		Type:         relType,
		CreatedAt:    createdAt.UTC(),
	}, nil
}

func parseRelationNodes(nodes []linearRelationNode) ([]Relation, error) {
	out := make([]Relation, 0, len(nodes))
	for i, n := range nodes {
		rel, err := parseRelationNode(n)
		if err != nil {
			return nil, fmt.Errorf("node[%d]: %w", i, err)
		}
		out = append(out, rel)
	}
	return out, nil
}

// listRelationsSide pages one connection side (outgoing=relations or
// incoming=inverseRelations). Side-specific independent cursors; reads use
// RetryRead. Mutations are never retried here.
func (l *LinearProvider) listRelationsSide(ctx context.Context, taskID string, outgoing bool) ([]Relation, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("linear listRelations: task id is required")
	}

	query := linearListIncomingRelationsQuery
	sideName := "inverseRelations"
	if outgoing {
		query = linearListOutgoingRelationsQuery
		sideName = "relations"
	}

	var (
		all           []Relation
		cursor        string
		pages         int
		seenRelations = make(map[string]Relation)
		seenCursors   = make(map[string]struct{})
	)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pages++
		if pages > DefaultMaxListPages {
			return nil, fmt.Errorf("linear listRelations %s %s: %w (%d pages)", taskID, sideName, ErrPaginationCap, pages)
		}

		vars := map[string]interface{}{"id": taskID}
		if cursor != "" {
			vars["after"] = cursor
		}

		// GraphQL responses retain the outer data envelope (doGraphQL / DecodeJSONResponse).
		var resp struct {
			Data struct {
				Issue *struct {
					Relations        *linearRelationConnection `json:"relations"`
					InverseRelations *linearRelationConnection `json:"inverseRelations"`
				} `json:"issue"`
			} `json:"data"`
		}

		err := RetryRead(ctx, l.readRetry(), func(attemptCtx context.Context) error {
			return l.doGraphQL(attemptCtx, query, vars, &resp)
		})
		if err != nil {
			return nil, fmt.Errorf("linear listRelations %s %s: %w", taskID, sideName, err)
		}
		if resp.Data.Issue == nil {
			return nil, fmt.Errorf("linear listRelations %s %s: issue %s not found", taskID, sideName, taskID)
		}

		var conn *linearRelationConnection
		if outgoing {
			conn = resp.Data.Issue.Relations
		} else {
			conn = resp.Data.Issue.InverseRelations
		}
		if conn == nil {
			return nil, fmt.Errorf("linear listRelations %s %s: missing %s connection", taskID, sideName, sideName)
		}

		// Empty nodes with hasNextPage=true must fail (not silently terminate).
		if len(conn.Nodes) == 0 {
			if conn.PageInfo.HasNextPage {
				return nil, fmt.Errorf("linear listRelations %s %s: empty page with hasNextPage=true", taskID, sideName)
			}
			break
		}

		page, err := parseRelationNodes(conn.Nodes)
		if err != nil {
			return nil, fmt.Errorf("linear listRelations %s %s: %w", taskID, sideName, err)
		}

		// Enforce connection side direction.
		for _, r := range page {
			if outgoing {
				if r.SourceTaskID != taskID {
					return nil, fmt.Errorf("linear listRelations %s relations: expected source=%s, got source=%s target=%s (id=%s)",
						taskID, taskID, r.SourceTaskID, r.TargetTaskID, r.ID)
				}
			} else if r.TargetTaskID != taskID {
				return nil, fmt.Errorf("linear listRelations %s inverseRelations: expected target=%s, got source=%s target=%s (id=%s)",
					taskID, taskID, r.SourceTaskID, r.TargetTaskID, r.ID)
			}
		}

		// Local ID dedupe: disagreeing fields fail closed; identical re-sightings
		// are skipped; a non-empty page with zero fresh IDs is ErrDuplicatePage.
		fresh := 0
		for _, r := range page {
			if prev, ok := seenRelations[r.ID]; ok {
				if !linearRelationFieldsEqual(prev, r) {
					return nil, fmt.Errorf("linear listRelations %s %s: relation %s disagree: %+v vs %+v",
						taskID, sideName, r.ID, prev, r)
				}
				continue
			}
			seenRelations[r.ID] = r
			all = append(all, r)
			fresh++
		}
		if fresh == 0 {
			return nil, fmt.Errorf("linear listRelations %s %s: %w", taskID, sideName, ErrDuplicatePage)
		}

		if !conn.PageInfo.HasNextPage {
			break
		}
		// hasNextPage=true requires a nonempty, previously unseen cursor.
		next := strings.TrimSpace(conn.PageInfo.EndCursor)
		if next == "" {
			return nil, fmt.Errorf("linear listRelations %s %s: hasNextPage with empty cursor", taskID, sideName)
		}
		if _, seen := seenCursors[next]; seen {
			return nil, fmt.Errorf("linear listRelations %s %s: %w (cursor %q)", taskID, sideName, ErrDuplicatePage, next)
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, nil
}

// ListRelations returns every dependency edge involving taskID (outgoing + incoming).
func (l *LinearProvider) ListRelations(ctx context.Context, taskID string) ([]Relation, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("linear ListRelations: task id is required")
	}

	dls := l.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()

	outgoing, err := l.listRelationsSide(ctx, taskID, true)
	if err != nil {
		return nil, err
	}
	incoming, err := l.listRelationsSide(ctx, taskID, false)
	if err != nil {
		return nil, err
	}

	// Merge; identical dual-end observations of the same ID must agree.
	seen := make(map[string]Relation, len(outgoing)+len(incoming))
	for _, r := range outgoing {
		if prev, ok := seen[r.ID]; ok {
			if !linearRelationFieldsEqual(prev, r) {
				return nil, fmt.Errorf("linear ListRelations %s: relation %s disagree across sides: %+v vs %+v",
					taskID, r.ID, prev, r)
			}
			continue
		}
		seen[r.ID] = r
	}
	for _, r := range incoming {
		if prev, ok := seen[r.ID]; ok {
			if !linearRelationFieldsEqual(prev, r) {
				return nil, fmt.Errorf("linear ListRelations %s: relation %s disagree across sides: %+v vs %+v",
					taskID, r.ID, prev, r)
			}
			continue
		}
		seen[r.ID] = r
	}

	out := make([]Relation, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListProjectRelations enumerates every relation involving any task in the project.
// Honest O(board) fan-out via ListTasks plus a fixed, bounded worker pool.
//
// Dual-end visibility rules:
//   - both endpoints in project → identical dual-end observation required
//   - exactly one endpoint outside project → single in-project observation allowed
//   - half-visible in-project relation → hard error (never silently skipped)
func (l *LinearProvider) ListProjectRelations(ctx context.Context, projectID string) ([]Relation, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = strings.TrimSpace(l.ProjectID)
	}
	if projectID == "" {
		return nil, fmt.Errorf("linear ListProjectRelations: project id is required")
	}

	dls := l.deadlines()
	listCtx, listCancel := WithOpDeadline(ctx, dls, OpList)
	defer listCancel()

	tasks, err := l.ListTasks(listCtx, projectID, "")
	if err != nil {
		return nil, fmt.Errorf("linear ListProjectRelations: list tasks: %w", err)
	}

	inProject := make(map[string]struct{}, len(tasks))
	ids := make([]string, 0, len(tasks))
	for i, task := range tasks {
		if task == nil {
			return nil, fmt.Errorf("linear ListProjectRelations: nil task at index %d", i)
		}
		id := strings.TrimSpace(task.ID)
		if id == "" {
			return nil, fmt.Errorf("linear ListProjectRelations: empty task id at index %d", i)
		}
		if _, exists := inProject[id]; exists {
			continue
		}
		inProject[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return []Relation{}, nil
	}

	concurrency := l.BulkConcurrency
	if concurrency <= 0 {
		concurrency = DefaultBulkRelationConcurrency
	}
	if concurrency > len(ids) {
		concurrency = len(ids)
	}

	type result struct {
		taskID string
		rels   []Relation
		err    error
	}

	workerCtx, workerCancel := context.WithCancel(listCtx)
	defer workerCancel()
	jobs := make(chan string)
	results := make(chan result, concurrency)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-workerCtx.Done():
				return
			case taskID, ok := <-jobs:
				if !ok {
					return
				}
				rels, readErr := l.ListRelations(workerCtx, taskID)
				res := result{taskID: taskID, rels: rels, err: readErr}
				if readErr != nil {
					select {
					case results <- res:
					case <-listCtx.Done():
					}
					workerCancel()
					return
				}
				select {
				case results <- res:
				case <-workerCtx.Done():
					return
				}
			}
		}
	}

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go worker()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-workerCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	byTask := make(map[string][]Relation, len(ids))
	var firstErr error
	for res := range results {
		if res.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("linear ListProjectRelations: task %s: %w", res.taskID, res.err)
			}
			continue
		}
		if _, duplicate := byTask[res.taskID]; duplicate {
			if firstErr == nil {
				firstErr = fmt.Errorf("linear ListProjectRelations: duplicate result for task %s", res.taskID)
				workerCancel()
			}
			continue
		}
		byTask[res.taskID] = res.rels
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if err := listCtx.Err(); err != nil {
		return nil, AsTimeout("linear", "ListProjectRelations", OpList, dls.For(OpList), err)
	}
	if len(byTask) != len(ids) {
		return nil, fmt.Errorf("linear ListProjectRelations: expected %d task results, got %d", len(ids), len(byTask))
	}

	type sighting struct {
		fromTask string
		rel      Relation
	}
	observations := make(map[string][]sighting)
	for _, taskID := range ids {
		for _, rel := range byTask[taskID] {
			if rel.SourceTaskID != taskID && rel.TargetTaskID != taskID {
				return nil, fmt.Errorf("linear ListProjectRelations: relation %s unrelated to task %s", rel.ID, taskID)
			}
			observations[rel.ID] = append(observations[rel.ID], sighting{fromTask: taskID, rel: rel})
		}
	}

	out := make([]Relation, 0, len(observations))
	for relID, sightings := range observations {
		if len(sightings) == 0 {
			return nil, fmt.Errorf("linear ListProjectRelations: relation %s has no sightings", relID)
		}
		canonical := sightings[0].rel
		for _, sighting := range sightings[1:] {
			if !linearRelationFieldsEqual(canonical, sighting.rel) {
				return nil, fmt.Errorf("linear ListProjectRelations: relation %s disagree across tasks: %+v vs %+v", relID, canonical, sighting.rel)
			}
		}

		_, sourceInProject := inProject[canonical.SourceTaskID]
		_, targetInProject := inProject[canonical.TargetTaskID]
		switch {
		case sourceInProject && targetInProject:
			var sawSource, sawTarget bool
			for _, sighting := range sightings {
				if sighting.fromTask == canonical.SourceTaskID {
					sawSource = true
				}
				if sighting.fromTask == canonical.TargetTaskID {
					sawTarget = true
				}
			}
			if !sawSource || !sawTarget {
				return nil, fmt.Errorf("linear ListProjectRelations: relation %s half-visible in project (source_seen=%v target_seen=%v source=%s target=%s)", relID, sawSource, sawTarget, canonical.SourceTaskID, canonical.TargetTaskID)
			}
			out = append(out, canonical)
		case sourceInProject || targetInProject:
			out = append(out, canonical)
		default:
			return nil, fmt.Errorf("linear ListProjectRelations: relation %s endpoints %s->%s both outside project", relID, canonical.SourceTaskID, canonical.TargetTaskID)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (l *LinearProvider) findExactRelation(ctx context.Context, sourceID, targetID string, relType RelationType) (*Relation, error) {
	rels, err := l.ListRelations(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	var match *Relation
	for i := range rels {
		r := rels[i]
		if r.SourceTaskID == sourceID && r.TargetTaskID == targetID && r.Type == relType {
			if match != nil && match.ID != r.ID {
				return nil, fmt.Errorf("multiple exact relations %s->%s type=%s", sourceID, targetID, relType)
			}
			cp := r
			match = &cp
		}
	}
	return match, nil
}

// relationByIDExact returns the single row with relationID, rejecting duplicate IDs.
func relationByIDExact(rels []Relation, relationID string) (*Relation, error) {
	var hit *Relation
	for i := range rels {
		if rels[i].ID != relationID {
			continue
		}
		if hit != nil {
			return nil, fmt.Errorf("relation %s listed more than once", relationID)
		}
		cp := rels[i]
		hit = &cp
	}
	return hit, nil
}

func linearRelationFieldsEqual(a, b Relation) bool {
	return relationFieldsEqual(a, b) && a.CreatedAt.Equal(b.CreatedAt)
}

// relationPresentBothEnds requires one identical relation row on each endpoint.
func (l *LinearProvider) relationPresentBothEnds(ctx context.Context, sourceID, targetID, relationID string) error {
	srcRels, err := l.ListRelations(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("source list: %w", err)
	}
	tgtRels, err := l.ListRelations(ctx, targetID)
	if err != nil {
		return fmt.Errorf("target list: %w", err)
	}
	srcHit, err := relationByIDExact(srcRels, relationID)
	if err != nil {
		return fmt.Errorf("source relation: %w", err)
	}
	tgtHit, err := relationByIDExact(tgtRels, relationID)
	if err != nil {
		return fmt.Errorf("target relation: %w", err)
	}
	if srcHit == nil || tgtHit == nil {
		return fmt.Errorf("relation %s not dual-end confirmed (source=%v target=%v)", relationID, srcHit != nil, tgtHit != nil)
	}
	if srcHit.SourceTaskID != sourceID || srcHit.TargetTaskID != targetID ||
		tgtHit.SourceTaskID != sourceID || tgtHit.TargetTaskID != targetID {
		return fmt.Errorf("relation %s endpoint disagreement", relationID)
	}
	if !linearRelationFieldsEqual(*srcHit, *tgtHit) {
		return fmt.Errorf("relation %s field disagreement between endpoints", relationID)
	}
	return nil
}

func (l *LinearProvider) relationAbsentBothEnds(ctx context.Context, sourceID, targetID, relationID string) error {
	srcRels, err := l.ListRelations(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("source list: %w", err)
	}
	tgtRels, err := l.ListRelations(ctx, targetID)
	if err != nil {
		return fmt.Errorf("target list: %w", err)
	}
	for _, r := range srcRels {
		if r.ID == relationID {
			return fmt.Errorf("relation %s still present on source %s", relationID, sourceID)
		}
	}
	for _, r := range tgtRels {
		if r.ID == relationID {
			return fmt.Errorf("relation %s still present on target %s", relationID, targetID)
		}
	}
	return nil
}

func (l *LinearProvider) reconcileUncertainCreate(ctx context.Context, sourceID, targetID string, relType RelationType, cause error) (*Relation, error) {
	existing, readErr := l.findExactRelation(ctx, sourceID, targetID, relType)
	if readErr == nil && existing != nil {
		if confirmErr := l.relationPresentBothEnds(ctx, sourceID, targetID, existing.ID); confirmErr == nil {
			return existing, nil
		} else {
			readErr = confirmErr
		}
	}
	return nil, &AmbiguousMutationError{
		Provider: "linear",
		Op:       "CreateRelation",
		TaskID:   sourceID + "->" + targetID,
		Want:     "relation present both ends",
		WriteErr: cause,
		ReadErr:  readErr,
	}
}

// CreateRelation creates a dependency edge source → target.
//
// Protocol:
//  1. reject blank/self/unsupported (including subtask)
//  2. idempotent exact dual-end precheck
//  3. one mutation attempt under WithOpDeadline (never blindly retried)
//  4. success=false / nil / malformed payload fails closed
//  5. on timeout/ambiguous: reconcile exact edge both ends; return existing only if proven
//  6. successful mutation requires exact dual-end readback/agreement
func (l *LinearProvider) CreateRelation(ctx context.Context, sourceID, targetID string, relType RelationType) (*Relation, error) {
	sourceID = strings.TrimSpace(sourceID)
	targetID = strings.TrimSpace(targetID)
	if sourceID == "" || targetID == "" {
		return nil, fmt.Errorf("linear CreateRelation: source and target ids are required")
	}
	if sourceID == targetID {
		return nil, fmt.Errorf("linear CreateRelation: self-edge not allowed")
	}
	if !ValidRelationType(relType) {
		return nil, fmt.Errorf("linear CreateRelation: unsupported relation type %q", relType)
	}
	wireType, err := linearRelationTypeWire(relType)
	if err != nil {
		return nil, fmt.Errorf("linear CreateRelation: %w", err)
	}

	l.relationMu.Lock()
	defer l.relationMu.Unlock()

	// Idempotent exact dual-end precheck.
	if existing, err := l.findExactRelation(ctx, sourceID, targetID, relType); err != nil {
		return nil, fmt.Errorf("linear CreateRelation: precheck: %w", err)
	} else if existing != nil {
		if err := l.relationPresentBothEnds(ctx, sourceID, targetID, existing.ID); err != nil {
			return nil, fmt.Errorf("linear CreateRelation: precheck dual-end: %w", err)
		}
		return existing, nil
	}

	vars := map[string]interface{}{
		"issueId":        sourceID,
		"relatedIssueId": targetID,
		"type":           wireType,
	}

	// One mutation attempt — never blindly retried.
	var resp struct {
		Data struct {
			IssueRelationCreate *struct {
				Success       bool                `json:"success"`
				IssueRelation *linearRelationNode `json:"issueRelation"`
			} `json:"issueRelationCreate"`
		} `json:"data"`
	}

	dls := l.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := l.doGraphQL(writeCtx, linearCreateRelationMutation, vars, &resp)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("linear", "CreateRelation", OpMutate, dls.For(OpMutate), writeErr)
	}

	if writeErr != nil {
		return l.reconcileUncertainCreate(ctx, sourceID, targetID, relType, writeErr)
	}

	payload := resp.Data.IssueRelationCreate
	if payload == nil {
		return l.reconcileUncertainCreate(ctx, sourceID, targetID, relType, fmt.Errorf("nil issueRelationCreate payload"))
	}
	if !payload.Success {
		return nil, fmt.Errorf("linear CreateRelation: success=false")
	}
	if payload.IssueRelation == nil {
		return l.reconcileUncertainCreate(ctx, sourceID, targetID, relType, fmt.Errorf("success response missing issueRelation"))
	}

	created, err := parseRelationNode(*payload.IssueRelation)
	if err != nil {
		return l.reconcileUncertainCreate(ctx, sourceID, targetID, relType, fmt.Errorf("malformed response: %w", err))
	}
	if created.SourceTaskID != sourceID || created.TargetTaskID != targetID {
		return l.reconcileUncertainCreate(ctx, sourceID, targetID, relType, fmt.Errorf("response endpoints %s->%s do not match requested %s->%s", created.SourceTaskID, created.TargetTaskID, sourceID, targetID))
	}
	if created.Type != relType {
		return l.reconcileUncertainCreate(ctx, sourceID, targetID, relType, fmt.Errorf("response type %s does not match requested %s", created.Type, relType))
	}

	// Successful mutation requires exact dual-end readback/agreement.
	if err := l.relationPresentBothEnds(ctx, sourceID, targetID, created.ID); err != nil {
		return l.reconcileUncertainCreate(ctx, sourceID, targetID, relType, fmt.Errorf("dual-end confirmation failed after create: %w", err))
	}
	return &created, nil
}

// DeleteRelation removes a dependency edge by ID.
//
// Protocol:
//  1. require relation/source/target IDs
//  2. authoritative pre-capture + exact dual-end agreement (already absent both = idempotent success)
//  3. one mutation attempt under WithOpDeadline
//  4. ambiguous timeout → success only if fresh absence both ends proven
//  5. successful mutation verifies absence both ends; lingering/unknown fails closed
func (l *LinearProvider) DeleteRelation(ctx context.Context, relationID, sourceID, targetID string) error {
	relationID = strings.TrimSpace(relationID)
	sourceID = strings.TrimSpace(sourceID)
	targetID = strings.TrimSpace(targetID)
	if relationID == "" || sourceID == "" || targetID == "" {
		return fmt.Errorf("linear DeleteRelation: relation, source, and target ids are required")
	}
	if sourceID == targetID {
		return fmt.Errorf("linear DeleteRelation: self-edge endpoints are invalid")
	}
	l.relationMu.Lock()
	defer l.relationMu.Unlock()

	// Authoritative pre-capture: confirm current dual-end state.
	srcRels, err := l.ListRelations(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("linear DeleteRelation: pre-capture source: %w", err)
	}
	tgtRels, err := l.ListRelations(ctx, targetID)
	if err != nil {
		return fmt.Errorf("linear DeleteRelation: pre-capture target: %w", err)
	}

	srcHit, err := relationByIDExact(srcRels, relationID)
	if err != nil {
		return fmt.Errorf("linear DeleteRelation: source relation: %w", err)
	}
	tgtHit, err := relationByIDExact(tgtRels, relationID)
	if err != nil {
		return fmt.Errorf("linear DeleteRelation: target relation: %w", err)
	}
	if srcHit == nil && tgtHit == nil {
		return nil
	}
	if srcHit == nil || tgtHit == nil {
		return fmt.Errorf("linear DeleteRelation: pre-delete dual-end disagreement for %s (source=%v target=%v)", relationID, srcHit != nil, tgtHit != nil)
	}
	if srcHit.SourceTaskID != sourceID || srcHit.TargetTaskID != targetID ||
		tgtHit.SourceTaskID != sourceID || tgtHit.TargetTaskID != targetID {
		return fmt.Errorf("linear DeleteRelation: relation %s endpoint disagreement", relationID)
	}
	if !linearRelationFieldsEqual(*srcHit, *tgtHit) {
		return fmt.Errorf("linear DeleteRelation: relation %s field disagreement between endpoints", relationID)
	}

	vars := map[string]interface{}{"id": relationID}
	var resp struct {
		Data struct {
			IssueRelationDelete *struct {
				Success bool `json:"success"`
			} `json:"issueRelationDelete"`
		} `json:"data"`
	}

	dls := l.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := l.doGraphQL(writeCtx, linearDeleteRelationMutation, vars, &resp)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("linear", "DeleteRelation", OpMutate, dls.For(OpMutate), writeErr)
	}

	if writeErr != nil {
		// Ambiguous: success only if fresh absence both ends proven.
		if absErr := l.relationAbsentBothEnds(ctx, sourceID, targetID, relationID); absErr == nil {
			return nil
		} else {
			return &AmbiguousMutationError{
				Provider: "linear",
				Op:       "DeleteRelation",
				TaskID:   relationID,
				Want:     "relation absent both ends",
				WriteErr: writeErr,
				ReadErr:  absErr,
				Actual:   sourceID + "->" + targetID,
			}
		}
	}

	payload := resp.Data.IssueRelationDelete
	if payload == nil {
		return fmt.Errorf("linear DeleteRelation: nil issueRelationDelete payload")
	}
	if !payload.Success {
		return fmt.Errorf("linear DeleteRelation: success=false")
	}

	// Successful mutation verifies absence both ends; lingering fails closed.
	if err := l.relationAbsentBothEnds(ctx, sourceID, targetID, relationID); err != nil {
		return fmt.Errorf("linear DeleteRelation: post-mutation dual-end absence confirmation failed: %w", err)
	}
	return nil
}
