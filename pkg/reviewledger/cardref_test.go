package reviewledger

import (
	"strings"
	"testing"
)

func TestCloseableCardRefExactOnly(t *testing.T) {
	cases := map[string]string{
		"CHA-2345":               "CHA-2345",
		"fac-12":                 "FAC-12",
		"  CHA-1  ":              "CHA-1",
		"standing/api-crusader":   "",
		"wt/chain-indexer":       "",
		"fix/cha-2120-telegram": "", // substring must not count
		"feat/some-branch":       "",
		"":                       "",
		"nonsense":               "",
		"toolongprefix-1":        "",
	}
	for in, want := range cases {
		if got := CloseableCardRef(in); got != want {
			t.Errorf("CloseableCardRef(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRequireCloseableCardRefNamesTheBadValue(t *testing.T) {
	err := RequireCloseableCardRef("standing/api-crusader", "artifact x.md task")
	if err == nil {
		t.Fatal("expected refusal")
	}
	msg := err.Error()
	for _, want := range []string{"FAC-578", `artifact x.md task "standing/api-crusader"`, "closeable card ref"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal missing %q in %q", want, msg)
		}
	}
}

func TestBuildEvidenceGapReport(t *testing.T) {
	rows := []LedgerRow{
		{Task: "standing/api-crusader"},
		{Task: "standing/api-crusader"},
		{Task: "CHA-100"},
		{Task: "wt/chain-indexer"},
		{Task: ""},
	}
	report := BuildEvidenceGapReport(rows, []string{"CHA-100", "CHA-200", "CHA-200", "not-a-card"})
	if report.LedgerRowsScanned != 5 {
		t.Fatalf("scanned=%d", report.LedgerRowsScanned)
	}
	if report.InReviewChecked != 4 {
		t.Fatalf("checked=%d", report.InReviewChecked)
	}
	if len(report.InReviewWithoutEvidence) != 1 || report.InReviewWithoutEvidence[0] != "CHA-200" {
		t.Fatalf("missing=%v want [CHA-200]", report.InReviewWithoutEvidence)
	}
	if len(report.NonCloseableTasks) != 2 {
		t.Fatalf("non-closeable=%v", report.NonCloseableTasks)
	}
	if report.NonCloseableTasks[0].Task != "standing/api-crusader" || report.NonCloseableTasks[0].Count != 2 {
		t.Fatalf("top leak=%v", report.NonCloseableTasks[0])
	}
}
