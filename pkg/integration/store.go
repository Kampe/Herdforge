package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/envelope"
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
	if err := t.Validate(); err != nil {
		return err
	}
	if t.DriverVersion != 0 {
		return fmt.Errorf("integration: executed history may only be saved by Advance")
	}
	final := Path(repoRoot, t.Candidate)
	return envelope.WithSessionFileLock(final, func() error {
		return saveLocked(repoRoot, t)
	})
}

// saveLocked requires the transaction file lock. Advance holds that same lock
// across load, durable intent, effect observation/execution, and completion.
func saveLocked(repoRoot string, t *Transaction) error {
	if err := t.Validate(); err != nil {
		return err
	}
	final := Path(repoRoot, t.Candidate)
	current, err := Load(repoRoot, t.Candidate)
	if err != nil {
		return err
	}
	if current.DriverVersion != 0 && t.DriverVersion != current.DriverVersion {
		return fmt.Errorf("integration: cannot downgrade executed history")
	}
	if current.Pending != nil {
		if t.Pending != nil {
			if *t.Pending != *current.Pending || len(t.Done) != len(current.Done) {
				return fmt.Errorf("integration: cannot replace unresolved intent")
			}
		} else if len(t.Done) != len(current.Done)+1 || t.Done[len(current.Done)].OperationID != current.Pending.ID {
			return fmt.Errorf("integration: unresolved intent requires its own completed effect")
		}
	} else if t.DriverVersion == 1 && len(t.Done) != len(current.Done) {
		return fmt.Errorf("integration: execution requires a previously durable intent")
	}
	if len(t.Done) < len(current.Done) {
		return fmt.Errorf("integration: stale writer cannot erase recorded progress")
	}
	for i, r := range current.Done {
		if t.Done[i] != r {
			return fmt.Errorf("integration: recorded evidence at step %d cannot be replaced", i)
		}
	}
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(final), ".integration-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(body, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), final); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(final))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Load reads a transaction, or returns a fresh one when none exists.
//
// A missing record is a NEW transaction, not an error: the first step of a
// candidate has nothing to resume. A CORRUPT record is an error, because
// silently restarting a lifecycle that may already have merged is precisely the
// double-work this exists to prevent.
func Load(repoRoot, candidate string) (*Transaction, error) {
	canonical, err := New(candidate)
	if err != nil {
		return nil, err
	}
	candidate = canonical.Candidate
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
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("integration: durable history is invalid; refusing to resume: %w", err)
	}
	return &t, nil
}
