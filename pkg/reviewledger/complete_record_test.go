package reviewledger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completionFixture(t *testing.T, mutate func(*LedgerRow, *LedgerRow)) (*Ledger, string, string) {
	t.Helper()
	l := newTestLedger(t)
	sha := strings.Repeat("a", 40)
	reviewer := "independent"
	r := LedgerRow{Event: string(EventRecord), SHA: sha, Reviewer: reviewer, Task: "FAC-759", BuilderFamily: FamilyUnrecorded, Gate: GateProvenanceUnrecorded, Lease: "real-lease", PatchURL: "real-patch"}
	v := LedgerRow{Event: string(EventVerdict), SHA: sha, CandidateSHA: sha, Reviewer: reviewer, Task: "FAC-759", BuilderFamily: "openai", ReviewerFamily: "google", Verdict: string(VerdictPASS), VerificationDigest: "real-verification", ArtifactDigest: strings.Repeat("b", 64)}
	if mutate != nil {
		mutate(&r, &v)
	}
	if e := l.appendRow(l.Path, &r); e != nil {
		t.Fatal(e)
	}
	if e := l.appendRow(l.Path, &v); e != nil {
		t.Fatal(e)
	}
	return l, sha, reviewer
}

func TestCompleteAdmissionRecordRefusesConflicts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LedgerRow, *LedgerRow)
	}{
		{"record task", func(r, v *LedgerRow) { r.Task = "FAC-758" }},
		{"verdict task", func(r, v *LedgerRow) { v.Task = "FAC-758" }},
		{"candidate", func(r, v *LedgerRow) { v.CandidateSHA = strings.Repeat("c", 40) }},
		{"FAIL", func(r, v *LedgerRow) { v.Verdict = string(VerdictFAIL) }},
		{"missing digest", func(r, v *LedgerRow) { v.VerificationDigest = "" }},
		{"missing artifact", func(r, v *LedgerRow) { v.ArtifactDigest = "" }},
		{"same family", func(r, v *LedgerRow) { v.ReviewerFamily = v.BuilderFamily }},
		{"unknown family", func(r, v *LedgerRow) { v.BuilderFamily = FamilyUnrecorded }},
		{"changed builder", func(r, v *LedgerRow) { r.BuilderFamily = "anthropic" }},
		{"changed reviewer", func(r, v *LedgerRow) { r.ReviewerFamily = "anthropic" }},
		{"changed branch", func(r, v *LedgerRow) { r.Branch = "different" }},
		{"changed tier", func(r, v *LedgerRow) { r.Tier = "R0" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, sha, reviewer := completionFixture(t, tc.mutate)
			before, e := os.ReadFile(l.Path)
			if e != nil {
				t.Fatal(e)
			}
			e = l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) { return RecordCompletion{Branch: "work", Tier: "R3"}, nil })
			if e == nil {
				t.Fatal("conflicting evidence admitted")
			}
			after, e := os.ReadFile(l.Path)
			if e != nil {
				t.Fatal(e)
			}
			if string(after) != string(before) {
				t.Fatal("refusal changed ledger")
			}
		})
	}
}

func TestCompleteAdmissionRecordRejectsDissentAndUnverifiedEvidence(t *testing.T) {
	for _, dissent := range []bool{false, true} {
		t.Run(fmt.Sprint(dissent), func(t *testing.T) {
			l, sha, reviewer := completionFixture(t, nil)
			if dissent {
				if e := l.appendRow(l.Path, &LedgerRow{Event: string(EventVerdict), SHA: sha, Reviewer: "other-reviewer", Task: "FAC-759", Verdict: string(VerdictBLOCKED)}); e != nil {
					t.Fatal(e)
				}
			}
			e := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) {
				if !dissent {
					return RecordCompletion{}, fmt.Errorf("native proof missing")
				}
				return RecordCompletion{Branch: "work", Tier: "R3"}, nil
			})
			if e == nil {
				t.Fatal("unverified or disputed evidence admitted")
			}
		})
	}
}

func TestCompleteAdmissionRecordSerializesAliases(t *testing.T) {
	l, sha, reviewer := completionFixture(t, nil)
	alias := filepath.Join(l.RepoRoot, "alias.jsonl")
	if e := os.Symlink(l.Path, alias); e != nil {
		t.Fatal(e)
	}
	other, e := NewReadOnlyReviewLedger(l.RepoRoot, alias)
	if e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	one := make(chan error, 1)
	two := make(chan error, 1)
	go func() {
		one <- l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) {
			close(entered)
			<-release
			return RecordCompletion{Branch: "work", Tier: "R3"}, nil
		})
	}()
	<-entered
	go func() {
		two <- other.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) { return RecordCompletion{Branch: "work", Tier: "R3"}, nil })
	}()
	select {
	case e := <-two:
		t.Fatalf("second completion escaped locked evidence check: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if e := <-one; e != nil {
		t.Fatal(e)
	}
	if e := <-two; e != nil {
		t.Fatal(e)
	}
	rows, e := l.AllRows()
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 3 {
		t.Fatalf("want one appended record, got %d rows", len(rows))
	}
}

func TestCompletionRequiresExactHostProjection(t *testing.T) {
	orders := [][]string{{"host-b", "host-a"}, {"host-a", "host-b"}}
	for _, order := range orders {
		t.Run(strings.Join(order, "_then_"), func(t *testing.T) {
			l := newTestLedger(t)
			sha := strings.Repeat("a", 40)
			reviewer := "independent"
			for _, host := range order {
				r := LedgerRow{Event: string(EventRecord), SHA: sha, Reviewer: reviewer, Host: host, Task: "FAC-759", BuilderFamily: FamilyUnrecorded, Gate: GateProvenanceUnrecorded, Lease: "real-lease", PatchURL: "real-patch"}
				if e := l.appendRow(l.Path, &r); e != nil {
					t.Fatal(e)
				}
			}
			v := LedgerRow{Event: string(EventVerdict), SHA: sha, CandidateSHA: sha, Reviewer: reviewer, Host: "host-b", Task: "FAC-759", BuilderFamily: "openai", ReviewerFamily: "google", Verdict: string(VerdictPASS), VerificationDigest: "real-verification", ArtifactDigest: strings.Repeat("b", 64)}
			if e := l.appendRow(l.Path, &v); e != nil {
				t.Fatal(e)
			}
			verify := func(got LedgerRow) (RecordCompletion, error) {
				if got.Host != "host-b" {
					t.Fatalf("completion used host %q", got.Host)
				}
				return RecordCompletion{Branch: "work", Tier: "R3"}, nil
			}
			if e := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "host-a", verify); e == nil {
				t.Fatal("host-B PASS completed host-A record")
			}
			if e := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "host-b", verify); e != nil {
				t.Fatalf("host-B completion: %v", e)
			}
		})
	}
}
