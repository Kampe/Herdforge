package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

type activeCandidate struct {
	Version   int    `json:"version"`
	Candidate string `json:"candidate"`
}

func activePath(root string) string { return filepath.Join(StoreDir(root), "active.json") }

// PendingCandidate is read-only discovery of the unfinished native cycle.
// The execution boundary rechecks this result under the shared cycle lock.
// Legacy managed histories are recovered only when exactly one is unfinished;
// ambiguity or corrupt history is never treated as an empty queue.
func PendingCandidate(root string) (string, error) {
	body, err := os.ReadFile(activePath(root))
	if err == nil {
		var active activeCandidate
		if err := json.Unmarshal(body, &active); err != nil {
			return "", fmt.Errorf("integration: corrupt active candidate: %w", err)
		}
		tx, err := New(active.Candidate)
		if err != nil || len(active.Candidate) != 40 || tx.Candidate != active.Candidate || active.Version != 1 {
			return "", fmt.Errorf("integration: invalid active candidate identity")
		}
		current, err := Load(root, active.Candidate)
		if err != nil {
			return "", err
		}
		if !current.Completed(StepCleanup) {
			return active.Candidate, nil
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	entries, err := os.ReadDir(StoreDir(root))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	candidate := ""
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == filepath.Base(activePath(root)) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if len(strings.TrimSuffix(name, ".json")) != 12 || strings.Trim(strings.TrimSuffix(name, ".json"), "0123456789abcdef") != "" {
			return "", fmt.Errorf("integration: unrecognized transaction file %s", name)
		}
		raw, err := os.ReadFile(filepath.Join(StoreDir(root), name))
		if err != nil {
			return "", err
		}
		var tx Transaction
		if err := json.Unmarshal(raw, &tx); err != nil {
			return "", fmt.Errorf("integration: corrupt retained transaction %s: %w", name, err)
		}
		if err := tx.Validate(); err != nil {
			return "", err
		}
		if filepath.Base(Path(root, tx.Candidate)) != name {
			return "", fmt.Errorf("integration: retained transaction filename does not match candidate")
		}
		if tx.DriverVersion != 1 || tx.Completed(StepCleanup) {
			continue
		}
		if candidate != "" && candidate != tx.Candidate {
			return "", fmt.Errorf("integration: multiple unfinished native candidates require reconciliation")
		}
		candidate = tx.Candidate
	}
	return candidate, nil
}

// CheckActiveCandidate refuses a successor while a native cycle is unfinished.
// This read-only check also serves previews; execution calls it under the cycle lock.
func CheckActiveCandidate(root, candidate string) error {
	pending, err := PendingCandidate(root)
	if err != nil {
		return err
	}
	if pending != "" && pending != candidate {
		return fmt.Errorf("integration: candidate %s must finish proof and cleanup before %s", pending, candidate)
	}
	return nil
}

// AdvanceActive keeps one candidate selected through runtime binding, proof,
// and cleanup. The next candidate cannot install a newer runtime underneath an
// unfinished predecessor. There is still at most one lifecycle step per call.
func AdvanceActive(ctx context.Context, root, candidate string, step Step, backend Backend) (*Record, error) {
	canonical, err := New(candidate)
	if err != nil || len(strings.TrimSpace(candidate)) != 40 {
		return nil, fmt.Errorf("integration: exact active candidate required")
	}
	if backend == nil {
		return nil, fmt.Errorf("integration: native backend required")
	}
	var result *Record
	err = envelope.WithSessionFileLock(activePath(root), func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := CheckActiveCandidate(root, canonical.Candidate); err != nil {
			return err
		}
		wrapped := activeBackend{Backend: backend, root: root, candidate: canonical.Candidate}
		result, err = Advance(ctx, root, canonical.Candidate, step, wrapped)
		if err != nil {
			return err
		}
		tx, err := Load(root, canonical.Candidate)
		if err != nil {
			return err
		}
		if tx.Completed(StepCleanup) {
			if err := os.Remove(activePath(root)); err != nil && !os.IsNotExist(err) {
				return err
			}
			return syncActiveDir(root)
		}
		return nil
	})
	return result, err
}

type activeBackend struct {
	Backend
	root, candidate string
}

func (b activeBackend) Check(ctx context.Context, tx Transaction, intent Intent) error {
	if err := b.Backend.Check(ctx, tx, intent); err != nil {
		return err
	}
	// Ownership becomes durable only after admission succeeds, and before the
	// candidate intent/effect. A crash in that gap retains a resumable selection.
	return writeActive(b.root, b.candidate)
}
func writeActive(root, candidate string) error {
	body, err := json.Marshal(activeCandidate{Version: 1, Candidate: candidate})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(StoreDir(root), ".active-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(append(body, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), activePath(root)); err != nil {
		return err
	}
	return syncActiveDir(root)
}
func syncActiveDir(root string) error {
	dir, err := os.Open(StoreDir(root))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
