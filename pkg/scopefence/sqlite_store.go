package scopefence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const persistedEvidenceReason = "persisted_ownership"

// SQLiteStore is the durable Store implementation for a scope fence. Every
// handle points at the same SQLite file; CAS is serialized by SQLite's writer
// lock and guarded by the metadata revision, not by a process-local mutex.
type SQLiteStore struct {
	db *sql.DB
}

type ReleaseProofRecord struct {
	Key         string
	Ownership   Ownership
	Authority   Authority
	ProofDigest string
}

// NewSQLiteStore opens or creates a durable scope-fence database. A file path
// is required for restart and cross-process visibility; callers own Close.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("scopefence: durable sqlite path required")
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("scopefence: open sqlite store: %w", err)
	}
	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS scopefence_meta (id INTEGER PRIMARY KEY CHECK (id = 1), revision INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO scopefence_meta (id, revision) VALUES (1, 1)`,
		`CREATE TABLE IF NOT EXISTS scopefence_owners (ordinal INTEGER PRIMARY KEY, ownership_json BLOB NOT NULL, evidence_json BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS scopefence_graph (repository TEXT PRIMARY KEY, graph_json BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS scopefence_release_proofs (proof_key TEXT PRIMARY KEY, ownership_json BLOB NOT NULL, authority INTEGER NOT NULL, proof_digest TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("scopefence: migrate sqlite store: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) Read(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT revision FROM scopefence_meta WHERE id = 1`).Scan(&revision); err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: read revision: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ownership_json FROM scopefence_owners ORDER BY ordinal`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: read owners: %w", err)
	}
	defer rows.Close()
	var owners []Ownership
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return Snapshot{}, fmt.Errorf("scopefence: scan owner: %w", err)
		}
		var owner Ownership
		if err := json.Unmarshal(encoded, &owner); err != nil {
			return Snapshot{}, fmt.Errorf("scopefence: decode owner: %w", err)
		}
		owners = append(owners, cloneOwnership(owner))
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: read owners: %w", err)
	}
	return Snapshot{Revision: fmt.Sprint(revision), Owners: owners}, nil
}

func (s *SQLiteStore) CompareAndSwap(ctx context.Context, expected string, next []Ownership) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		won, err := s.compareAndSwapOnce(ctx, expected, next)
		if err == nil || !sqliteBusy(err) {
			return won, err
		}
		if err := waitSQLiteRetry(ctx, attempt); err != nil {
			return false, err
		}
	}
	return false, errors.New("scopefence: sqlite CAS contention")
}

func (s *SQLiteStore) compareAndSwapOnce(ctx context.Context, expected string, next []Ownership) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE scopefence_meta SET revision = revision + 1 WHERE id = 1 AND revision = ?`, expected)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scopefence_owners`); err != nil {
		return false, err
	}
	for ordinal, owner := range next {
		if err := owner.validate(); err != nil {
			return false, fmt.Errorf("scopefence: invalid owner in CAS: %w", err)
		}
		encoded, err := json.Marshal(cloneOwnership(owner))
		if err != nil {
			return false, fmt.Errorf("scopefence: encode owner: %w", err)
		}
		evidence, err := json.Marshal(persistedEvidence(owner))
		if err != nil {
			return false, fmt.Errorf("scopefence: encode evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO scopefence_owners (ordinal, ownership_json, evidence_json) VALUES (?, ?, ?)`, ordinal, encoded, evidence); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ReadEvidence returns the exact durable evidence associated with the current
// owner inventory, in the same deterministic order as Read.
func (s *SQLiteStore) ReadEvidence(ctx context.Context) ([]Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT evidence_json FROM scopefence_owners ORDER BY ordinal`)
	if err != nil {
		return nil, fmt.Errorf("scopefence: read evidence: %w", err)
	}
	defer rows.Close()
	var evidence []Evidence
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scopefence: scan evidence: %w", err)
		}
		var item Evidence
		if err := json.Unmarshal(encoded, &item); err != nil {
			return nil, fmt.Errorf("scopefence: decode evidence: %w", err)
		}
		evidence = append(evidence, item)
	}
	return evidence, rows.Err()
}

func persistedEvidence(owner Ownership) Evidence {
	return Evidence{Repository: owner.Repository, Task: owner.Task, Branch: owner.Branch, Generation: owner.Generation, Packages: append([]string(nil), owner.Scope.Packages...), Files: append([]string(nil), owner.Scope.Files...), Symbols: append([]string(nil), owner.Scope.Symbols...), GraphRevision: owner.GraphRevision, GraphFiles: owner.GraphFiles, Reason: persistedEvidenceReason}
}

func (s *SQLiteStore) RecordReleaseProof(ctx context.Context, req ReleaseRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical, err := canonicalScope(req.Scope)
	if err != nil || req.Generation <= 0 || !validState(req.State) {
		return errors.New("scopefence: invalid release proof")
	}
	req.Scope = canonical
	ownerJSON, err := json.Marshal(req.Ownership)
	if err != nil {
		return err
	}
	proofDigest := sha256.Sum256([]byte(req.Proof))
	key := releaseProofKey(req)
	_, err = s.db.ExecContext(ctx, `INSERT INTO scopefence_release_proofs (proof_key, ownership_json, authority, proof_digest) VALUES (?, ?, ?, ?) ON CONFLICT(proof_key) DO UPDATE SET ownership_json = excluded.ownership_json, authority = excluded.authority, proof_digest = excluded.proof_digest`, key, ownerJSON, req.Authority, hex.EncodeToString(proofDigest[:]))
	if err != nil {
		return fmt.Errorf("scopefence: record release proof: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ReadReleaseProof(ctx context.Context, req ReleaseRequest) (ReleaseProofRecord, error) {
	var encoded []byte
	var authority Authority
	var digest string
	key := releaseProofKey(req)
	if err := s.db.QueryRowContext(ctx, `SELECT ownership_json, authority, proof_digest FROM scopefence_release_proofs WHERE proof_key = ?`, key).Scan(&encoded, &authority, &digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReleaseProofRecord{}, errors.New("scopefence: release proof not found")
		}
		return ReleaseProofRecord{}, err
	}
	var ownership Ownership
	if err := json.Unmarshal(encoded, &ownership); err != nil {
		return ReleaseProofRecord{}, fmt.Errorf("scopefence: decode release proof: %w", err)
	}
	return ReleaseProofRecord{Key: key, Ownership: ownership, Authority: authority, ProofDigest: digest}, nil
}

// ReleaseProofKey is stable for the exact fenced release tuple and excludes
// the proof secret; the stored digest still binds the authenticated proof.
func ReleaseProofKey(req ReleaseRequest) string { return releaseProofKey(req) }

func releaseProofKey(req ReleaseRequest) string {
	canonical, _ := canonicalScope(req.Scope)
	value := struct {
		Identity
		Generation    int64
		Scope         Scope
		State         State
		GraphRevision string
		GraphFiles    int
		Authority     Authority
	}{req.Identity, req.Generation, canonical, req.State, req.GraphRevision, req.GraphFiles, req.Authority}
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func sqliteBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "sqlite_busy") || strings.Contains(message, "database table is locked")
}

func waitSQLiteRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * 20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ Store = (*SQLiteStore)(nil)
var _ ReleaseProofStore = (*SQLiteStore)(nil)
