package scopefence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

type SQLiteGraphAuthority struct {
	store            *SQLiteStore
	repository       string
	expectedRevision string
	expectedFiles    int
	Verifier         GraphScopeVerifier
}

func NewSQLiteGraphAuthority(store *SQLiteStore, repository, expectedRevision string, expectedFiles int) *SQLiteGraphAuthority {
	return &SQLiteGraphAuthority{store: store, repository: repository, expectedRevision: expectedRevision, expectedFiles: expectedFiles}
}

type GraphScopeVerifier interface {
	VerifyGraph(context.Context, AuthorityReceipt, Graph) error
	VerifyScope(context.Context, AuthorityReceipt, Scope) error
}

func (s *SQLiteStore) PutGraphSnapshot(ctx context.Context, repository string, graph Graph) error {
	if s == nil || s.db == nil || repository == "" {
		return errors.New("scopefence: graph snapshot store is not configured")
	}
	if err := graph.validate(graph.Revision, graph.Files); err != nil {
		return fmt.Errorf("scopefence: refusing incomplete graph snapshot: %w", err)
	}
	encoded, err := json.Marshal(graph)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO scopefence_graph (repository, graph_json) VALUES (?, ?) ON CONFLICT(repository) DO UPDATE SET graph_json = excluded.graph_json`, repository, encoded)
	return err
}

// ReadGraphSnapshot returns the published snapshot WITHOUT verifying it. It
// exists so a caller can bind its expected revision/file-count to exactly what
// was published; verification still happens in Current.
func (s *SQLiteStore) ReadGraphSnapshot(ctx context.Context, repository string) (Graph, error) {
	if s == nil || s.db == nil || repository == "" {
		return Graph{}, errors.New("scopefence: graph snapshot store is not configured")
	}
	var encoded []byte
	if err := s.db.QueryRowContext(ctx, `SELECT graph_json FROM scopefence_graph WHERE repository = ?`, repository).Scan(&encoded); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Graph{}, errors.New("scopefence: no published graph snapshot")
		}
		return Graph{}, err
	}
	var graph Graph
	if err := json.Unmarshal(encoded, &graph); err != nil {
		return Graph{}, err
	}
	return graph, nil
}

func (a *SQLiteGraphAuthority) Current(ctx context.Context) (TrustedGraph, error) {
	if a == nil || a.store == nil || a.store.db == nil || a.repository == "" {
		return TrustedGraph{}, errors.New("scopefence: graph authority is not configured")
	}
	if a.Verifier == nil {
		return TrustedGraph{}, errors.New("scopefence: protected graph verifier required")
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
	if a.expectedRevision == "" || a.expectedFiles <= 0 {
		return TrustedGraph{}, errors.New("scopefence: independently bound graph receipt required")
	}
	if err := graph.validate(a.expectedRevision, a.expectedFiles); err != nil {
		return TrustedGraph{}, fmt.Errorf("scopefence: trusted graph snapshot rejected: %w", err)
	}
	if err := a.Verifier.VerifyGraph(ctx, authorityReceipt("graph", a.repository, "", a.expectedRevision, a.expectedFiles, graph), graph); err != nil {
		return TrustedGraph{}, err
	}
	return TrustedGraph{Snapshot: graph, ExpectedRevision: a.expectedRevision, ExpectedFiles: a.expectedFiles}, nil
}

type ResolvingFence struct {
	Fence     Fence
	Authority ScopeAuthority
}

type SQLiteScopeAuthority struct {
	store    *SQLiteStore
	Verifier GraphScopeVerifier
}

func NewSQLiteScopeAuthority(store *SQLiteStore) *SQLiteScopeAuthority {
	return &SQLiteScopeAuthority{store: store}
}

func (a *SQLiteScopeAuthority) Resolve(ctx context.Context, repository, task, revision string) (Scope, error) {
	if a == nil || a.store == nil || repository == "" || task == "" || revision == "" {
		return Scope{}, errors.New("scopefence: scope authority is not configured")
	}
	if a.Verifier == nil {
		return Scope{}, errors.New("scopefence: protected scope verifier required")
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
	if err := a.Verifier.VerifyScope(ctx, authorityReceipt("scope", repository, task, revision, 0, canonical), canonical); err != nil {
		return Scope{}, err
	}
	return canonical, nil
}

func authorityReceipt(kind, repository, task, revision string, files int, payload any) AuthorityReceipt {
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return AuthorityReceipt{Kind: kind, Repository: repository, Task: task, Revision: revision, Files: files, PayloadDigest: hex.EncodeToString(digest[:])}
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

var _ GraphAuthority = (*SQLiteGraphAuthority)(nil)
var _ ScopeAuthority = (*SQLiteScopeAuthority)(nil)
