package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/quotasup"
)

func stamp(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

// FAC-593: capacity planning read the quota snapshot with no age check while
// every other consumer honours DefaultMaxObservationAge. A four-day-old
// snapshot on this fleet still asserted grok exhausted at 100% and claude
// blocked, when live grok was 1% used and codex 0% — so lanes piled onto claude
// until it hit 74% while two full pools sat idle.
func TestQuotaSnapshotFreshRejectsStaleReading(t *testing.T) {
	s := quotasup.Snapshot{ObservedAt: stamp(4 * 24 * time.Hour)}
	err := quotaSnapshotFresh(s, quotasup.DefaultMaxObservationAge)
	if err == nil {
		t.Fatal("a four-day-old snapshot must not be used to plan capacity")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error should name the staleness: %v", err)
	}
	// The operator needs the remedy, not just the complaint.
	if !strings.Contains(err.Error(), "quota-supervisor") {
		t.Errorf("error should name the refresh command: %v", err)
	}
}

func TestQuotaSnapshotFreshAcceptsCurrentReading(t *testing.T) {
	s := quotasup.Snapshot{ObservedAt: stamp(30 * time.Second)}
	if err := quotaSnapshotFresh(s, quotasup.DefaultMaxObservationAge); err != nil {
		t.Fatalf("a 30s-old snapshot must be usable: %v", err)
	}
}

// An undateable snapshot cannot prove it is current. Treating a missing
// timestamp as "now" is exactly how a stale reading keeps being trusted, so it
// must fail closed.
func TestQuotaSnapshotFreshRejectsUndateable(t *testing.T) {
	for name, s := range map[string]quotasup.Snapshot{
		"no timestamps":  {},
		"unparseable":    {ObservedAt: "yesterday-ish"},
		"empty observed": {ObservedAt: ""},
	} {
		if err := quotaSnapshotFresh(s, quotasup.DefaultMaxObservationAge); err == nil {
			t.Errorf("%s: must fail closed, got nil", name)
		}
	}
}

// source_at is the documented fallback when observed_at is absent, so a
// snapshot carrying only source_at must still be judged on its real age.
func TestQuotaSnapshotFreshFallsBackToSourceAt(t *testing.T) {
	fresh := quotasup.Snapshot{SourceAt: stamp(10 * time.Second)}
	if err := quotaSnapshotFresh(fresh, quotasup.DefaultMaxObservationAge); err != nil {
		t.Errorf("fresh source_at must be accepted: %v", err)
	}
	old := quotasup.Snapshot{SourceAt: stamp(48 * time.Hour)}
	if err := quotaSnapshotFresh(old, quotasup.DefaultMaxObservationAge); err == nil {
		t.Error("stale source_at must be refused")
	}
}

// A zero or negative maxAge must fall back to the package default rather than
// disabling the gate, or a caller could accidentally re-open the hole.
func TestQuotaSnapshotFreshZeroMaxAgeUsesDefault(t *testing.T) {
	s := quotasup.Snapshot{ObservedAt: stamp(4 * 24 * time.Hour)}
	if err := quotaSnapshotFresh(s, 0); err == nil {
		t.Error("maxAge=0 must use the default gate, not disable it")
	}
}
