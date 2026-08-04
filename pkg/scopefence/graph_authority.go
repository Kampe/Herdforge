package scopefence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SQLiteGraphAuthority reads a graph snapshot from the same durable store and
// binds it to the configured repository, revision, and file count. It refuses
// incomplete or implausible indexes before returning trusted graph data.
type SQLiteGraphAuthority struct {
	store            *SQLiteStore
	repository       string
	expectedRevision string
	expectedFiles    int
}

func NewSQLiteGraphAuthority(store *SQLiteStore, repository, expectedRevision string, expectedFiles int) *SQLiteGraphAuthority {
	return &SQLiteGraphAuthority{store: store, repository: repository, expectedRevision: expectedRevision, expectedFiles: expectedFiles}
}

// PutGraphSnapshot durably publishes one complete graph snapshot for a repo.
// Publication is separate from Current so callers cannot accidentally treat a
// request-supplied graph as trusted authority.
func (s *SQLiteStore) PutGraphSnapshot(ctx context.Context, repository string, graph Graph) error {
	if s == nil || s.db == nil || repository == "" {
		return errors.New("scopefence: graph snapshot store is not configured")
	}
	if err := graph.validate(graph.Revision, graph.Files); err != nil {
		return fmt.Errorf("scopefence: refusing incomplete graph snapshot: %w", err)
	}
	encoded, err := json.Marshal(graph)
	if err != nil {
		return fmt.Errorf("scopefence: encode graph snapshot: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO scopefence_graph (repository, graph_json) VALUES (?, ?) ON CONFLICT(repository) DO UPDATE SET graph_json = excluded.graph_json`, repository, encoded)
	return err
}

func (a *SQLiteGraphAuthority) Current(ctx context.Context) (TrustedGraph, error) {
	if a == nil || a.store == nil || a.store.db == nil || a.repository == "" {
		return TrustedGraph{}, errors.New("scopefence: graph authority is not configured")
	}
	var encoded []byte
	if err := a.store.db.QueryRowContext(ctx, `SELECT graph_json FROM scopefence_graph WHERE repository = ?`, a.repository).Scan(&encoded); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TrustedGraph{}, errors.New("scopefence: trusted graph snapshot unavailable")
		}
		return TrustedGraph{}, fmt.Errorf("scopefence: read trusted graph snapshot: %w", err)
	}
	var graph Graph
	if err := json.Unmarshal(encoded, &graph); err != nil {
		return TrustedGraph{}, fmt.Errorf("scopefence: decode trusted graph snapshot: %w", err)
	}
	expectedRevision, expectedFiles := a.expectedRevision, a.expectedFiles
	if expectedRevision == "" && expectedFiles == 0 {
		expectedRevision, expectedFiles = graph.Revision, graph.Files
	}
	if err := graph.validate(expectedRevision, expectedFiles); err != nil {
		return TrustedGraph{}, fmt.Errorf("scopefence: trusted graph snapshot rejected: %w", err)
	}
	return TrustedGraph{Snapshot: graph, ExpectedRevision: expectedRevision, ExpectedFiles: expectedFiles}, nil
}

type ResolvingFence struct {
	Fence     Fence
	Authority ScopeAuthority
}

type SQLiteScopeAuthority struct{ store *SQLiteStore }

func NewSQLiteScopeAuthority(store *SQLiteStore) *SQLiteScopeAuthority {
	return &SQLiteScopeAuthority{store: store}
}

func (a *SQLiteScopeAuthority) Resolve(ctx context.Context, repository, task, revision string) (Scope, error) {
	if a == nil || a.store == nil || repository == "" || task == "" || revision == "" {
		return Scope{}, errors.New("scopefence: scope authority is not configured")
	}
	var encoded []byte
	if err := a.store.db.QueryRowContext(ctx, `SELECT scope_json FROM scopefence_scopes WHERE repository = ? AND task = ? AND graph_revision = ?`, repository, task, revision).Scan(&encoded); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Scope{}, errors.New("scopefence: trusted task scope unavailable")
		}
		return Scope{}, fmt.Errorf("scopefence: read trusted task scope: %w", err)
	}
	var scope Scope
	if err := json.Unmarshal(encoded, &scope); err != nil {
		return Scope{}, fmt.Errorf("scopefence: decode trusted task scope: %w", err)
	}
	canonical, err := canonicalScope(scope)
	if err != nil || !scopesEqual(scope, canonical) {
		return Scope{}, errors.New("scopefence: trusted task scope is noncanonical")
	}
	return canonical, nil
}

func (s *SQLiteStore) PutScopeDeclaration(ctx context.Context, repository, task, revision string, scope Scope) error {
	canonical, err := canonicalScope(scope)
	if err != nil || !scopesEqual(scope, canonical) || repository == "" || task == "" || revision == "" {
		return errors.New("scopefence: invalid scope declaration")
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO scopefence_scopes (repository, task, graph_revision, scope_json) VALUES (?, ?, ?, ?) ON CONFLICT(repository, task, graph_revision) DO UPDATE SET scope_json = excluded.scope_json`, repository, task, revision, encoded)
	return err
}

func (f ResolvingFence) Acquire(ctx context.Context, req AcquireRequest) (Decision, error) {
	if f.Authority == nil {
		return Decision{Evidence: Evidence{Reason: ReasonGraphUntrusted}}, nil
	}
	if req.ExpectedGraphRevision == "" {
		return Decision{Evidence: Evidence{Reason: ReasonGraphInvalid}}, nil
	}
	resolved, err := f.Authority.Resolve(ctx, req.Repository, req.Task, req.ExpectedGraphRevision)
	if err != nil {
		return Decision{}, err
	}
	req.Scope = resolved
	return f.Fence.Acquire(ctx, req)
}

func (f ResolvingFence) Release(ctx context.Context, req ReleaseRequest) error {
	return f.Fence.Release(ctx, req)
}

var _ ScopeAuthority = (*SQLiteScopeAuthority)(nil)

var _ GraphAuthority = (*SQLiteGraphAuthority)(nil)
