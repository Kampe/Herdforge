package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// TestDrainPartialIsLabelledPartial is the FAC-555 regression. The command
// previously exited with only "bounded scan exceeded 2m0s: context deadline
// exceeded" -- no counts, no identities, no remaining figure -- so an operator
// could not tell an empty queue from a scan that never got there.
func TestDrainPartialIsLabelledPartial(t *testing.T) {
	result := &harvest.HarvestResult{
		UnmergedWorktrees: []harvest.UnmergedWork{
			{Branch: "herd/cha-2199"},
			{Branch: "herd/cha-2150"},
		},
		Errors: []string{"one input unreadable"},
	}
	var out, errOut bytes.Buffer
	emitDrainPartial(&out, &errOut, result, false)
	text := errOut.String()
	for _, want := range []string{"PARTIAL", "failed_phase=review-scan", "harvest_scanned=2", "herd/cha-2150"} {
		if !strings.Contains(text, want) {
			t.Fatalf("partial output must contain %q; got:\n%s", want, text)
		}
	}
	// It must never read as a completed drain decision.
	if !strings.Contains(text, "must not") {
		t.Fatalf("partial must disclaim being a drain decision; got:\n%s", text)
	}
}

func TestDrainPartialJSONCarriesTheFlagAndCounts(t *testing.T) {
	result := &harvest.HarvestResult{
		UnmergedWorktrees: []harvest.UnmergedWork{{Branch: "b"}},
	}
	var out, errOut bytes.Buffer
	emitDrainPartial(&out, &errOut, result, true)

	var got DrainPartial
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("partial JSON must parse: %v (%s)", err, out.String())
	}
	if !got.Partial || got.Phase != "review-scan" || got.Scanned != 1 {
		t.Fatalf("partial JSON fields wrong: %+v", got)
	}
	if got.Note == "" {
		t.Fatal("partial JSON must carry the disclaiming note")
	}
}

// A nil result must say so rather than emit a zero-count report that reads
// like a clean board.
func TestDrainPartialNilResultSaysNone(t *testing.T) {
	var out, errOut bytes.Buffer
	emitDrainPartial(&out, &errOut, nil, false)
	if !strings.Contains(errOut.String(), "partial=none") {
		t.Fatalf("nil result must report none; got %q", errOut.String())
	}
}
