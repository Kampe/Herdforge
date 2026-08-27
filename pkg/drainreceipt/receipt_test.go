package drainreceipt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInterruptedDrainLeavesTimeoutReceiptWithCursor(t *testing.T) {
	root := t.TempDir()
	if _, err := Begin(root, "50ms", "harvest-scan"); err != nil {
		t.Fatal(err)
	}
	if err := MarkTimeout(root, "review-scan", "abc123deadbeef", 12, 1742); err != nil {
		t.Fatal(err)
	}
	r, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusTimeout {
		t.Fatalf("status=%q want timeout", r.Status)
	}
	if r.ResumeCursor != "abc123deadbeef" {
		t.Fatalf("cursor=%q", r.ResumeCursor)
	}
	if r.Bound != "50ms" {
		t.Fatalf("bound=%q", r.Bound)
	}
	if r.FinishedAt == "" {
		t.Fatal("timeout must set finished_at")
	}
	if !strings.Contains(strings.ToLower(r.Note), "stale") && !strings.Contains(r.Note, "bounded") {
		t.Fatalf("timeout note must explain degradation: %q", r.Note)
	}
}

func TestCompletedDrainDistinguishableFromNeverRan(t *testing.T) {
	root := t.TempDir()
	if _, err := Load(root); !os.IsNotExist(err) {
		t.Fatalf("never-ran must be missing receipt, got %v", err)
	}
	if _, err := Begin(root, "2m0s", "harvest-scan"); err != nil {
		t.Fatal(err)
	}
	if err := MarkCompleted(root, 10, 10); err != nil {
		t.Fatal(err)
	}
	r, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusCompleted {
		t.Fatalf("status=%q want completed", r.Status)
	}
	if r.ResumeCursor != "" {
		t.Fatalf("completed drain clears resume cursor, got %q", r.ResumeCursor)
	}
	if _, err := os.Stat(filepath.Join(root, RelPath)); err != nil {
		t.Fatal(err)
	}
}

// Revert-style proof: without MarkTimeout, a Begin-only receipt stays running —
// that is the vanished-process shape FAC-605 forbids as a terminal answer.
func TestRunningIsNotTerminal(t *testing.T) {
	root := t.TempDir()
	if _, err := Begin(root, "2m", "harvest-scan"); err != nil {
		t.Fatal(err)
	}
	r, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusRunning {
		t.Fatalf("status=%q", r.Status)
	}
	if r.FinishedAt != "" {
		t.Fatal("running receipt must not set finished_at")
	}
}

func TestPriorResumeCursorOnlyFromTimeout(t *testing.T) {
	root := t.TempDir()
	if _, ok := PriorResumeCursor(root); ok {
		t.Fatal("never-ran must not offer a resume cursor")
	}
	if _, err := Begin(root, "1m", "harvest-scan"); err != nil {
		t.Fatal(err)
	}
	if _, ok := PriorResumeCursor(root); ok {
		t.Fatal("fresh running receipt must not offer a resume cursor")
	}
	if err := MarkTimeout(root, "review-scan", "deadbeef", 3, 10); err != nil {
		t.Fatal(err)
	}
	c, ok := PriorResumeCursor(root)
	if !ok || c != "deadbeef" {
		t.Fatalf("timeout cursor: ok=%v cursor=%q", ok, c)
	}
	if err := MarkCompleted(root, 10, 10); err != nil {
		t.Fatal(err)
	}
	if _, ok := PriorResumeCursor(root); ok {
		t.Fatal("completed drain clears resume")
	}
}

// FAC-605 residual: a process killed mid-scan freezes the receipt at
// status=running. Hot-loop Progress must have left a cursor, and a stale
// running receipt must resume — not collapse into "never ran".
func TestHardKillLeavesAbandonedRunningCursorUsable(t *testing.T) {
	root := t.TempDir()
	if _, err := Begin(root, "2m0s", "harvest-scan"); err != nil {
		t.Fatal(err)
	}
	// Simulate the one phase-boundary Progress (empty cursor) then hot-loop
	// Progress with a real tip — then process death before MarkTimeout.
	if err := Progress(root, "review-scan", "", 0, 100, 10); err != nil {
		t.Fatal(err)
	}
	if err := Progress(root, "review-scan", "tip-sha-halfway", 50, 100, 10); err != nil {
		t.Fatal(err)
	}
	r, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusRunning {
		t.Fatalf("status=%q want running (process died)", r.Status)
	}
	if r.ResumeCursor != "tip-sha-halfway" {
		t.Fatalf("hot-loop cursor missing: %q", r.ResumeCursor)
	}
	// Fresh running must NOT resume (presence is not "take over a live drain").
	if _, ok := PriorResumeCursor(root); ok {
		t.Fatal("fresh running receipt must not be claimed as abandoned")
	}
	// Age the receipt past the bound: abandoned.
	r.UpdatedAt = time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339Nano)
	if err := write(root, r); err != nil {
		t.Fatal(err)
	}
	if !Abandoned(r, time.Now().UTC()) {
		t.Fatal("expected Abandoned after stale UpdatedAt")
	}
	c, ok := PriorResumeCursor(root)
	if !ok || c != "tip-sha-halfway" {
		t.Fatalf("abandoned running must yield cursor: ok=%v cursor=%q", ok, c)
	}
}

// Revert proof target: without hot-loop Progress, Begin+phase Progress leaves
// an empty cursor and PriorResumeCursor stays false even when abandoned.
func TestPhaseBoundaryProgressAloneLeavesNoUsableCursor(t *testing.T) {
	root := t.TempDir()
	if _, err := Begin(root, "30s", "harvest-scan"); err != nil {
		t.Fatal(err)
	}
	if err := Progress(root, "review-scan", "", 0, 0, 5); err != nil {
		t.Fatal(err)
	}
	r, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	r.UpdatedAt = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	if err := write(root, r); err != nil {
		t.Fatal(err)
	}
	if _, ok := PriorResumeCursor(root); ok {
		t.Fatal("empty cursor must not resume even when abandoned")
	}
}
