package reviewledger

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultPathIsOneDefinition is the FAC-573 gate for the ledger path.
//
// Six callers rebuilt this from segments, split between a cwd-relative form and
// a root-prefixed one. The ledger is the record of what was reviewed and
// admitted, and a ledger that resolves differently per caller is worse than a
// mailbox that does: a missing row reads as "never reviewed".
func TestDefaultPathIsOneDefinition(t *testing.T) {
	if got, want := DefaultPath(""), filepath.Join(".herd", Leaf); got != want {
		t.Errorf("DefaultPath(\"\") = %q, want %q", got, want)
	}
	if got, want := DefaultPath("/tmp/repo"), filepath.Join("/tmp/repo", ".herd", Leaf); got != want {
		t.Errorf("DefaultPath(root) = %q, want %q", got, want)
	}
	if got := DefaultPath("  "); got != DefaultPath("") {
		t.Errorf("a blank root must behave as empty, got %q", got)
	}
	// The leaf must stay distinct from the candidate index's own ledger, which
	// lives under .herd/review/. Conflating them is what the rename prevents.
	if strings.Contains(DefaultPath(""), filepath.Join(".herd", "review", "ledger.jsonl")) {
		t.Error("the review ledger must not be confused with the candidate index ledger")
	}
}
