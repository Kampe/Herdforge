package main

import (
	"io"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/drainreceipt"
	"github.com/Kampe/Herdforge/pkg/review"
)

// FAC-605: the cadenced drainreceipt.Progress call inside the Progress closure
// must be exercised. Deleting that call (the independent FAIL mutation) must
// turn this test red on the cursor assertion — not merely fail to compile.
func TestAttachDrainReceiptProgressWritesCursorThroughHotLoopSeam(t *testing.T) {
	root := t.TempDir()
	if _, err := drainreceipt.Begin(root, "2m", "review-scan"); err != nil {
		t.Fatal(err)
	}
	d := &review.Drain{}
	attachDrainReceiptProgress(d, root, true, io.Discard, time.Now(), time.Nanosecond)
	if d.Progress == nil {
		t.Fatal("Progress not attached")
	}
	// Simulate Drain.Scan's per-tip callback (pipeline.go calls Progress before probe).
	d.Progress(0, 3, "a", "sha-tip-0")
	d.Progress(1, 3, "b", "sha-tip-1")
	d.Progress(2, 3, "c", "sha-tip-2")
	r, err := drainreceipt.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.ResumeCursor != "sha-tip-2" {
		t.Fatalf("resume_cursor=%q want sha-tip-2 (cadenced Progress must write from the closure)", r.ResumeCursor)
	}
	if r.Status != drainreceipt.StatusRunning {
		t.Fatalf("status=%q", r.Status)
	}
}
