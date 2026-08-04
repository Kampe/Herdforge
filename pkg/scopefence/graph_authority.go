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
	if a == nil || a.store == nil || a.store.db == nil || a.repository == "" || a.expectedRevision == "" || a.expectedFiles <= 0 {
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
	if err := graph.validate(a.expectedRevision, a.expectedFiles); err != nil {
		return TrustedGraph{}, fmt.Errorf("scopefence: trusted graph snapshot rejected: %w", err)
	}
	return TrustedGraph{Snapshot: graph, ExpectedRevision: a.expectedRevision, ExpectedFiles: a.expectedFiles}, nil
}

var _ GraphAuthority = (*SQLiteGraphAuthority)(nil)
