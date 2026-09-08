package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSameHostRetryReachesLegacyClosure(t *testing.T) {
	sha := "dddddddddddddddddddddddddddddddddddddddd"
	ref := "FAC-765"
	path := filepath.Join(t.TempDir(), "review-ledger.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	rows := []map[string]string{
		{"event": "verdict", "sha": sha, "reviewer": "reviewer-a", "host": "host-a", "verdict": "FAIL", "branch": "task/" + ref, "artifact": "a.md", "reviewer_family": "anthropic"},
		{"event": "verdict", "sha": sha, "reviewer": "reviewer-b", "host": "host-a", "verdict": "PASS", "retry_of": "reviewer-a", "branch": "task/" + ref, "artifact": "b.md", "reviewer_family": "openai"},
		{"event": "supersession", "sha": sha, "task": sha, "reviewer": "reviewer-a", "host": "host-a", "retry_of": "reviewer-a", "status": "superseded"},
	}
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ev, err := newLedgerLegacyReview(path).AdmittedPass(ref)
	if err != nil {
		t.Fatalf("legacy closure: %v", err)
	}
	if ev.CandidateSHA != sha || ev.Verdict != "PASS" {
		t.Fatalf("legacy %+v want current PASS", ev)
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
