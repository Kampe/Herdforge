package drainreceipt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		t.Fatal("running receipt must not offer a resume cursor")
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
