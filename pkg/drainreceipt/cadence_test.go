package drainreceipt

import (
	"testing"
	"time"
)

func TestProgressCadenceBoundsWrites(t *testing.T) {
	c := NewProgressCadence(2 * time.Second)
	t0 := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if !c.ShouldWrite(t0, 0) {
		t.Fatal("first tip must write")
	}
	c.MarkWritten(t0)
	if c.ShouldWrite(t0.Add(500*time.Millisecond), 1) {
		t.Fatal("within interval must not write")
	}
	if !c.ShouldWrite(t0.Add(2*time.Second), 10) {
		t.Fatal("at interval must write")
	}
	c.MarkWritten(t0.Add(2 * time.Second))
	if c.ShouldWrite(t0.Add(3*time.Second), 11) {
		t.Fatal("1s later must not write")
	}
}

// Revert-style: a cadence of always-write (Every tiny / nil) is what the FAIL
// rejected. ShouldWrite with Every=1ns writes every tip — that is the defect
// we bound away from; callers must use DefaultProgressInterval.
func TestDefaultProgressIntervalIsTwoSeconds(t *testing.T) {
	if DefaultProgressInterval != 2*time.Second {
		t.Fatalf("DefaultProgressInterval=%s want 2s", DefaultProgressInterval)
	}
	c := NewProgressCadence(0)
	if c.Every != DefaultProgressInterval {
		t.Fatalf("Every=%s", c.Every)
	}
}
