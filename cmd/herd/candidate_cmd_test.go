package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/candidate"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestLedgerReviewsAdmittedForRefPreservesCrossHostDissent(t *testing.T) {
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reviewer := "review-fac-765-52bbffb7dc79"
	ref := "FAC-765"

	t.Run("hostA_FAIL_then_hostB_PASS", func(t *testing.T) {
		reviews := admittedReviews(t, sha, reviewer, ref,
			hostVerdictRow{host: "host-a", verdict: "FAIL", artifact: "a.md", family: "anthropic"},
			hostVerdictRow{host: "host-b", verdict: "PASS", artifact: "b.md", family: "xai"},
		)
		if len(reviews) != 1 {
			t.Fatalf("reviews=%d want 1", len(reviews))
		}
		if reviews[0].Verdict != "FAIL" {
			t.Fatalf("verdict=%q want FAIL (cross-host PASS must not hide FAIL)", reviews[0].Verdict)
		}
		if reviews[0].Artifact != "a.md" || reviews[0].ReviewerFamily != "anthropic" {
			t.Fatalf("mixed FAIL identity %+v", reviews[0])
		}
	})
	t.Run("reversed_hostB_PASS_then_hostA_FAIL", func(t *testing.T) {
		reviews := admittedReviews(t, sha, reviewer, ref,
			hostVerdictRow{host: "host-b", verdict: "PASS", artifact: "b.md", family: "xai"},
			hostVerdictRow{host: "host-a", verdict: "FAIL", artifact: "a.md", family: "anthropic"},
		)
		if len(reviews) != 1 || reviews[0].Verdict != "FAIL" {
			t.Fatalf("reviews=%+v want FAIL", reviews)
		}
		if reviews[0].Artifact != "a.md" || reviews[0].ReviewerFamily != "anthropic" {
			t.Fatalf("mixed FAIL identity %+v", reviews[0])
		}
	})
	t.Run("same_host_FAIL_then_PASS_reassessment", func(t *testing.T) {
		reviews := admittedReviews(t, sha, reviewer, ref,
			hostVerdictRow{host: "host-a", verdict: "FAIL", artifact: "a.md", family: "anthropic"},
			hostVerdictRow{host: "host-a", verdict: "PASS", artifact: "a.md", family: "anthropic"},
		)
		if len(reviews) != 1 || reviews[0].Verdict != "PASS" {
			t.Fatalf("reviews=%+v want PASS after same-host reassessment", reviews)
		}
		if reviews[0].Artifact != "a.md" || reviews[0].ReviewerFamily != "anthropic" {
			t.Fatalf("reassessment mixed identity %+v", reviews[0])
		}
	})
	t.Run("single_veto", func(t *testing.T) {
		reviews := admittedReviews(t, sha, reviewer, ref,
			hostVerdictRow{host: "host-a", verdict: "FAIL", artifact: "a.md", family: "anthropic"},
		)
		if len(reviews) != 1 || reviews[0].Verdict != "FAIL" || reviews[0].Artifact != "a.md" {
			t.Fatalf("single veto %+v", reviews)
		}
	})
}

func TestLedgerReviewsSameHostRetryRetainsPass(t *testing.T) {
	sha := "dddddddddddddddddddddddddddddddddddddddd"
	ref := "FAC-765"
	dir := t.TempDir()
	path := filepath.Join(dir, "review-ledger.jsonl")
	l, err := reviewledger.NewReviewLedger(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	mustCandidateRecord(t, l, sha, "reviewer-a", "host-a", ref)
	mustCandidateRecord(t, l, sha, "reviewer-b", "host-a", ref)
	mustCandidateVerdict(t, l, sha, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "", ref, ref+"-a.md")
	mustCandidateVerdict(t, l, sha, "reviewer-b", "host-a", reviewledger.VerdictPASS, "reviewer-a", ref, ref+"-b.md")
	got, err := (ledgerReviews{path: path}).AdmittedForRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Verdict != "PASS" || got[0].Artifact != ref+"-b.md" {
		t.Fatalf("same-host retry display %+v want PASS %s-b.md", got, ref)
	}
	id, err := candidate.Resolve(context.Background(), ref, ledgerReviews{path: path}, harvestGit{})
	if err != nil {
		t.Fatal(err)
	}
	if id.Disposition != candidate.DispositionHarvest || id.Verdict != "PASS" {
		t.Fatalf("native same-host retry Resolve %+v want harvest PASS", id)
	}
}

func TestCandidateFabricatedRetryDoesNotAuthorizeHarvest(t *testing.T) {
	sha := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	ref := "FAC-765"
	dir := t.TempDir()
	path := filepath.Join(dir, "review-ledger.jsonl")
	l, err := reviewledger.NewReviewLedger(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	mustCandidateRecord(t, l, sha, "reviewer-a", "host-a", ref)
	mustCandidateVerdict(t, l, sha, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "", ref, ref+"-a.md")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(f).Encode(map[string]string{
		"event": "verdict", "sha": sha, "reviewer": "reviewer-b", "host": "host-a",
		"verdict": "PASS", "retry_of": "reviewer-a", "branch": "task/" + ref, "artifact": "b.md",
		"reviewer_family": "openai", "builder_family": "anthropic",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := (ledgerReviews{path: path}).AdmittedForRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Verdict != "FAIL" {
		t.Fatalf("fabricated RetryOf authorized candidate reviews %+v want FAIL", got)
	}
	id, err := candidate.Resolve(context.Background(), ref, ledgerReviews{path: path}, harvestGit{})
	if err != nil {
		t.Fatal(err)
	}
	if id.Disposition == candidate.DispositionHarvest || id.Verdict == "PASS" {
		t.Fatalf("fabricated RetryOf authorized Harvest: %+v", id)
	}
	if id.Disposition != candidate.DispositionRepair {
		t.Fatalf("disposition=%s want repair", id.Disposition)
	}
}

type harvestGit struct{}

func (harvestGit) ObjectExists(context.Context, string) bool { return true }
func (harvestGit) BranchesContaining(context.Context, string) ([]string, error) {
	return []string{"task/FAC-765"}, nil
}
func (harvestGit) WorktreeForBranch(context.Context, string) (string, error) { return "", nil }
func (harvestGit) ContainedInMain(context.Context, string) bool              { return false }
func (harvestGit) PatchLandedOnMain(context.Context, string) bool            { return false }

func mustCandidateRecord(t *testing.T, l *reviewledger.Ledger, sha, reviewer, host, ref string) {
	t.Helper()
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Reviewer: reviewer, Host: host, Branch: "task/" + ref,
		BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent",
	}); err != nil {
		t.Fatal(err)
	}
}

func mustCandidateVerdict(t *testing.T, l *reviewledger.Ledger, sha, reviewer, host string, v reviewledger.Verdict, retry, ref, artifact string) {
	t.Helper()
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: reviewer, Host: host, Verdict: v, RetryOf: retry,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", Branch: "task/" + ref, Artifact: artifact,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerReviewsConflictingHostVetoesAreStable(t *testing.T) {
	sha := "cccccccccccccccccccccccccccccccccccccccc"
	reviewer := "review-fac-765-a336abd2fd66"
	ref := "FAC-765"
	want := candidate.Review{
		CandidateSHA:   sha,
		Verdict:        "FAIL",
		Artifact:       "a.md",
		Reviewer:       reviewer,
		ReviewerFamily: "anthropic",
	}
	orders := [][]hostVerdictRow{
		{
			{host: "host-a", verdict: "FAIL", artifact: "a.md", family: "anthropic"},
			{host: "host-b", verdict: "FAIL", artifact: "b.md", family: "xai"},
		},
		{
			{host: "host-b", verdict: "FAIL", artifact: "b.md", family: "xai"},
			{host: "host-a", verdict: "FAIL", artifact: "a.md", family: "anthropic"},
		},
	}
	seen := map[string]int{}
	for _, rows := range orders {
		for i := 0; i < 100; i++ {
			reviews := admittedReviews(t, sha, reviewer, ref, rows...)
			if len(reviews) != 1 {
				t.Fatalf("reviews=%d want 1", len(reviews))
			}
			got := reviews[0]
			key := got.Artifact + "/" + got.ReviewerFamily + "/" + got.Verdict
			seen[key]++
			if got.Verdict != want.Verdict || got.Artifact != want.Artifact || got.ReviewerFamily != want.ReviewerFamily {
				t.Fatalf("incoherent or unstable display %+v want artifact=%s family=%s", got, want.Artifact, want.ReviewerFamily)
			}
			if got.Reviewer != reviewer {
				t.Fatalf("reviewer=%q", got.Reviewer)
			}
		}
	}
	if len(seen) != 1 {
		t.Fatalf("display identities varied across reads: %v", seen)
	}
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
		artifact := v.artifact
		if artifact == "" {
			artifact = ref + ".md"
		}
		row := map[string]string{
			"event":    "verdict",
			"sha":      sha,
			"reviewer": reviewer,
			"verdict":  v.verdict,
			"branch":   "task/" + ref,
			"artifact": artifact,
		}
		if v.host != "" {
			row["host"] = v.host
		}
		if v.family != "" {
			row["reviewer_family"] = v.family
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
