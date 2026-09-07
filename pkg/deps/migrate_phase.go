package deps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

const (
	DefaultMigrateJournalDir = ".herd/migrate-journal"
	migrateJournalRootsFile  = "roots.jsonl"

	JournalStatusApplied         = "applied"
	JournalStatusRolledBack      = "rolled_back"
	JournalStatusRollbackPending = "rollback_pending"
)

type rollbackBudgetKey struct{}

// WithMigrationRollbackBudget pins the independent rollback phase budget for
// tests. Production keeps DefaultMigrationRollbackBudget.
func WithMigrationRollbackBudget(ctx context.Context, budget time.Duration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget <= 0 {
		return ctx
	}
	return context.WithValue(ctx, rollbackBudgetKey{}, budget)
}

func migrationRollbackBudget(ctx context.Context) time.Duration {
	if ctx != nil {
		if budget, ok := ctx.Value(rollbackBudgetKey{}).(time.Duration); ok && budget > 0 {
			return budget
		}
	}
	return DefaultMigrationRollbackBudget
}

// RollbackPendingError is returned when a description write landed and rollback
// itself could not complete. Dispatch and approval must refuse until reconciled.
type RollbackPendingError struct {
	Ref         string
	JournalPath string
	Cause       error
}

func (e *RollbackPendingError) Error() string {
	if e == nil {
		return "deps migrate rollback_pending"
	}
	return fmt.Sprintf("deps migrate rollback_pending ref=%s journal=%s: %v", e.Ref, e.JournalPath, e.Cause)
}

func (e *RollbackPendingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func migrateJournalDir(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if env := strings.TrimSpace(os.Getenv("HERD_MIGRATE_JOURNAL")); env != "" {
		return env
	}
	return DefaultMigrateJournalDir
}

func rememberMigrateJournalRoot(journalDir string) error {
	journalDir = filepath.Clean(strings.TrimSpace(journalDir))
	if journalDir == "" || journalDir == "." {
		return nil
	}
	canonical := migrateJournalDir("")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		return err
	}
	path := filepath.Join(canonical, migrateJournalRootsFile)
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	want := journalDir + "\n"
	for _, line := range strings.Split(string(existing), "\n") {
		if filepath.Clean(strings.TrimSpace(line)) == journalDir {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(want)
	return err
}

func knownMigrateJournalRoots(explicit string) []string {
	if dir := strings.TrimSpace(explicit); dir != "" {
		return []string{filepath.Clean(dir)}
	}
	seen := map[string]struct{}{}
	var roots []string
	add := func(dir string) {
		dir = filepath.Clean(strings.TrimSpace(dir))
		if dir == "" || dir == "." {
			return
		}
		if _, ok := seen[dir]; ok {
			return
		}
		seen[dir] = struct{}{}
		roots = append(roots, dir)
	}
	add(migrateJournalDir(""))
	add(DefaultMigrateJournalDir)
	for _, base := range []string{migrateJournalDir(""), DefaultMigrateJournalDir} {
		raw, err := os.ReadFile(filepath.Join(base, migrateJournalRootsFile))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			add(line)
		}
	}
	return roots
}

type migrationPhases struct {
	planning context.Context
	mutation context.Context
	readback context.Context
	rollback context.Context
	cancel   func()
}

func deriveMigrationPhases(op context.Context) migrationPhases {
	if op == nil {
		op = context.Background()
	}
	planning, cancelPlanning := context.WithCancel(op)
	mutation, cancelMutation := context.WithCancel(op)
	readback, cancelReadback := context.WithCancel(op)
	rollback, cancelRollback := context.WithTimeout(context.WithoutCancel(op), migrationRollbackBudget(op))
	return migrationPhases{
		planning: planning,
		mutation: mutation,
		readback: readback,
		rollback: rollback,
		cancel: func() {
			cancelPlanning()
			cancelMutation()
			cancelReadback()
			cancelRollback()
		},
	}
}

// RefusePendingRollback fails closed when a migrate journal is waiting for
// operator reconciliation. An empty journalDir scans every root apply can
// write (default, HERD_MIGRATE_JOURNAL, and remembered --journal dirs).
// A missing journal directory is not pending work.
func RefusePendingRollback(journalDir string) error {
	pending, err := PendingRollbackJournals(journalDir)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	return &RollbackPendingError{JournalPath: pending[0], Ref: "pending", Cause: errors.New("reconcile migrate journal before dispatch or approval")}
}

// PendingRollbackJournals lists apply journals marked rollback_pending.
// An explicit directory is scanned alone so a default-dir-only check can
// prove it would miss a non-default --journal. An empty directory scans
// every remembered apply root.
func PendingRollbackJournals(journalDir string) ([]string, error) {
	var pending []string
	for _, dir := range knownMigrateJournalRoots(journalDir) {
		found, err := pendingRollbackJournalsInDir(dir)
		if err != nil {
			return nil, err
		}
		pending = append(pending, found...)
	}
	return pending, nil
}

func pendingRollbackJournalsInDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var pending []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "apply-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		var journal Journal
		if unmarshalErr := json.Unmarshal(raw, &journal); unmarshalErr != nil {
			continue
		}
		if journal.Status == JournalStatusRollbackPending {
			pending = append(pending, path)
		}
	}
	return pending, nil
}

func readbackUnknown(err error) bool {
	return err != nil && provider.IsTimeout(err)
}
