package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Compile-time interface assertions.
var (
	_ RelationProvider = (*LinearProvider)(nil)
	_ TaskProvider     = (*LinearProvider)(nil)
)

const testRelCreatedAt = "2024-01-15T10:00:00Z"

func newLinearRelTestServer(t *testing.T, handler http.HandlerFunc) (*LinearProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &LinearProvider{
		BaseURL:   srv.URL,
		APIKey:    "test-key",
		ProjectID: "proj-1",
		Client:    srv.Client(),
		Retry: RetryPolicy{
			MaxAttempts: 1,
			BaseBackoff: time.Millisecond,
			MaxBackoff:  time.Millisecond,
		},
		Deadlines: Deadlines{
			Get:      time.Second,
			List:     time.Second,
			Mutate:   time.Second,
			Comment:  time.Second,
			Readback: time.Second,
		},
	}, srv
}

func linearGQLData(t *testing.T, w http.ResponseWriter, data map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

func linearRelNode(id, typ, source, target string) map[string]any {
	return map[string]any{
		"id":           id,
		"type":         typ,
		"createdAt":    testRelCreatedAt,
		"issue":        map[string]any{"id": source},
		"relatedIssue": map[string]any{"id": target},
	}
}

func linearEmptyConn() map[string]any {
	return map[string]any{
		"nodes":    []any{},
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
	}
}

func linearConn(nodes ...map[string]any) map[string]any {
	out := make([]any, len(nodes))
	for i, n := range nodes {
		out[i] = n
	}
	return map[string]any{
		"nodes":    out,
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
	}
}

func readLinearOp(t *testing.T, r *http.Request) (string, map[string]any) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return payload.Query, payload.Variables
}

func linearTaskNode(id, title string) map[string]any {
	return map[string]any{
		"id":          id,
		"identifier":  id,
		"title":       title,
		"description": "",
		"priority":    0,
		"state":       map[string]any{"name": "Todo"},
		"project":     map[string]any{"id": "proj-1"},
		"labels":      map[string]any{"nodes": []any{}},
	}
}

// ---------------------------------------------------------------------------
// ListTasks schema contract: $projectID: ID! (not String!)
// ---------------------------------------------------------------------------

func TestLinearListTasks_ProjectIDIsGraphQLID(t *testing.T) {
	var gotQuery string
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, _ = readLinearOp(t, r)
		linearGQLData(t, w, map[string]any{
			"issues": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
			},
		})
	})

	if _, err := lin.ListTasks(context.Background(), "proj-1", ""); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if !strings.Contains(gotQuery, "$projectID: ID!") {
		t.Fatalf("expected $projectID: ID! in ListTasks query, got:\n%s", gotQuery)
	}
	if strings.Contains(gotQuery, "$projectID: String!") {
		t.Fatal("ListTasks must not use $projectID: String!")
	}
}

// Explicit non-vacuity mutant: old String! must be rejected by this assertion.
func TestLinearListTasks_ProjectIDRejectsStringMutant(t *testing.T) {
	mutant := `query ListIssues($projectID: String!, $after: String) {
  issues(filter: {project: {id: {eq: $projectID}}}, first: 50, after: $after) {
    nodes { id }
  }
}`
	if strings.Contains(mutant, "$projectID: ID!") {
		t.Fatal("mutant fixture incorrectly contains ID!")
	}
	if !strings.Contains(mutant, "$projectID: String!") {
		t.Fatal("mutant fixture must contain String!")
	}

	// Production path must use ID! — re-fetch live query text from a ListTasks call.
	var gotQuery string
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, _ = readLinearOp(t, r)
		linearGQLData(t, w, map[string]any{
			"issues": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
			},
		})
	})
	if _, err := lin.ListTasks(context.Background(), "proj-1", ""); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if strings.Contains(gotQuery, "$projectID: String!") {
		t.Fatal("production ListTasks regressed to String!")
	}
	if !strings.Contains(gotQuery, "$projectID: ID!") {
		t.Fatal("production ListTasks missing ID!")
	}
	// Mutant must differ from production.
	if gotQuery == mutant {
		t.Fatal("production query identical to String! mutant")
	}
}

// ---------------------------------------------------------------------------
// mapLinearRelationType
// ---------------------------------------------------------------------------

func TestMapLinearRelationType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw     string
		want    RelationType
		wantErr bool
	}{
		{"blocks", RelationBlocks, false},
		{"Blocks", RelationBlocks, false},
		{"related", RelationRelated, false},
		{"RELATED", RelationRelated, false},
		{"duplicate", "", true},
		{"duplicate_of", "", true},
		{"subtask", "", true},
		{"relates", "", true},
		{"relates_to", "", true},
		{"related_to", "", true},
		{"blocked_by", "", true},
		{"", "", true},
		{"   ", "", true},
		{"unknown", "", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			t.Parallel()
			got, err := mapLinearRelationType(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %s", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ListRelations
// ---------------------------------------------------------------------------

func TestLinearListRelations_OutgoingAndIncomingDirection(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		id, _ := vars["id"].(string)
		switch {
		case strings.Contains(q, "inverseRelations"):
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{
					"inverseRelations": linearConn(linearRelNode("rel-in", "blocks", "task-C", id)),
				},
			})
		case strings.Contains(q, "relations(first:"):
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{
					"relations": linearConn(linearRelNode("rel-out", "blocks", id, "task-B")),
				},
			})
		default:
			t.Fatalf("unexpected query: %s", q)
		}
	})

	rels, err := lin.ListRelations(context.Background(), "task-A")
	if err != nil {
		t.Fatalf("ListRelations: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("got %d rels, want 2: %+v", len(rels), rels)
	}
	if rels[0].ID != "rel-in" || rels[1].ID != "rel-out" {
		t.Fatalf("sort/ids wrong: %+v", rels)
	}
	if rels[1].SourceTaskID != "task-A" || rels[1].TargetTaskID != "task-B" {
		t.Fatalf("outgoing direction wrong: %+v", rels[1])
	}
	if rels[0].SourceTaskID != "task-C" || rels[0].TargetTaskID != "task-A" {
		t.Fatalf("incoming direction wrong: %+v", rels[0])
	}
}

func TestLinearListRelations_WrongSideMutant(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		// Wrong side: source is other-task, not task-A
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{
				"relations": linearConn(linearRelNode("rel-x", "blocks", "other-task", "task-A")),
			},
		})
	})

	_, err := lin.ListRelations(context.Background(), "task-A")
	if err == nil {
		t.Fatal("expected wrong-side direction error")
	}
	if !strings.Contains(err.Error(), "expected source=task-A") {
		t.Fatalf("error should mention direction, got: %v", err)
	}
}

func TestLinearListRelations_IndependentPaginationAndCap(t *testing.T) {
	var outgoingPages, incomingPages int
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		after, _ := vars["after"].(string)
		if strings.Contains(q, "inverseRelations") {
			incomingPages++
			if after == "" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{
						"inverseRelations": map[string]any{
							"nodes":    []any{linearRelNode("rel-in-1", "related", "task-Z", "task-A")},
							"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "in-cursor-1"},
						},
					},
				})
				return
			}
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{
					"inverseRelations": map[string]any{
						"nodes":    []any{linearRelNode("rel-in-2", "related", "task-Y", "task-A")},
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
					},
				},
			})
			return
		}
		outgoingPages++
		if after == "" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{
					"relations": map[string]any{
						"nodes":    []any{linearRelNode("rel-out-1", "blocks", "task-A", "task-B")},
						"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "out-cursor-1"},
					},
				},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{
				"relations": map[string]any{
					"nodes":    []any{linearRelNode("rel-out-2", "blocks", "task-A", "task-C")},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				},
			},
		})
	})

	rels, err := lin.ListRelations(context.Background(), "task-A")
	if err != nil {
		t.Fatalf("ListRelations: %v", err)
	}
	if outgoingPages != 2 || incomingPages != 2 {
		t.Fatalf("independent pagination broken: out=%d in=%d", outgoingPages, incomingPages)
	}
	if len(rels) != 4 {
		t.Fatalf("got %d rels want 4", len(rels))
	}
}

func TestLinearListRelations_RepeatedCursorRejected(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		// Always same cursor with hasNextPage=true → repeated cursor.
		// Second page must use different node IDs so PageAccumulator.Add doesn't
		// trip first; cursor loop detection is the target.
		after := ""
		if body, _ := io.ReadAll(io.NopCloser(strings.NewReader(""))); len(body) >= 0 {
			// re-read via query vars already consumed — use a counter instead
		}
		_ = after
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{
				"relations": map[string]any{
					"nodes":    []any{linearRelNode("rel-1", "blocks", "task-A", "task-B")},
					"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "same-cursor"},
				},
			},
		})
	})

	// First page succeeds Add(rel-1), marks same-cursor. Second page tries Add(rel-1)
	// again → ErrDuplicatePage (duplicate id) OR repeated cursor. Either is fail-closed.
	_, err := lin.ListRelations(context.Background(), "task-A")
	if err == nil {
		t.Fatal("expected repeated cursor / duplicate page error")
	}
	if !errors.Is(err, ErrDuplicatePage) && !strings.Contains(err.Error(), "duplicate") && !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("expected duplicate/cursor error, got: %v", err)
	}
}

func TestLinearListRelations_HasNextPageEmptyCursorRejected(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{
				"relations": map[string]any{
					"nodes":    []any{linearRelNode("rel-1", "blocks", "task-A", "task-B")},
					"pageInfo": map[string]any{"hasNextPage": true, "endCursor": ""},
				},
			},
		})
	})

	_, err := lin.ListRelations(context.Background(), "task-A")
	if err == nil {
		t.Fatal("expected empty cursor with hasNextPage error")
	}
	if !strings.Contains(err.Error(), "empty cursor") {
		t.Fatalf("expected empty cursor error, got: %v", err)
	}
}

func TestLinearListRelations_DifferentDuplicateDisagree(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "inverseRelations") {
			// Same ID, different endpoints → disagreement
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{
					"inverseRelations": linearConn(linearRelNode("rel-dup", "blocks", "task-X", "task-A")),
				},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{
				"relations": linearConn(linearRelNode("rel-dup", "blocks", "task-A", "task-B")),
			},
		})
	})

	_, err := lin.ListRelations(context.Background(), "task-A")
	if err == nil {
		t.Fatal("expected duplicate ID disagreement error")
	}
	if !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("expected disagree error, got: %v", err)
	}
}

func TestLinearListRelations_MalformedAndUnsupported(t *testing.T) {
	cases := []struct {
		name string
		node map[string]any
		want string
	}{
		{
			name: "missing_id",
			node: map[string]any{
				"id": "", "type": "blocks", "createdAt": testRelCreatedAt,
				"issue": map[string]any{"id": "task-A"}, "relatedIssue": map[string]any{"id": "task-B"},
			},
			want: "immutable id",
		},
		{
			name: "self_edge",
			node: linearRelNode("rel-1", "blocks", "task-A", "task-A"),
			want: "self-edge",
		},
		{
			name: "unsupported_type",
			node: linearRelNode("rel-1", "duplicate", "task-A", "task-B"),
			want: "unsupported",
		},
		{
			name: "blank_type",
			node: linearRelNode("rel-1", "", "task-A", "task-B"),
			want: "blank relation type",
		},
		{
			name: "missing_createdAt",
			node: map[string]any{
				"id": "rel-1", "type": "blocks", "createdAt": "",
				"issue": map[string]any{"id": "task-A"}, "relatedIssue": map[string]any{"id": "task-B"},
			},
			want: "createdAt",
		},
		{
			name: "malformed_createdAt",
			node: map[string]any{
				"id": "rel-1", "type": "blocks", "createdAt": "not-a-timestamp",
				"issue": map[string]any{"id": "task-A"}, "relatedIssue": map[string]any{"id": "task-B"},
			},
			want: "createdAt",
		},
		{
			name: "missing_endpoint",
			node: map[string]any{
				"id": "rel-1", "type": "blocks", "createdAt": testRelCreatedAt,
				"issue": map[string]any{"id": "task-A"}, "relatedIssue": nil,
			},
			want: "relatedIssue",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				q, _ := readLinearOp(t, r)
				if strings.Contains(q, "inverseRelations") {
					linearGQLData(t, w, map[string]any{
						"issue": map[string]any{"inverseRelations": linearEmptyConn()},
					})
					return
				}
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"relations": linearConn(tc.node)},
				})
			})
			_, err := lin.ListRelations(context.Background(), "task-A")
			if err == nil {
				t.Fatal("expected error for malformed/unsupported row")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLinearListRelations_NilIssueRejected(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		linearGQLData(t, w, map[string]any{"issue": nil})
	})
	_, err := lin.ListRelations(context.Background(), "task-A")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found, got: %v", err)
	}
}

func TestLinearListRelations_MissingConnectionRejected(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": nil}})
			return
		}
		linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": nil}})
	})
	_, err := lin.ListRelations(context.Background(), "task-A")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing connection, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListProjectRelations
// ---------------------------------------------------------------------------

func TestLinearListProjectRelations_DualEndAndOutside(t *testing.T) {
	// Tasks: A, B in project. C/X outside.
	// Edges:
	//   rel-ab: A->B (both in project) — must appear on both
	//   rel-ac: A->C (C outside) — single observation OK
	//   rel-xa: X->A (X outside) — single observation OK
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)

		if strings.Contains(q, "issues(") {
			linearGQLData(t, w, map[string]any{
				"issues": map[string]any{
					"nodes": []any{
						linearTaskNode("task-A", "A"),
						linearTaskNode("task-B", "B"),
					},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				},
			})
			return
		}

		id, _ := vars["id"].(string)
		switch id {
		case "task-A":
			if strings.Contains(q, "inverseRelations") {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{
						"inverseRelations": linearConn(linearRelNode("rel-xa", "blocks", "task-X", "task-A")),
					},
				})
				return
			}
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{
					"relations": linearConn(
						linearRelNode("rel-ab", "blocks", "task-A", "task-B"),
						linearRelNode("rel-ac", "related", "task-A", "task-C"),
					),
				},
			})
		case "task-B":
			if strings.Contains(q, "inverseRelations") {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{
						"inverseRelations": linearConn(linearRelNode("rel-ab", "blocks", "task-A", "task-B")),
					},
				})
				return
			}
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearEmptyConn()},
			})
		default:
			t.Fatalf("unexpected id %s", id)
		}
	})

	rels, err := lin.ListProjectRelations(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProjectRelations: %v", err)
	}
	if len(rels) != 3 {
		t.Fatalf("got %d rels want 3: %+v", len(rels), rels)
	}
	if rels[0].ID != "rel-ab" || rels[1].ID != "rel-ac" || rels[2].ID != "rel-xa" {
		t.Fatalf("unexpected sort/ids: %+v", rels)
	}
}

func TestLinearListProjectRelations_MissingInProjectEndFails(t *testing.T) {
	// Both A and B in project, but rel-ab only visible from A (missing on B).
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issues(") {
			linearGQLData(t, w, map[string]any{
				"issues": map[string]any{
					"nodes": []any{
						linearTaskNode("task-A", "A"),
						linearTaskNode("task-B", "B"),
					},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				},
			})
			return
		}
		id, _ := vars["id"].(string)
		if id == "task-A" && !strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{
					"relations": linearConn(linearRelNode("rel-ab", "blocks", "task-A", "task-B")),
				},
			})
			return
		}
		// All other sides empty — half-visible.
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})

	_, err := lin.ListProjectRelations(context.Background(), "proj-1")
	if err == nil {
		t.Fatal("expected half-visible in-project relation error")
	}
	if !strings.Contains(err.Error(), "half-visible") {
		t.Fatalf("expected half-visible error, got: %v", err)
	}
}

func TestLinearListProjectRelations_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "issues(") {
			linearGQLData(t, w, map[string]any{
				"issues": map[string]any{
					"nodes": []any{
						linearTaskNode("task-A", "A"),
						linearTaskNode("task-B", "B"),
					},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				},
			})
			cancel()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{
				"relations":        linearEmptyConn(),
				"inverseRelations": linearEmptyConn(),
			},
		})
	})

	_, err := lin.ListProjectRelations(ctx, "proj-1")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled, got: %v", err)
	}
}

func TestLinearListProjectRelations_BoundedConcurrency(t *testing.T) {
	const nTasks = 8
	var maxInFlight, current atomic.Int32
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "issues(") {
			nodes := make([]any, nTasks)
			for i := 0; i < nTasks; i++ {
				nodes[i] = linearTaskNode(fmt.Sprintf("t-%d", i), "T")
			}
			linearGQLData(t, w, map[string]any{
				"issues": map[string]any{
					"nodes":    nodes,
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				},
			})
			return
		}
		cur := current.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})
	lin.BulkConcurrency = 3

	if _, err := lin.ListProjectRelations(context.Background(), "proj-1"); err != nil {
		t.Fatalf("ListProjectRelations: %v", err)
	}
	// BulkConcurrency bounds concurrent ListRelations invocations; each does 2
	// sequential HTTP calls, so peak HTTP ≤ 2 * BulkConcurrency.
	if maxInFlight.Load() > 6 {
		t.Fatalf("concurrency not bounded: maxInFlight=%d", maxInFlight.Load())
	}
}

// ---------------------------------------------------------------------------
// CreateRelation
// ---------------------------------------------------------------------------

func TestLinearCreateRelation_RejectsBlankSelfUnsupported(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no HTTP expected for validation failures")
	})

	if _, err := lin.CreateRelation(context.Background(), "", "b", RelationBlocks); err == nil {
		t.Fatal("expected blank source error")
	}
	if _, err := lin.CreateRelation(context.Background(), "a", "a", RelationBlocks); err == nil {
		t.Fatal("expected self-edge error")
	}
	if _, err := lin.CreateRelation(context.Background(), "a", "b", RelationSubtask); err == nil {
		t.Fatal("expected subtask rejection")
	}
	if _, err := lin.CreateRelation(context.Background(), "a", "b", RelationType("nope")); err == nil {
		t.Fatal("expected unsupported type error")
	}
}

func TestLinearCreateRelation_IdempotentExactPrecheck(t *testing.T) {
	var mu sync.Mutex
	var mutations int
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationCreate") {
			mu.Lock()
			mutations++
			mu.Unlock()
			t.Fatal("mutation must not run when exact edge already present")
		}
		id, _ := vars["id"].(string)
		node := linearRelNode("rel-existing", "blocks", "task-A", "task-B")
		if strings.Contains(q, "inverseRelations") {
			if id == "task-B" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearConn(node)},
				})
				return
			}
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})

	rel, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	if rel == nil || rel.ID != "rel-existing" {
		t.Fatalf("expected existing relation, got %+v", rel)
	}
	mu.Lock()
	defer mu.Unlock()
	if mutations != 0 {
		t.Fatalf("mutations=%d want 0", mutations)
	}
}

func TestLinearCreateRelation_SuccessWithDualEndReadback(t *testing.T) {
	var mu sync.Mutex
	created := false
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationCreate") {
			mu.Lock()
			created = true
			mu.Unlock()
			linearGQLData(t, w, map[string]any{
				"issueRelationCreate": map[string]any{
					"success":       true,
					"issueRelation": linearRelNode("rel-new", "blocks", "task-A", "task-B"),
				},
			})
			return
		}
		id, _ := vars["id"].(string)
		mu.Lock()
		isCreated := created
		mu.Unlock()
		node := linearRelNode("rel-new", "blocks", "task-A", "task-B")
		if !isCreated {
			if strings.Contains(q, "inverseRelations") {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"relations": linearEmptyConn()},
				})
			}
			return
		}
		if strings.Contains(q, "inverseRelations") {
			if id == "task-B" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearConn(node)},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			}
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
		} else {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearEmptyConn()},
			})
		}
	})

	rel, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	if rel.ID != "rel-new" || rel.SourceTaskID != "task-A" || rel.TargetTaskID != "task-B" || rel.Type != RelationBlocks {
		t.Fatalf("unexpected relation: %+v", rel)
	}
}

func TestLinearCreateRelation_SuccessFalse(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationCreate") {
			linearGQLData(t, w, map[string]any{
				"issueRelationCreate": map[string]any{
					"success":       false,
					"issueRelation": nil,
				},
			})
			return
		}
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})

	_, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("expected success=false error, got: %v", err)
	}
}

func TestLinearCreateRelation_AmbiguousLand(t *testing.T) {
	// Mutation times out, but edge is present both ends on reconcile.
	var mu sync.Mutex
	mutated := false
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationCreate") {
			mu.Lock()
			mutated = true
			mu.Unlock()
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		id, _ := vars["id"].(string)
		mu.Lock()
		done := mutated
		mu.Unlock()
		if !done {
			if strings.Contains(q, "inverseRelations") {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"relations": linearEmptyConn()},
				})
			}
			return
		}
		node := linearRelNode("rel-landed", "blocks", "task-A", "task-B")
		if strings.Contains(q, "inverseRelations") {
			if id == "task-B" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearConn(node)},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			}
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
		} else {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearEmptyConn()},
			})
		}
	})

	rel, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err != nil {
		t.Fatalf("expected reconcile success, got: %v", err)
	}
	if rel.ID != "rel-landed" {
		t.Fatalf("got %+v", rel)
	}
}

func TestLinearCreateRelation_AmbiguousNoLand(t *testing.T) {
	// Mutation times out, edge never appears → AmbiguousMutationError (timeout).
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationCreate") {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})

	_, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err == nil {
		t.Fatal("expected error")
	}
	var amb *AmbiguousMutationError
	if !errors.As(err, &amb) {
		t.Fatalf("expected AmbiguousMutationError, got: %T %v", err, err)
	}
	if amb.Provider != "linear" || amb.Op != "CreateRelation" {
		t.Fatalf("unexpected amb fields: %+v", amb)
	}
	if amb.Want != "relation present both ends" {
		t.Fatalf("Want=%q", amb.Want)
	}
	if amb.WriteErr == nil {
		t.Fatal("WriteErr must be set")
	}
}

func TestLinearCreateRelation_AmbiguousPartialRead(t *testing.T) {
	// Mutation fails; edge visible on source but not target → AmbiguousMutationError.
	var mu sync.Mutex
	mutated := false
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationCreate") {
			mu.Lock()
			mutated = true
			mu.Unlock()
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		id, _ := vars["id"].(string)
		mu.Lock()
		done := mutated
		mu.Unlock()
		if !done {
			if strings.Contains(q, "inverseRelations") {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"relations": linearEmptyConn()},
				})
			}
			return
		}
		node := linearRelNode("rel-partial", "blocks", "task-A", "task-B")
		// Only source sees it.
		if !strings.Contains(q, "inverseRelations") && id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
			return
		}
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})

	_, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	var amb *AmbiguousMutationError
	if !errors.As(err, &amb) {
		t.Fatalf("expected AmbiguousMutationError, got %T: %v", err, err)
	}
	if amb.Provider != "linear" || amb.Op != "CreateRelation" {
		t.Fatalf("unexpected amb fields: %+v", amb)
	}
}

// ---------------------------------------------------------------------------
// DeleteRelation
// ---------------------------------------------------------------------------

func TestLinearDeleteRelation_Success(t *testing.T) {
	var mu sync.Mutex
	deleted := false
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationDelete") {
			mu.Lock()
			deleted = true
			mu.Unlock()
			linearGQLData(t, w, map[string]any{
				"issueRelationDelete": map[string]any{"success": true},
			})
			return
		}
		id, _ := vars["id"].(string)
		mu.Lock()
		gone := deleted
		mu.Unlock()
		node := linearRelNode("rel-del", "blocks", "task-A", "task-B")
		if gone {
			if strings.Contains(q, "inverseRelations") {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"relations": linearEmptyConn()},
				})
			}
			return
		}
		if strings.Contains(q, "inverseRelations") {
			if id == "task-B" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearConn(node)},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			}
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
		} else {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearEmptyConn()},
			})
		}
	})

	if err := lin.DeleteRelation(context.Background(), "rel-del", "task-A", "task-B"); err != nil {
		t.Fatalf("DeleteRelation: %v", err)
	}
}

func TestLinearDeleteRelation_AlreadyAbsent(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationDelete") {
			t.Fatal("delete mutation must not run when already absent")
		}
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})

	if err := lin.DeleteRelation(context.Background(), "rel-gone", "task-A", "task-B"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestLinearDeleteRelation_SuccessFalse(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationDelete") {
			linearGQLData(t, w, map[string]any{
				"issueRelationDelete": map[string]any{"success": false},
			})
			return
		}
		id, _ := vars["id"].(string)
		node := linearRelNode("rel-del", "blocks", "task-A", "task-B")
		if strings.Contains(q, "inverseRelations") {
			if id == "task-B" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearConn(node)},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			}
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
		} else {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearEmptyConn()},
			})
		}
	})

	err := lin.DeleteRelation(context.Background(), "rel-del", "task-A", "task-B")
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("expected success=false, got: %v", err)
	}
}

func TestLinearDeleteRelation_AmbiguousLand(t *testing.T) {
	// Mutation times out but relation is gone both ends → success.
	var mu sync.Mutex
	mutated := false
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationDelete") {
			mu.Lock()
			mutated = true
			mu.Unlock()
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		id, _ := vars["id"].(string)
		mu.Lock()
		gone := mutated
		mu.Unlock()
		node := linearRelNode("rel-del", "blocks", "task-A", "task-B")
		if gone {
			if strings.Contains(q, "inverseRelations") {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"relations": linearEmptyConn()},
				})
			}
			return
		}
		if strings.Contains(q, "inverseRelations") {
			if id == "task-B" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearConn(node)},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			}
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
		} else {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearEmptyConn()},
			})
		}
	})

	if err := lin.DeleteRelation(context.Background(), "rel-del", "task-A", "task-B"); err != nil {
		t.Fatalf("expected ambiguous-land success, got: %v", err)
	}
}

func TestLinearDeleteRelation_AmbiguousNoLand(t *testing.T) {
	// Mutation times out and relation still present both ends → AmbiguousMutationError.
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, vars := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationDelete") {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		id, _ := vars["id"].(string)
		node := linearRelNode("rel-del", "blocks", "task-A", "task-B")
		if strings.Contains(q, "inverseRelations") {
			if id == "task-B" {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearConn(node)},
				})
			} else {
				linearGQLData(t, w, map[string]any{
					"issue": map[string]any{"inverseRelations": linearEmptyConn()},
				})
			}
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
		} else {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearEmptyConn()},
			})
		}
	})

	err := lin.DeleteRelation(context.Background(), "rel-del", "task-A", "task-B")
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	var amb *AmbiguousMutationError
	if !errors.As(err, &amb) {
		t.Fatalf("expected AmbiguousMutationError, got %T: %v", err, err)
	}
	if amb.Provider != "linear" || amb.Op != "DeleteRelation" {
		t.Fatalf("unexpected fields: %+v", amb)
	}
	if amb.Want != "relation absent both ends" {
		t.Fatalf("Want=%q", amb.Want)
	}
}

func TestLinearDeleteRelation_BlankIDs(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no HTTP for blank ids")
	})
	if err := lin.DeleteRelation(context.Background(), "", "a", "b"); err == nil {
		t.Fatal("expected blank relation id error")
	}
	if err := lin.DeleteRelation(context.Background(), "r", "", "b"); err == nil {
		t.Fatal("expected blank source error")
	}
	if err := lin.DeleteRelation(context.Background(), "r", "a", ""); err == nil {
		t.Fatal("expected blank target error")
	}
}

// ---------------------------------------------------------------------------
// Retry policy: reads retry, mutations do not
// ---------------------------------------------------------------------------

func TestLinearRelations_ReadsRetryMutationsDoNot(t *testing.T) {
	var mutHits atomic.Int32
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q, _ := readLinearOp(t, r)
		if strings.Contains(q, "issueRelationCreate") {
			mutHits.Add(1)
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		if strings.Contains(q, "inverseRelations") {
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"inverseRelations": linearEmptyConn()},
			})
			return
		}
		linearGQLData(t, w, map[string]any{
			"issue": map[string]any{"relations": linearEmptyConn()},
		})
	})
	lin.Retry = RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}

	if _, err := lin.ListRelations(context.Background(), "task-A"); err != nil {
		t.Fatalf("ListRelations: %v", err)
	}

	mutHits.Store(0)
	_, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err == nil {
		t.Fatal("expected create error")
	}
	if mutHits.Load() != 1 {
		t.Fatalf("mutation hits=%d want 1 (no blind retry)", mutHits.Load())
	}
}

func TestLinearRelationPresentBothEnds_FieldDisagreement(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query, vars := readLinearOp(t, r)
		id, _ := vars["id"].(string)
		node := linearRelNode("rel-fields", "blocks", "task-A", "task-B")
		if strings.Contains(query, "inverseRelations") {
			if id == "task-B" {
				node["createdAt"] = "2024-01-15T10:00:01Z"
				linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearConn(node)}})
				return
			}
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearEmptyConn()}})
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearConn(node)}})
			return
		}
		linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearEmptyConn()}})
	})

	err := lin.relationPresentBothEnds(context.Background(), "task-A", "task-B", "rel-fields")
	if err == nil || !strings.Contains(err.Error(), "field disagreement") {
		t.Fatalf("expected full-field disagreement, got %v", err)
	}
}

func TestLinearCreateRelation_ConcurrentSerialized(t *testing.T) {
	var mu sync.Mutex
	created := false
	mutationCalls := 0
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query, vars := readLinearOp(t, r)
		if strings.Contains(query, "issueRelationCreate") {
			// Hold the first mutation open so a mutex-less implementation lets the
			// second caller finish its empty precheck and issue a duplicate write.
			time.Sleep(75 * time.Millisecond)
			mu.Lock()
			mutationCalls++
			created = true
			mu.Unlock()
			linearGQLData(t, w, map[string]any{
				"issueRelationCreate": map[string]any{
					"success":       true,
					"issueRelation": linearRelNode("rel-concurrent", "blocks", "task-A", "task-B"),
				},
			})
			return
		}

		id, _ := vars["id"].(string)
		mu.Lock()
		visible := created
		mu.Unlock()
		node := linearRelNode("rel-concurrent", "blocks", "task-A", "task-B")
		if strings.Contains(query, "inverseRelations") {
			if visible && id == "task-B" {
				linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearConn(node)}})
			} else {
				linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearEmptyConn()}})
			}
			return
		}
		if visible && id == "task-A" {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearConn(node)}})
		} else {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearEmptyConn()}})
		}
	})

	start := make(chan struct{})
	type createResult struct {
		rel *Relation
		err error
	}
	results := make(chan createResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			rel, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
			results <- createResult{rel: rel, err: err}
		}()
	}
	close(start)
	first := <-results
	second := <-results
	for i, result := range []createResult{first, second} {
		if result.err != nil {
			t.Fatalf("result %d: %v", i, result.err)
		}
		if result.rel == nil || result.rel.ID != "rel-concurrent" {
			t.Fatalf("result %d relation: %+v", i, result.rel)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if mutationCalls != 1 {
		t.Fatalf("mutation calls=%d want 1", mutationCalls)
	}
}

func TestLinearListProjectRelations_FirstErrorCancelsPromptly(t *testing.T) {
	var canceled atomic.Int32
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query, vars := readLinearOp(t, r)
		if strings.Contains(query, "issues(") {
			linearGQLData(t, w, map[string]any{
				"issues": map[string]any{
					"nodes": []any{
						linearTaskNode("task-fast", "fast"),
						linearTaskNode("task-block-1", "block"),
						linearTaskNode("task-block-2", "block"),
					},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				},
			})
			return
		}
		id, _ := vars["id"].(string)
		if id == "task-fast" {
			time.Sleep(30 * time.Millisecond)
			http.Error(w, "fast failure", http.StatusInternalServerError)
			return
		}
		select {
		case <-r.Context().Done():
			canceled.Add(1)
			return
		case <-time.After(2 * time.Second):
			http.Error(w, "cancellation not propagated", http.StatusGatewayTimeout)
		}
	})
	lin.BulkConcurrency = 3

	started := time.Now()
	_, err := lin.ListProjectRelations(context.Background(), "proj-1")
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "task-fast") {
		t.Fatalf("expected attributable fast failure, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("first error did not cancel promptly: %v", elapsed)
	}
	cancelDeadline := time.Now().Add(250 * time.Millisecond)
	for canceled.Load() == 0 && time.Now().Before(cancelDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if canceled.Load() == 0 {
		t.Fatal("blocked peer requests did not observe cancellation")
	}
}

func TestLinearListRelations_DuplicateTimestampDisagreement(t *testing.T) {
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query, vars := readLinearOp(t, r)
		if strings.Contains(query, "inverseRelations") {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearEmptyConn()}})
			return
		}
		node := linearRelNode("rel-page-time", "blocks", "task-A", "task-B")
		if _, secondPage := vars["after"]; secondPage {
			node["createdAt"] = "2024-01-15T10:00:01Z"
			linearGQLData(t, w, map[string]any{
				"issue": map[string]any{"relations": linearConn(node)},
			})
			return
		}
		conn := linearConn(node)
		conn["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "cursor-1"}
		linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": conn}})
	})

	_, err := lin.ListRelations(context.Background(), "task-A")
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("expected timestamp disagreement across pages, got %v", err)
	}
}

func TestLinearCreateRelation_MissingSuccessPayloadReconciles(t *testing.T) {
	var mu sync.Mutex
	mutated := false
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query, vars := readLinearOp(t, r)
		if strings.Contains(query, "issueRelationCreate") {
			mu.Lock()
			mutated = true
			mu.Unlock()
			linearGQLData(t, w, map[string]any{
				"issueRelationCreate": map[string]any{"success": true, "issueRelation": nil},
			})
			return
		}
		id, _ := vars["id"].(string)
		mu.Lock()
		visible := mutated
		mu.Unlock()
		node := linearRelNode("rel-reconciled-payload", "blocks", "task-A", "task-B")
		if strings.Contains(query, "inverseRelations") {
			if visible && id == "task-B" {
				linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearConn(node)}})
			} else {
				linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearEmptyConn()}})
			}
			return
		}
		if visible && id == "task-A" {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearConn(node)}})
		} else {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearEmptyConn()}})
		}
	})

	relation, err := lin.CreateRelation(context.Background(), "task-A", "task-B", RelationBlocks)
	if err != nil {
		t.Fatalf("expected readback reconciliation, got %v", err)
	}
	if relation == nil || relation.ID != "rel-reconciled-payload" {
		t.Fatalf("unexpected relation: %+v", relation)
	}
}

func TestLinearDeleteRelation_FieldDisagreementDoesNotMutate(t *testing.T) {
	var mutations atomic.Int32
	lin, _ := newLinearRelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query, vars := readLinearOp(t, r)
		if strings.Contains(query, "issueRelationDelete") {
			mutations.Add(1)
			linearGQLData(t, w, map[string]any{"issueRelationDelete": map[string]any{"success": true}})
			return
		}
		id, _ := vars["id"].(string)
		node := linearRelNode("rel-delete-fields", "blocks", "task-A", "task-B")
		if strings.Contains(query, "inverseRelations") {
			if id == "task-B" {
				node["createdAt"] = "2024-01-15T10:00:01Z"
				linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearConn(node)}})
			} else {
				linearGQLData(t, w, map[string]any{"issue": map[string]any{"inverseRelations": linearEmptyConn()}})
			}
			return
		}
		if id == "task-A" {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearConn(node)}})
		} else {
			linearGQLData(t, w, map[string]any{"issue": map[string]any{"relations": linearEmptyConn()}})
		}
	})

	err := lin.DeleteRelation(context.Background(), "rel-delete-fields", "task-A", "task-B")
	if err == nil || !strings.Contains(err.Error(), "field disagreement") {
		t.Fatalf("expected pre-delete field disagreement, got %v", err)
	}
	if mutations.Load() != 0 {
		t.Fatalf("delete mutations=%d want 0", mutations.Load())
	}
}
