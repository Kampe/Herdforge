package scopefence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const persistedEvidenceReason = "persisted_ownership"

// SQLiteStore is the durable Store implementation for a scope fence. Every
// handle points at the same SQLite file; CAS is serialized by SQLite's writer
// lock and guarded by the metadata revision, not by a process-local mutex.
type SQLiteStore struct {
	db             *sql.DB
	proofWriteHook func() error
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
		`CREATE TABLE IF NOT EXISTS scopefence_scopes (repository TEXT NOT NULL, task TEXT NOT NULL, graph_revision TEXT NOT NULL, scope_json BLOB NOT NULL, PRIMARY KEY (repository, task, graph_revision))`,
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
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: begin read: %w", err)
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM scopefence_meta WHERE id = 1`).Scan(&revision); err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: read revision: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT ownership_json, evidence_json FROM scopefence_owners ORDER BY ordinal`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: read owners: %w", err)
	}
	defer rows.Close()
	var owners []Ownership
	for rows.Next() {
		var encoded, evidenceEncoded []byte
		if err := rows.Scan(&encoded, &evidenceEncoded); err != nil {
			return Snapshot{}, fmt.Errorf("scopefence: scan owner: %w", err)
		}
		var owner Ownership
		if err := json.Unmarshal(encoded, &owner); err != nil {
			return Snapshot{}, fmt.Errorf("scopefence: decode owner: %w", err)
		}
		if err := owner.validate(); err != nil {
			return Snapshot{}, fmt.Errorf("scopefence: persisted owner invalid: %w", err)
		}
		var evidence Evidence
		if err := json.Unmarshal(evidenceEncoded, &evidence); err != nil {
			return Snapshot{}, fmt.Errorf("scopefence: decode evidence: %w", err)
		}
		if !sameEvidence(evidence, persistedEvidence(owner)) {
			return Snapshot{}, errors.New("scopefence: persisted evidence does not bind ownership")
		}
		owners = append(owners, cloneOwnership(owner))
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: read owners: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("scopefence: commit read: %w", err)
	}
	return Snapshot{Revision: fmt.Sprint(revision), Owners: owners}, nil
}

func (s *SQLiteStore) CompareAndSwap(ctx context.Context, expected string, next []Ownership) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := strictRevision(expected); err != nil {
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
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT ownership_json, evidence_json FROM scopefence_owners ORDER BY ordinal`)
	if err != nil {
		return nil, fmt.Errorf("scopefence: read evidence: %w", err)
	}
	defer rows.Close()
	var evidence []Evidence
	for rows.Next() {
		var ownerEncoded, encoded []byte
		if err := rows.Scan(&ownerEncoded, &encoded); err != nil {
			return nil, fmt.Errorf("scopefence: scan evidence: %w", err)
		}
		var item Evidence
		if err := json.Unmarshal(encoded, &item); err != nil {
			return nil, fmt.Errorf("scopefence: decode evidence: %w", err)
		}
		var owner Ownership
		if err := json.Unmarshal(ownerEncoded, &owner); err != nil || !sameEvidence(item, persistedEvidence(owner)) {
			return nil, errors.New("scopefence: persisted evidence does not bind ownership")
		}
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return evidence, nil
}

func sameEvidence(a, b Evidence) bool {
	return a.Repository == b.Repository && a.Task == b.Task && a.Branch == b.Branch && a.Generation == b.Generation && a.GraphRevision == b.GraphRevision && a.GraphFiles == b.GraphFiles && a.Reason == b.Reason && strings.Join(a.Packages, "\x00") == strings.Join(b.Packages, "\x00") && strings.Join(a.Files, "\x00") == strings.Join(b.Files, "\x00") && strings.Join(a.Symbols, "\x00") == strings.Join(b.Symbols, "\x00")
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
	digest := hex.EncodeToString(proofDigest[:])
	var existingDigest string
	var existingAuthority Authority
	var existingOwner []byte
	err = s.db.QueryRowContext(ctx, `SELECT ownership_json, authority, proof_digest FROM scopefence_release_proofs WHERE proof_key = ?`, key).Scan(&existingOwner, &existingAuthority, &existingDigest)
	if err == nil {
		if existingAuthority != req.Authority || existingDigest != digest || string(existingOwner) != string(ownerJSON) {
			return errors.New("scopefence: conflicting release proof")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO scopefence_release_proofs (proof_key, ownership_json, authority, proof_digest) VALUES (?, ?, ?, ?)`, key, ownerJSON, req.Authority, digest)
	if err != nil {
		return fmt.Errorf("scopefence: record release proof: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Release(ctx context.Context, req ReleaseRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical, err := canonicalScope(req.Scope)
	if err != nil || req.Generation <= 0 || !validState(req.State) {
		return ErrBlocked
	}
	req.Scope = canonical
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ordinal int
	var ownerEncoded []byte
	rows, err := tx.QueryContext(ctx, `SELECT ordinal, ownership_json FROM scopefence_owners ORDER BY ordinal`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var candidateOrdinal int
		var encoded []byte
		if err := rows.Scan(&candidateOrdinal, &encoded); err != nil {
			rows.Close()
			return err
		}
		var owner Ownership
		if err := json.Unmarshal(encoded, &owner); err != nil {
			rows.Close()
			return err
		}
		if owner.Identity == req.Identity && owner.Generation == req.Generation && owner.State == req.State && owner.GraphRevision == req.GraphRevision && owner.GraphFiles == req.GraphFiles && scopesEqual(owner.Scope, req.Scope) {
			ordinal, ownerEncoded, found = candidateOrdinal, encoded, true
			break
		}
	}
	rows.Close()
	if !found {
		return ErrBlocked
	}
	if s.proofWriteHook != nil {
		if err := s.proofWriteHook(); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scopefence_owners WHERE ordinal = ?`, ordinal); err != nil {
		return err
	}
	proofDigest := sha256.Sum256([]byte(req.Proof))
	key := releaseProofKey(req)
	if _, err := tx.ExecContext(ctx, `INSERT INTO scopefence_release_proofs (proof_key, ownership_json, authority, proof_digest) VALUES (?, ?, ?, ?)`, key, ownerEncoded, req.Authority, hex.EncodeToString(proofDigest[:])); err != nil {
		return fmt.Errorf("scopefence: record release proof: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scopefence_meta SET revision = revision + 1 WHERE id = 1`); err != nil {
		return err
	}
	return tx.Commit()
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
	return ReleaseProof(req)
}

func strictRevision(value string) (int64, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("scopefence: noncanonical revision")
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != value {
		return 0, errors.New("scopefence: noncanonical revision")
	}
	return revision, nil
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
