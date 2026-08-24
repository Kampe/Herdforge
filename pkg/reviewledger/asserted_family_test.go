package reviewledger

import (
	"os"
	"path/filepath"
	"testing"
)

// FAC-612: an asserted family is only useful if it lands where admission reads.
// If EnsureRecord's row is not visible to ProvenBuilderFamily, the escape hatch
// stays a dead end and the candidate can still never merge.
func TestEnsureRecord_MakesFamilyProvable(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "review-ledger.jsonl")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := NewReviewLedger(root, p)
	if err != nil {
		t.Fatal(err)
	}
	sha := "1111111111111111111111111111111111111111"

	if got, _ := l.ProvenBuilderFamily(sha); got != "" {
		t.Fatalf("precondition: want unproven, got %q", got)
	}
	if err := l.EnsureRecord(RecordOpts{SHA: sha, BuilderFamily: "xai", Reviewer: "review-x", Task: "CHA-1", Gate: "operator-asserted"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.ProvenBuilderFamily(sha)
	if err != nil {
		t.Fatal(err)
	}
	if got != "xai" {
		t.Fatalf("an asserted family must become provable, got %q", got)
	}
}
