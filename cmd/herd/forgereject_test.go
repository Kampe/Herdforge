package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-140: the coordinator's view of "which cards carry an unrepaired FAIL"
// is projected from the review ledger. Verdict rows carry the sha but not the
// branch (Ledger.Verdict writes the branch to the queue row), so the ref comes
// from joining the two.

const failArtifact = "verdict.md"

func verdictRow(sha, verdict, reviewer, artifact string) reviewledger.LedgerRow {
	return reviewledger.LedgerRow{
		Event: string(reviewledger.EventVerdict), SHA: sha,
		Verdict: verdict, Reviewer: reviewer, Artifact: artifact,
	}
}

func fakeArtifact(body string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if path != failArtifact {
			return nil, errors.New("no such file")
		}
		return []byte("sha: " + strings.Repeat("a", 40) + "\nverdict: FAIL\n---\n" + body + "\n"), nil
	}
}

func TestOutstandingRejections_FailIsProjectedWithFindings(t *testing.T) {
	sha := strings.Repeat("a", 40)
	rows := []reviewledger.LedgerRow{verdictRow(sha, "FAIL", "review-fac-121-openai", failArtifact)}
	queue := []reviewledger.LedgerRow{{Event: string(reviewledger.EventRevoked), SHA: sha, Branch: "herd/fac-121"}}

	got := outstandingRejections(rows, queue, fakeArtifact("1. receipt.go:65 false prompt-consumption."))
	r, ok := got["FAC-121"]
	if !ok {
		t.Fatalf("FAIL verdict did not project onto its ref: %+v", got)
	}
	if r.SHA != sha || r.Reviewer != "review-fac-121-openai" || r.Artifact != failArtifact {
		t.Fatalf("rejection lost its provenance: %+v", r)
	}
	// The worker repairs against the reviewer's own words, not a summary — and
	// the front matter is not part of them.
	if !strings.Contains(r.Findings, "receipt.go:65") {
		t.Fatalf("findings body missing the reviewer's evidence: %q", r.Findings)
	}
	if strings.Contains(r.Findings, "verdict: FAIL") {
		t.Fatalf("findings must be the body, not the front matter: %q", r.Findings)
	}
}

func TestOutstandingRejections_OnlyAPassClearsAFail(t *testing.T) {
	old, fresh := strings.Repeat("a", 40), strings.Repeat("b", 40)
	queue := []reviewledger.LedgerRow{
		{Event: string(reviewledger.EventRevoked), SHA: old, Branch: "herd/fac-121"},
		{Event: string(reviewledger.EventEnqueue), SHA: fresh, Branch: "herd/fac-121"},
	}
	read := fakeArtifact("1. finding")

	// A later BLOCKED grants no merge authority, so it must not wipe the FAIL.
	blocked := []reviewledger.LedgerRow{
		verdictRow(old, "FAIL", "r1", failArtifact),
		verdictRow(fresh, "BLOCKED", "r2", failArtifact),
	}
	if _, ok := outstandingRejections(blocked, queue, read)["FAC-121"]; !ok {
		t.Fatal("a BLOCKED verdict wiped an outstanding FAIL")
	}

	// A PASS on the repaired candidate is what closes the loop.
	passed := []reviewledger.LedgerRow{
		verdictRow(old, "FAIL", "r1", failArtifact),
		verdictRow(fresh, "PASS", "r2", failArtifact),
	}
	if r, ok := outstandingRejections(passed, queue, read)["FAC-121"]; ok {
		t.Fatalf("a fresh PASS must clear the rejection, still held %+v", r)
	}
}

// A re-FAIL on a fresh candidate must replace the old rejection, or the
// coordinator's (ref, SHA) idempotency key would never advance and the worker
// would never hear about the second rejection.
func TestOutstandingRejections_ReFailAdvancesTheCandidateSHA(t *testing.T) {
	old, fresh := strings.Repeat("a", 40), strings.Repeat("b", 40)
	rows := []reviewledger.LedgerRow{
		verdictRow(old, "FAIL", "r1", failArtifact),
		verdictRow(fresh, "FAIL", "r2", failArtifact),
	}
	queue := []reviewledger.LedgerRow{
		{Event: string(reviewledger.EventRevoked), SHA: old, Branch: "herd/fac-121"},
		{Event: string(reviewledger.EventRevoked), SHA: fresh, Branch: "herd/fac-121"},
	}
	got := outstandingRejections(rows, queue, fakeArtifact("1. finding"))["FAC-121"]
	if got.SHA != fresh {
		t.Fatalf("want the newest FAILed candidate %s, got %s", fresh, got.SHA)
	}
}

// A verdict whose ref cannot be resolved is dropped rather than guessed: a
// bogus key would veto a card nobody rejected.
func TestOutstandingRejections_UnresolvableRefIsDropped(t *testing.T) {
	rows := []reviewledger.LedgerRow{verdictRow(strings.Repeat("c", 40), "FAIL", "r1", failArtifact)}
	queue := []reviewledger.LedgerRow{
		{Event: string(reviewledger.EventRevoked), SHA: strings.Repeat("c", 40), Branch: "feature/not-a-task-branch"},
	}
	if got := outstandingRejections(rows, queue, fakeArtifact("1. finding")); len(got) != 0 {
		t.Fatalf("unresolvable ref produced a rejection: %+v", got)
	}
}

func TestRefForTaskBranch(t *testing.T) {
	for branch, want := range map[string]string{
		"herd/fac-140":            "FAC-140",
		" herd/fac-140 ":          "FAC-140",
		"main":                    "",
		"":                        "",
		"harvest-smith-abc":       "",
		"feature/herd/fac-140":    "",
		"herd/fac-140-superseded": "FAC-140-SUPERSEDED",
	} {
		if got := refForTaskBranch(branch); got != want {
			t.Errorf("refForTaskBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}

// Delivering an empty rejection would satisfy the routing and strand the
// worker exactly as the original defect did.
func TestReject_RefusesABareRejection(t *testing.T) {
	d := &cliForgeDriver{maxLanes: 1}
	err := d.Reject(context.Background(), &provider.Task{Ref: "FAC-121"},
		daemon.Rejection{Ref: "FAC-121", SHA: strings.Repeat("a", 40), Artifact: "missing.md", Findings: "  \n "})
	if err == nil {
		t.Fatal("a rejection with no findings was accepted for delivery")
	}
	if !strings.Contains(err.Error(), "no findings body") || !strings.Contains(err.Error(), "missing.md") {
		t.Fatalf("error must name the empty rejection and its artifact, got %v", err)
	}
}

// The payload is what actually closes the loop without a coordinator: the
// reviewer's findings verbatim plus the fresh-SHA repair order.
func TestRejectionPrompt_CarriesFindingsAndFreshSHAOrder(t *testing.T) {
	sha := strings.Repeat("a", 40)
	findings := "1. dispatch.go:149-160 discards outbox and compensation errors."
	got := rejectionPrompt("FAC-121", daemon.Rejection{
		Ref: "FAC-121", SHA: sha, Reviewer: "review-fac-121-openai", Findings: findings})

	for _, want := range []string{"FAC-121", findings, "review-fac-121-openai", sha, "fresh SHA", "Never merge"} {
		if !strings.Contains(got, want) {
			t.Errorf("rejection prompt is missing %q:\n%s", want, got)
		}
	}
}
