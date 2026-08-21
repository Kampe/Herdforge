package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPartitionSeparatesInheritedFromNew(t *testing.T) {
	baseline := &AuditBaseline{TaskIDs: []string{"old-1", "old-2"}}
	findings := []AuditFinding{
		{Kind: AuditNoEvidence, Ref: "FAC-1", TaskID: "old-1"},
		{Kind: AuditCommitHintOnly, Ref: "FAC-2", TaskID: "new-1"},
		{Kind: AuditOverride, Ref: "FAC-3", TaskID: "ovr-1"},
	}
	newV, hist := PartitionFindings(findings, baseline)
	if len(newV) != 1 || newV[0].Ref != "FAC-2" {
		t.Fatalf("only the un-baselined finding is a violation, got %+v", newV)
	}
	// An override is attributable by construction and is never a violation.
	if len(hist) != 2 {
		t.Fatalf("want inherited + override as historical, got %+v", hist)
	}
}

// With no baseline every finding is new, so a fresh repository cannot silently
// inherit a clean bill of health.
func TestPartitionWithoutBaselineCountsEverythingNew(t *testing.T) {
	findings := []AuditFinding{
		{Kind: AuditNoEvidence, TaskID: "a"},
		{Kind: AuditCommitHintOnly, TaskID: "b"},
	}
	newV, hist := PartitionFindings(findings, nil)
	if len(newV) != 2 || len(hist) != 0 {
		t.Fatalf("no baseline must mean all new; got new=%d hist=%d", len(newV), len(hist))
	}
}

func TestBaselineRoundTripAndAttribution(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteAuditBaseline(dir, "", []string{"a"}); err == nil {
		t.Fatal("an unattributed baseline must be refused")
	}
	if _, err := WriteAuditBaseline(dir, "coordinator", nil); err == nil {
		t.Fatal("an empty baseline must be refused")
	}
	written, err := WriteAuditBaseline(dir, "coordinator", []string{"b", "a", "a", " "})
	if err != nil {
		t.Fatal(err)
	}
	if len(written.TaskIDs) != 2 || written.TaskIDs[0] != "a" {
		t.Fatalf("want deduped, sorted ids, got %v", written.TaskIDs)
	}
	if written.Actor != "coordinator" || written.CapturedAt == "" {
		t.Fatalf("attribution must be recorded, got %+v", written)
	}
	// The note must state plainly that this is not completion evidence.
	if written.Note == "" || !contains(written.Note, "NOT evidence") {
		t.Fatalf("baseline must disclaim being a receipt, got %q", written.Note)
	}

	back, err := ReadAuditBaseline(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Has("a") || back.Has("zzz") {
		t.Fatalf("round-trip membership wrong: %+v", back)
	}
}

// A missing baseline is not an error; a corrupt one must fail closed rather
// than degrade to "everything is historical", which would hide every bypass.
func TestReadBaselineMissingVsCorrupt(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadAuditBaseline(dir)
	if err != nil || got != nil {
		t.Fatalf("missing baseline must be (nil, nil), got %+v %v", got, err)
	}
	path := filepath.Join(dir, BaselineFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAuditBaseline(dir); err == nil {
		t.Fatal("a corrupt baseline must fail closed")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
