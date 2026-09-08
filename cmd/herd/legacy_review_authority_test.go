package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestSameHostRetryReachesLegacyClosure(t *testing.T) {
	sha := "dddddddddddddddddddddddddddddddddddddddd"
	ref := "FAC-765"
	dir := t.TempDir()
	path := filepath.Join(dir, "review-ledger.jsonl")
	l, err := reviewledger.NewReviewLedger(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	mustLegacyRecord(t, l, sha, "reviewer-a", "host-a", ref)
	mustLegacyRecord(t, l, sha, "reviewer-b", "host-a", ref)
	mustLegacyVerdict(t, l, sha, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "", ref)
	mustLegacyVerdict(t, l, sha, "reviewer-b", "host-a", reviewledger.VerdictPASS, "reviewer-a", ref)
	ev, err := newLedgerLegacyReview(path).AdmittedPass(ref)
	if err != nil {
		t.Fatalf("legacy closure: %v", err)
	}
	if ev.CandidateSHA != sha || ev.Verdict != "PASS" {
		t.Fatalf("legacy %+v want current PASS", ev)
	}
}

func TestLegacyClosureCrossHostDissentBothOrders(t *testing.T) {
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := "FAC-765"
	orders := [][]string{{"fail-first", "pass-second"}, {"pass-first", "fail-second"}}
	vetoes := []reviewledger.Verdict{reviewledger.VerdictFAIL, reviewledger.VerdictBLOCKED}
	for _, order := range orders {
		for _, veto := range vetoes {
			t.Run(strings.Join(order, "_")+string(veto), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "review-ledger.jsonl")
				l, err := reviewledger.NewReviewLedger(dir, path)
				if err != nil {
					t.Fatal(err)
				}
				mustLegacyRecord(t, l, sha, "reviewer-a", "host-a", ref)
				mustLegacyRecord(t, l, sha, "reviewer-a", "host-b", ref)
				fail := func() {
					mustLegacyVerdict(t, l, sha, "reviewer-a", "host-a", veto, "", ref)
				}
				pass := func() {
					mustLegacyVerdict(t, l, sha, "reviewer-a", "host-b", reviewledger.VerdictPASS, "reviewer-a", ref)
				}
				if order[0] == "fail-first" {
					fail()
					pass()
				} else {
					pass()
					fail()
				}
				_, err = newLedgerLegacyReview(path).AdmittedPass(ref)
				if err == nil || !strings.Contains(err.Error(), "no current admitted PASS") {
					t.Fatalf("cross-host %s %s closed: err=%v", order, veto, err)
				}
			})
		}
	}
}

func TestLegacyClosureConsumeAndReenqueue(t *testing.T) {
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ref := "FAC-765"
	t.Run("consumed_withdraws", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "review-ledger.jsonl")
		l, err := reviewledger.NewReviewLedger(dir, path)
		if err != nil {
			t.Fatal(err)
		}
		mustLegacyRecord(t, l, sha, "reviewer-b", "host-a", ref)
		mustLegacyVerdict(t, l, sha, "reviewer-b", "host-a", reviewledger.VerdictPASS, "", ref)
		if err := l.Consumed(sha, strings.Repeat("c", 40)); err != nil {
			t.Fatal(err)
		}
		_, err = newLedgerLegacyReview(path).AdmittedPass(ref)
		if err == nil || !strings.Contains(err.Error(), "no current admitted PASS") {
			t.Fatalf("consumed PASS still closed: err=%v", err)
		}
	})
	t.Run("same_host_retry_reenqueues", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "review-ledger.jsonl")
		l, err := reviewledger.NewReviewLedger(dir, path)
		if err != nil {
			t.Fatal(err)
		}
		mustLegacyRecord(t, l, sha, "reviewer-a", "host-a", ref)
		mustLegacyRecord(t, l, sha, "reviewer-b", "host-a", ref)
		mustLegacyVerdict(t, l, sha, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "", ref)
		mustLegacyVerdict(t, l, sha, "reviewer-b", "host-a", reviewledger.VerdictPASS, "reviewer-a", ref)
		ev, err := newLedgerLegacyReview(path).AdmittedPass(ref)
		if err != nil || ev.CandidateSHA != sha || ev.Verdict != "PASS" {
			t.Fatalf("authorized same-host retry: %+v err=%v", ev, err)
		}
	})
}

func mustLegacyRecord(t *testing.T, l *reviewledger.Ledger, sha, reviewer, host, ref string) {
	t.Helper()
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Reviewer: reviewer, Host: host, Branch: "task/" + ref,
		BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent",
	}); err != nil {
		t.Fatal(err)
	}
}

func mustLegacyVerdict(t *testing.T, l *reviewledger.Ledger, sha, reviewer, host string, v reviewledger.Verdict, retry, ref string) {
	t.Helper()
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: reviewer, Host: host, Verdict: v, RetryOf: retry,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", Branch: "task/" + ref, Artifact: "task/" + ref,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRevocationWithdrawsLegacyClosure(t *testing.T) {
	sha := "cccccccccccccccccccccccccccccccccccccccc"
	ref := "FAC-765"
	dir := t.TempDir()
	path := filepath.Join(dir, "review-ledger.jsonl")
	l, err := reviewledger.NewReviewLedger(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Reviewer: "reviewer-b", Host: "host-a", Branch: "task/" + ref,
		BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent",
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Branch: "task/" + ref,
		BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: "reviewer-b", Host: "host-a", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", Branch: "task/" + ref, Artifact: "task/" + ref,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Verdict: reviewledger.VerdictFAIL,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", Branch: "task/" + ref,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = newLedgerLegacyReview(path).AdmittedPass(ref)
	if err == nil || !strings.Contains(err.Error(), "no current admitted PASS") {
		t.Fatalf("native queue EventRevoked must withdraw PASS, got err=%v", err)
	}
}

func TestLegacyClosureEventRevokedWithdrawsSHA(t *testing.T) {
	sha := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	ref := "FAC-765"
	path := filepath.Join(t.TempDir(), "review-ledger.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, row := range []map[string]string{
		{"event": "verdict", "sha": sha, "reviewer": "reviewer-b", "host": "host-a", "verdict": "PASS", "branch": "task/" + ref, "artifact": "b.md", "reviewer_family": "openai"},
		{"event": "revoked", "sha": sha, "branch": "task/" + ref},
	} {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = newLedgerLegacyReview(path).AdmittedPass(ref)
	if err == nil || !strings.Contains(err.Error(), "no current admitted PASS") {
		t.Fatalf("EventRevoked must withdraw SHA, got err=%v", err)
	}
}

func TestLegacyClosureIdentityReplacementWithdrawsPrevious(t *testing.T) {
	old := "ffffffffffffffffffffffffffffffffffffffff"
	ref := "FAC-765"
	path := filepath.Join(t.TempDir(), "review-ledger.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, row := range []map[string]string{
		{"event": "verdict", "sha": old, "reviewer": "reviewer-b", "host": "host-a", "verdict": "PASS", "branch": "task/" + ref, "artifact": "b.md", "reviewer_family": "openai"},
		{"event": "supersession", "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "task": old, "reviewer": "reviewer-a", "reason": "candidate identity replacement", "status": "superseded"},
	} {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = newLedgerLegacyReview(path).AdmittedPass(ref)
	if err == nil || !strings.Contains(err.Error(), "no current admitted PASS") {
		t.Fatalf("identity replacement must withdraw previous SHA, got err=%v", err)
	}
}
