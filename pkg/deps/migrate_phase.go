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
// operator reconciliation. A missing journal directory is not pending work.
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
func PendingRollbackJournals(journalDir string) ([]string, error) {
	dir := migrateJournalDir(journalDir)
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
