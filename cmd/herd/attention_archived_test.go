package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestActiveTaskResolverAcceptsEveryCanonicalStatus is the FAC-552 regression.
//
// Observed live in a peer fleet: `herdforge attention` returned
//
//	active task authority: active task resolver: unknown provider status "archived"
//
// One archived card took the whole triage command down, which is precisely the
// kind of gap that pushes a coordinator to raw herdr. Every status the provider
// layer can canonically produce must be accepted; terminal ones are skipped,
// never fatal.
func TestActiveTaskResolverAcceptsEveryCanonicalStatus(t *testing.T) {
	canonical := []string{
		provider.StatusToDo, provider.StatusInProgress, provider.StatusInReview,
		provider.StatusDone, provider.StatusPlanned, provider.StatusArchived,
	}
	for _, status := range canonical {
		if !activeResolverAcceptsStatus(status) {
			t.Fatalf("canonical status %q must not fail the active task resolver", status)
		}
	}
	// A genuinely unknown status must still fail closed rather than be treated
	// as inactive by accident.
	if activeResolverAcceptsStatus("not-a-real-status") {
		t.Fatal("an unknown status must still fail closed")
	}
	if !strings.EqualFold(provider.StatusArchived, "archived") {
		t.Fatalf("fixture assumes the archived spelling, got %q", provider.StatusArchived)
	}
}
