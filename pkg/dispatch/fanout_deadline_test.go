package dispatch

import (
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestActiveFanOutDeadlineScalesWithColumns is the FAC-522 regression.
//
// The active-column read is a fan-out, not one call. Budgeting it with a
// single per-op deadline made it expire before columns that each answered
// inside their own budget, so dispatch failed to resolve a ref that was on
// the board. Removing the ceiling instead is NOT the fix: unbounded, a stalled
// provider hangs the caller (pkg/dispatch hung to a 600s test timeout).
func TestActiveFanOutDeadlineScalesWithColumns(t *testing.T) {
	per := 30 * time.Second
	d := provider.Deadlines{List: per}

	got := activeFanOutDeadline(d)
	columns := len(provider.ActiveStatuses())
	if columns < 2 {
		t.Fatalf("fixture expects several active columns, got %d", columns)
	}
	if want := per * time.Duration(columns); got != want {
		t.Fatalf("fan-out deadline = %v, want %v (%d columns x %v)", got, want, columns, per)
	}
	// Strictly larger than one column's budget, and still finite.
	if got <= per {
		t.Fatalf("fan-out budget %v must exceed a single column budget %v", got, per)
	}
	if got == 0 {
		t.Fatal("fan-out must stay bounded; an unbounded read can hang forever")
	}
}

// TestActiveFanOutDeadlineStaysBoundedWithoutConfig keeps a zero-value
// Deadlines from collapsing the ceiling to zero.
func TestActiveFanOutDeadlineStaysBoundedWithoutConfig(t *testing.T) {
	if got := activeFanOutDeadline(provider.Deadlines{}); got <= 0 {
		t.Fatalf("unconfigured deadlines must still yield a positive bound, got %v", got)
	}
}
