package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StoreDirEnv overrides where transactions are persisted.
const StoreDirEnv = "HERD_INTEGRATION_DIR"

// StoreDir resolves the durable transaction directory.
func StoreDir(repoRoot string) string {
	if d := strings.TrimSpace(os.Getenv(StoreDirEnv)); d != "" {
		return d
	}
	return filepath.Join(repoRoot, ".herd", "integration")
}

// Path is the durable record for one candidate.
func Path(repoRoot, candidate string) string {
	return filepath.Join(StoreDir(repoRoot), short(candidate)+".json")
}

// Save persists a transaction after every step.
//
// FAC-710: an in-memory transaction dies with the process, which is most of
// what "transaction" is supposed to mean. A coordinator killed between merge
// and cleanup left no record of which steps had run, so the next operator could
// not tell a merged-but-uncleaned candidate from an unmerged one -- and the
// safe reading (assume nothing landed) is exactly how work gets done twice.
//
// Written whole to a temp file and renamed, so a reader never sees a partial
// record. A half-written lifecycle is worse than none: it looks authoritative.
func Save(repoRoot string, t *Transaction) error {
	if t == nil {
		return fmt.Errorf("integration: nil transaction")
	}
	dir := StoreDir(repoRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	final := Path(repoRoot, t.Candidate)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Load reads a transaction, or returns a fresh one when none exists.
//
// A missing record is a NEW transaction, not an error: the first step of a
// candidate has nothing to resume. A CORRUPT record is an error, because
// silently restarting a lifecycle that may already have merged is precisely the
// double-work this exists to prevent.
func Load(repoRoot, candidate string) (*Transaction, error) {
	raw, err := os.ReadFile(Path(repoRoot, candidate))
	if os.IsNotExist(err) {
		return New(candidate)
	}
	if err != nil {
		return nil, err
	}
	var t Transaction
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("integration %s: durable record is unreadable, refusing to restart a lifecycle "+
			"that may already have merged: %w", short(candidate), err)
	}
	if !strings.EqualFold(t.Candidate, strings.TrimSpace(candidate)) {
		return nil, fmt.Errorf("integration %s: durable record names candidate %s; refusing to drive one candidate's lifecycle from another's receipt",
			short(candidate), short(t.Candidate))
	}
	return &t, nil
}
