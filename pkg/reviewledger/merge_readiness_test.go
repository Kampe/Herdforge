package reviewledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ledgerWith(t *testing.T, lines ...string) *Ledger {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "review-ledger.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := NewReadOnlyReviewLedger(root, p)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The exact mistake this exists to prevent: rows are PRESENT, so a row count
// says "reviewed", while the verdict says FAIL. Eight candidates were reported
// merge-ready this way.
func TestMergeReadiness_RowsPresentButVerdictIsFail(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"record","sha":"aaa","reviewer":"r1"}`,
		`{"event":"verdict","sha":"aaa","reviewer":"r1","verdict":"FAIL"}`)
	got, err := l.MergeReadinessFor("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ready {
		t.Fatalf("a FAIL verdict must never be merge-ready: %+v", got)
	}
	if got.Failures != 1 {
		t.Fatalf("failures=%d, want 1", got.Failures)
	}
}

// #3203: FAIL from one reviewer, PASS from another, same timestamp. VerdictFor
// returns last-wins and would report PASS, discarding the dissent.
func TestMergeReadiness_SplitDecisionIsNotAPass(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"verdict","sha":"bbb","reviewer":"pool-04","verdict":"FAIL"}`,
		`{"event":"verdict","sha":"bbb","reviewer":"pool-06","verdict":"PASS"}`)
	got, _ := l.MergeReadinessFor("bbb")
	if got.Ready {
		t.Fatalf("a split decision must not be merge-ready: %+v", got)
	}
	if !strings.Contains(got.Reason, "disagree") {
		t.Errorf("reason must name the disagreement: %q", got.Reason)
	}
}

// BLOCKED is stronger than FAIL and must block even alongside a PASS.
func TestMergeReadiness_BlockedAlwaysBlocks(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"verdict","sha":"ccc","reviewer":"r1","verdict":"PASS"}`,
		`{"event":"verdict","sha":"ccc","reviewer":"r2","verdict":"BLOCKED"}`)
	got, _ := l.MergeReadinessFor("ccc")
	if got.Ready {
		t.Fatalf("BLOCKED must block: %+v", got)
	}
}

// A later verdict from the SAME reviewer supersedes its earlier one, so a fixed
// candidate can become ready without a second reviewer.
func TestMergeReadiness_SameReviewerSupersedes(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"verdict","sha":"ddd","reviewer":"r1","verdict":"FAIL"}`,
		`{"event":"verdict","sha":"ddd","reviewer":"r1","verdict":"PASS"}`)
	got, _ := l.MergeReadinessFor("ddd")
	if !got.Ready {
		t.Fatalf("a reviewer's own later PASS must supersede its FAIL: %+v", got)
	}
}

// No verdict at all is not readiness. Absence is not a pass.
func TestMergeReadiness_NoVerdictIsNotReady(t *testing.T) {
	l := ledgerWith(t, `{"event":"record","sha":"eee","reviewer":"r1"}`)
	got, _ := l.MergeReadinessFor("eee")
	if got.Ready {
		t.Fatalf("no verdict must not be ready: %+v", got)
	}
}

// A genuine clean pass must still be ready, so the guard cannot be satisfied by
// never returning true.
func TestMergeReadiness_CleanPassIsReady(t *testing.T) {
	l := ledgerWith(t, `{"event":"verdict","sha":"fff","reviewer":"r1","verdict":"PASS"}`)
	got, _ := l.MergeReadinessFor("fff")
	if !got.Ready || got.Passes != 1 {
		t.Fatalf("a clean PASS must be ready: %+v", got)
	}
}

// The ledger stores 40-char SHAs; callers hold 12-char short forms from PR head
// refs and pane names. Exact matching reported "no verdict recorded" for
// candidates that had several -- an absence that reads as safe.
func TestMergeReadiness_MatchesShortAndLongSHA(t *testing.T) {
	full := "ce46de20808a1111111111111111111111111111"
	l := ledgerWith(t, `{"event":"verdict","sha":"`+full+`","reviewer":"r1","verdict":"FAIL"}`)

	short, err := l.MergeReadinessFor("ce46de20808a")
	if err != nil {
		t.Fatal(err)
	}
	if short.Failures != 1 {
		t.Fatalf("short sha must find the verdict, got %+v", short)
	}
	if short.Ready {
		t.Fatal("a FAIL found by short sha must still block")
	}
	long, _ := l.MergeReadinessFor(full)
	if long.Failures != short.Failures {
		t.Fatalf("short and long forms must agree: %+v vs %+v", short, long)
	}
}

// A prefix shorter than 12 chars must NOT match, or unrelated commits collide.
func TestMergeReadiness_RejectsTooShortPrefix(t *testing.T) {
	l := ledgerWith(t, `{"event":"verdict","sha":"ce46de20808a1111111111111111111111111111","reviewer":"r1","verdict":"PASS"}`)
	got, _ := l.MergeReadinessFor("ce46de")
	if got.Passes != 0 {
		t.Fatalf("a 6-char prefix must not match; collision risk: %+v", got)
	}
}

// FAC-627: an honest "provenance was never recorded" must PRESERVE the review
// but must not grant the independence claim. Discarding it is what left the
// review host with 7 free lanes and ~20 candidates it was forbidden to touch.
func TestMergeReadiness_UnrecordedProvenancePassIsAdmittedButNotReady(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"verdict","sha":"aaa1111111111111111111111111111111111111","reviewer":"r1","verdict":"PASS","gate":"provenance-unrecorded","builder_family":"unrecorded"}`)
	got, err := l.MergeReadinessFor("aaa1111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if got.Passes != 1 {
		t.Fatalf("the review must be preserved, not discarded: %+v", got)
	}
	if got.Ready {
		t.Fatalf("unprovable authorship must not read as a clean pass: %+v", got)
	}
	if got.ProvenanceUnrecorded != 1 {
		t.Fatalf("the unrecorded provenance must be counted and visible: %+v", got)
	}
}

// A candidate with BOTH an unrecorded pass and a genuine cross-family pass is
// ready: the provable review carries it.
func TestMergeReadiness_ProvableePassAlongsideUnrecordedIsReady(t *testing.T) {
	sha := "bbb1111111111111111111111111111111111111"
	l := ledgerWith(t,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"r1","verdict":"PASS","gate":"provenance-unrecorded","builder_family":"unrecorded"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"r2","verdict":"PASS","gate":"independent","builder_family":"openai"}`)
	got, _ := l.MergeReadinessFor(sha)
	if !got.Ready {
		t.Fatalf("a provable PASS must still carry the candidate: %+v", got)
	}
}

// A FAIL under the unrecorded gate still blocks -- admitting the review does not
// weaken a negative verdict.
func TestMergeReadiness_UnrecordedFailStillBlocks(t *testing.T) {
	sha := "ccc1111111111111111111111111111111111111"
	l := ledgerWith(t,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"r1","verdict":"FAIL","gate":"provenance-unrecorded","builder_family":"unrecorded"}`)
	got, _ := l.MergeReadinessFor(sha)
	if got.Ready {
		t.Fatalf("a FAIL must block regardless of provenance: %+v", got)
	}
}

// The unrecorded gate must reject a real family: it exists for the honest
// unknown case only, not as a bypass for recording a family without proof.
func TestValidateRecord_UnrecordedGateRejectsARealFamily(t *testing.T) {
	err := validateRecord(RecordOpts{Gate: GateProvenanceUnrecorded, BuilderFamily: "xai"})
	if err == nil {
		t.Fatal("the unrecorded gate must not accept an asserted family; that is the bypass it exists to avoid")
	}
}

func TestValidateRecord_UnrecordedGateAcceptsUnrecorded(t *testing.T) {
	if err := validateRecord(RecordOpts{Gate: GateProvenanceUnrecorded, BuilderFamily: FamilyUnrecorded}); err != nil {
		t.Fatalf("honest unrecorded provenance must be admissible: %v", err)
	}
}
