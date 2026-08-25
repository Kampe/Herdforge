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
