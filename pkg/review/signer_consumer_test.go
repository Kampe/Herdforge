package review

import (
	"strings"
	"testing"
)

func TestSignerConsumer_RequiresTopology(t *testing.T) {
	// Unprovisioned host: Open must fail closed (not soft-open).
	_, err := NewSignerConsumer(t.TempDir(), t.TempDir(), "id")
	if err == nil {
		t.Fatal("expected fail-closed without topology")
	}
	if !strings.Contains(err.Error(), "BLOCKED") &&
		!strings.Contains(err.Error(), "boundary") &&
		!strings.Contains(err.Error(), "topology") &&
		!strings.Contains(err.Error(), "UID") {
		t.Fatalf("unexpected error: %v", err)
	}
}
