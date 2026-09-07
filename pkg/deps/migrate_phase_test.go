package deps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

type phaseScopedDescriptionWriter struct {
	*recordingDescriptionWriter
	mu              sync.Mutex
	sets            int
	gets            int
	blockReadback   bool
	blockRollback   bool
	rollbackCtxLive bool
	rollbackSeen    bool
}

func (w *phaseScopedDescriptionWriter) SetDescription(ctx context.Context, taskID, description string) error {
	w.mu.Lock()
	w.sets++
	n := w.sets
	block := w.blockRollback && n > 1
	w.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return w.recordingDescriptionWriter.SetDescription(ctx, taskID, description)
}

func (w *phaseScopedDescriptionWriter) GetDescription(ctx context.Context, taskID string) (string, error) {
	w.mu.Lock()
	w.gets++
	n := w.gets
	blockReadback := w.blockReadback && n == 2
	w.mu.Unlock()
	if blockReadback {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if n > 2 {
		w.mu.Lock()
		w.rollbackSeen = true
		w.rollbackCtxLive = ctx.Err() == nil
		w.mu.Unlock()
	}
	return w.recordingDescriptionWriter.GetDescription(ctx, taskID)
}

func isolateMigrateJournal(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("HERD_MIGRATE_JOURNAL", "")
}

func TestApplyMigrationReadbackTimeoutUsesIndependentRollbackContext(t *testing.T) {
	isolateMigrateJournal(t)
	tp := newExactMigrationProvider()
	const before = "before readback timeout"
	tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-765", Status: provider.StatusInProgress, ProjectID: "p", Description: before})
	store := NewProviderStore(tp, "p")
	base := &recordingDescriptionWriter{mp: tp.MemoryProvider}
	writer := &phaseScopedDescriptionWriter{recordingDescriptionWriter: base, blockReadback: true}

	ctx, cancel := WithMigrationRequestBudget(context.Background(), 80*time.Millisecond)
	defer cancel()
	plan, err := ApplyMigrationForRef(ctx, store, tp, "p", "FAC-765", writer, t.TempDir())
	if err != nil {
		t.Fatalf("independent rollback returned error: %v", err)
	}
	if plan == nil || plan.OK || len(plan.Items) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	item := plan.Items[0]
	if !strings.Contains(item.Detail, "UNKNOWN") {
		t.Fatalf("readback timeout detail = %q, want UNKNOWN", item.Detail)
	}
	if item.ReadbackOK || !item.RolledBack {
		t.Fatalf("item = %+v, want rolled back without fence", item)
	}
	if !writer.rollbackSeen || !writer.rollbackCtxLive {
		t.Fatalf("rollback ctx live=%v seen=%v, want a live independent rollback context", writer.rollbackCtxLive, writer.rollbackSeen)
	}
	task, getErr := tp.GetTask(context.Background(), "target-id")
	if getErr != nil || task.Description != before {
		t.Fatalf("description after independent rollback = %q err=%v", task.Description, getErr)
	}
}

func TestApplyMigrationRollbackTimeoutLeavesRollbackPending(t *testing.T) {
	isolateMigrateJournal(t)
	tp := newExactMigrationProvider()
	const before = "before rollback timeout"
	tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-765", Status: provider.StatusInProgress, ProjectID: "p", Description: before})
	store := NewProviderStore(tp, "p")
	base := &recordingDescriptionWriter{mp: tp.MemoryProvider}
	writer := &phaseScopedDescriptionWriter{recordingDescriptionWriter: base, blockReadback: true, blockRollback: true}
	journalDir := t.TempDir()

	ctx := WithMigrationRollbackBudget(context.Background(), 40*time.Millisecond)
	ctx, cancel := WithMigrationRequestBudget(ctx, 80*time.Millisecond)
	defer cancel()
	plan, err := ApplyMigrationForRef(ctx, store, tp, "p", "FAC-765", writer, journalDir)
	var pending *RollbackPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want rollback_pending", err)
	}
	if plan == nil || plan.JournalPath == "" || pending.JournalPath != plan.JournalPath {
		t.Fatalf("journal path plan=%v pending=%v", plan, pending)
	}
	if plan.OK || len(plan.Items) != 1 || plan.Items[0].ReadbackOK || plan.Items[0].RolledBack {
		t.Fatalf("pending item claimed success or fence: %+v", plan.Items)
	}
	raw, readErr := os.ReadFile(plan.JournalPath)
	if readErr != nil {
		t.Fatalf("read journal: %v", readErr)
	}
	var journal Journal
	if unmarshalErr := json.Unmarshal(raw, &journal); unmarshalErr != nil {
		t.Fatalf("decode journal: %v", unmarshalErr)
	}
	if journal.Status != JournalStatusRollbackPending {
		t.Fatalf("journal status = %q, want %s", journal.Status, JournalStatusRollbackPending)
	}
	if len(journal.Entries) != 1 || journal.Entries[0].AfterDesc == "" {
		t.Fatalf("journal lost after-image: %+v", journal.Entries)
	}
	if err := RefusePendingRollback(journalDir); err == nil {
		t.Fatal("dispatch/approval must refuse while rollback_pending")
	}
	task, getErr := tp.GetTask(context.Background(), "target-id")
	if getErr != nil || task.Description == before {
		t.Fatalf("unreconciled description = %q err=%v; fence must not be claimed and before-image is unrestored", task.Description, getErr)
	}
}

// TestNonDefaultJournalPendingRollbackBlocksLaunch proves the W4 FAIL on
// 5a3e0ea3: ApplyMigrationForRef(--journal tmp) can leave rollback_pending
// where a default-dir-only scan never looks, and production ValidateLaunch /
// RefusePendingRollback("") must still refuse.
func TestNonDefaultJournalPendingRollbackBlocksLaunch(t *testing.T) {
	isolateMigrateJournal(t)

	tp := newExactMigrationProvider()
	const before = "before non-default journal timeout"
	tp.AddTask(&provider.Task{ID: "target-id", Ref: "FAC-765", Status: provider.StatusInProgress, ProjectID: "p", Description: before})
	store := NewProviderStore(tp, "p")
	base := &recordingDescriptionWriter{mp: tp.MemoryProvider}
	writer := &phaseScopedDescriptionWriter{recordingDescriptionWriter: base, blockReadback: true, blockRollback: true}
	journalDir := t.TempDir()

	ctx := WithMigrationRollbackBudget(context.Background(), 40*time.Millisecond)
	ctx, cancel := WithMigrationRequestBudget(ctx, 80*time.Millisecond)
	defer cancel()
	plan, err := ApplyMigrationForRef(ctx, store, tp, "p", "FAC-765", writer, journalDir)
	var pending *RollbackPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want rollback_pending", err)
	}
	if plan == nil || pending.JournalPath == "" || !strings.HasPrefix(pending.JournalPath, journalDir) {
		t.Fatalf("pending journal %q is not under --journal tmp %q", pending.JournalPath, journalDir)
	}

	defaultPending, scanErr := PendingRollbackJournals(DefaultMigrateJournalDir)
	if scanErr != nil {
		t.Fatalf("default-dir scan: %v", scanErr)
	}
	if len(defaultPending) != 0 {
		t.Fatalf("default-dir-only scan found %v; that is the fail-open the production gate had", defaultPending)
	}

	if err := RefusePendingRollback(""); err == nil {
		t.Fatal("RefusePendingRollback(\"\") must refuse the tmp journal apply actually used")
	}
	_, launchErr := ValidateLaunch(context.Background(), NewMemoryStore(), EntryDispatch, "FAC-765", nil, "")
	var blocked *BlockedError
	if !errors.As(launchErr, &blocked) || blocked.Code != "rollback_pending" {
		t.Fatalf("ValidateLaunch err = %v, want rollback_pending", launchErr)
	}
}
