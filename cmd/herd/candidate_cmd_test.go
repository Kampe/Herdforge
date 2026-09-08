package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/candidate"
)

func TestLedgerReviewsAdmittedForRefPreservesCrossHostDissent(t *testing.T) {
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reviewer := "review-fac-765-52bbffb7dc79"
	ref := "FAC-765"

	t.Run("hostA_FAIL_then_hostB_PASS", func(t *testing.T) {
		reviews := admittedReviews(t, sha, reviewer, ref,
			hostVerdictRow{"host-a", "FAIL"},
			hostVerdictRow{"host-b", "PASS"},
		)
		if len(reviews) != 1 {
			t.Fatalf("reviews=%d want 1", len(reviews))
		}
		if reviews[0].Verdict != "FAIL" {
			t.Fatalf("verdict=%q want FAIL (cross-host PASS must not hide FAIL)", reviews[0].Verdict)
		}
	})
	t.Run("reversed_hostB_PASS_then_hostA_FAIL", func(t *testing.T) {
		reviews := admittedReviews(t, sha, reviewer, ref,
			hostVerdictRow{"host-b", "PASS"},
			hostVerdictRow{"host-a", "FAIL"},
		)
		if len(reviews) != 1 || reviews[0].Verdict != "FAIL" {
			t.Fatalf("reviews=%+v want FAIL", reviews)
		}
	})
	t.Run("same_host_FAIL_then_PASS_reassessment", func(t *testing.T) {
		reviews := admittedReviews(t, sha, reviewer, ref,
			hostVerdictRow{"host-a", "FAIL"},
			hostVerdictRow{"host-a", "PASS"},
		)
		if len(reviews) != 1 || reviews[0].Verdict != "PASS" {
			t.Fatalf("reviews=%+v want PASS after same-host reassessment", reviews)
		}
	})
}

func admittedReviews(t *testing.T, sha, reviewer, ref string, vs ...hostVerdictRow) []candidate.Review {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review-ledger.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, v := range vs {
		row := map[string]string{
			"event":    "verdict",
			"sha":      sha,
			"reviewer": reviewer,
			"verdict":  v.verdict,
			"branch":   "task/" + ref,
			"artifact": ref + ".md",
		}
		if v.host != "" {
			row["host"] = v.host
		}
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := (ledgerReviews{path: path}).AdmittedForRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.CandidateSHA != sha {
			t.Fatalf("sha=%q want %s", r.CandidateSHA, sha)
		}
	}
	return got
}
