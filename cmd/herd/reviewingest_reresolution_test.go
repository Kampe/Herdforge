package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

type provenanceReresolutionFixture struct {
	root, lane, branch, sha, artifact, ledgerPath string
	commitTime                                    time.Time
}

func newProvenanceReresolutionFixture(t *testing.T, artifactFamily string) provenanceReresolutionFixture {
	t.Helper()
	t.Setenv("HERD_LAUNCH_RECEIPTS", "")
	t.Setenv("HERD_REVIEW_LEDGER", "")
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git(root, "init", "-q", "-b", "main")
	git(root, "config", "user.email", "fac630@test.invalid")
	git(root, "config", "user.name", "fac630")
	git(root, "commit", "-q", "--allow-empty", "-m", "base")
	git(root, "branch", "origin/main")

	branch := "fac-630-historical"
	lane := filepath.Join(root, ".herd", "worktrees", branch)
	git(root, "worktree", "add", "-q", "-b", branch, lane)
	if err := os.WriteFile(filepath.Join(lane, "change.txt"), []byte("fac-630\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(lane, "add", "change.txt")
	git(lane, "commit", "-q", "-m", "fac-630 historical candidate")
	sha := strings.TrimSpace(runOutput(t, lane, "rev-parse", "HEAD"))
	commitISO := strings.TrimSpace(runOutput(t, lane, "show", "-s", "--format=%cI", sha))
	commitTime, err := time.Parse(time.RFC3339, commitISO)
	if err != nil {
		t.Fatalf("parse commit time %q: %v", commitISO, err)
	}

	ledgerPath := reviewledger.DefaultPath(lane)
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	l, err := reviewledger.NewReviewLedger(lane, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: branch, Task: "FAC-630", Reviewer: "reviewer-a",
		ReviewerFamily: "xai", BuilderFamily: reviewledger.FamilyUnrecorded,
		Gate: reviewledger.GateProvenanceUnrecorded, Tier: "R3",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Branch: branch, Task: "FAC-630", Reviewer: "reviewer-a",
		ReviewerFamily: "xai", BuilderFamily: "openai", Verdict: reviewledger.VerdictPASS,
		VfyDigest: "2344575ebd590030e9c06cfd230e1896",
	}); err != nil {
		t.Fatal(err)
	}

	body := "sha: " + sha + "\n" +
		"branch: " + branch + "\n" +
		"task: FAC-630\n" +
		"reviewer: reviewer-a\n" +
		"reviewer-family: xai\n"
	if artifactFamily != "" {
		body += "builder-family: " + artifactFamily + "\n"
	}
	body += "verdict: PASS\n" +
		"reviewed-head: " + sha + "\n" +
		"---\n" +
		"Verification: go test -count=1 ./pkg/reviewledger — PASS\n" +
		strings.Repeat("Independently verified the exact candidate and its focused tests. ", 8) + "\n"
	artifact := filepath.Join(lane, "historical-verdict.md")
	if err := os.WriteFile(artifact, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return provenanceReresolutionFixture{
		root: root, lane: lane, branch: branch, sha: sha, artifact: artifact,
		ledgerPath: ledgerPath, commitTime: commitTime,
	}
}

func (f provenanceReresolutionFixture) writeReceipts(t *testing.T, receipts ...launch.Receipt) {
	t.Helper()
	path := launch.ReceiptPathFor(f.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, receipt := range receipts {
		line, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runProvenanceReresolution(binary string, fixture provenanceReresolutionFixture) (string, error) {
	cmd := exec.Command(binary, "review-ingest", filepath.Base(fixture.artifact), "--reresolve-provenance")
	cmd.Dir = fixture.lane
	cmd.Env = append(os.Environ(), "HERD_REVIEW_LEDGER="+fixture.ledgerPath)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func candidateRecordRows(t *testing.T, fixture provenanceReresolutionFixture) []reviewledger.LedgerRow {
	t.Helper()
	l, err := reviewledger.NewReadOnlyReviewLedger(fixture.lane, fixture.ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	var out []reviewledger.LedgerRow
	for _, row := range rows {
		if row.Event == string(reviewledger.EventRecord) && row.SHA == fixture.sha && row.Reviewer == "reviewer-a" {
			out = append(out, row)
		}
	}
	return out
}

// Drives the compiled command: an explicitly named historical duplicate is
// resolved only from the project-root receipt log, appended, and read back.
// The retry proves idempotency by refusing a second append.
func TestReviewIngestReResolvesHistoricalProvenanceFromAReachingReceipt(t *testing.T) {
	binary := buildHerd(t)
	fixture := newProvenanceReresolutionFixture(t, "unknown")
	fixture.writeReceipts(t, launch.Receipt{
		CreatedAt: fixture.commitTime.Add(-time.Minute), Accepted: true,
		Branch: fixture.branch, BuilderFamily: "openai", Provider: "codex", Model: "gpt-5.6-sol",
	})

	out, err := runProvenanceReresolution(binary, fixture)
	if err != nil {
		t.Fatalf("re-resolution: %v\n%s", err, out)
	}
	for _, want := range []string{"RERESOLVED_PROVENANCE", "builder_family=openai", "appended=true", "readback=true", "idempotent=false"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	rows := candidateRecordRows(t, fixture)
	if len(rows) != 2 {
		t.Fatalf("record rows=%d, want historical plus one resolved row: %+v", len(rows), rows)
	}
	if rows[0].BuilderFamily != reviewledger.FamilyUnrecorded || rows[0].Gate != reviewledger.GateProvenanceUnrecorded {
		t.Fatalf("historical row was rewritten: %+v", rows[0])
	}

	retryOut, err := runProvenanceReresolution(binary, fixture)
	if err != nil {
		t.Fatalf("idempotent retry: %v\n%s", err, retryOut)
	}
	if !strings.Contains(retryOut, "appended=false") || !strings.Contains(retryOut, "readback=true") || !strings.Contains(retryOut, "idempotent=true") {
		t.Fatalf("retry did not report exact no-op readback:\n%s", retryOut)
	}
	if retryRows := candidateRecordRows(t, fixture); len(retryRows) != len(rows) {
		t.Fatalf("retry grew record history %d -> %d", len(rows), len(retryRows))
	}
}

func TestReviewIngestReResolutionRefusesMissingAmbiguousAndUnknownReceipts(t *testing.T) {
	binary := buildHerd(t)
	for _, tc := range []struct {
		name          string
		writeReceipts func(t *testing.T, fixture provenanceReresolutionFixture)
		want          string
	}{
		{
			name:          "no reaching receipt",
			writeReceipts: func(t *testing.T, fixture provenanceReresolutionFixture) {},
			want:          "receipt_resolution=none",
		},
		{
			name: "ambiguous reaching receipts",
			writeReceipts: func(t *testing.T, fixture provenanceReresolutionFixture) {
				when := fixture.commitTime.Add(-time.Minute)
				fixture.writeReceipts(t,
					launch.Receipt{CreatedAt: when, Accepted: true, Branch: fixture.branch, BuilderFamily: "anthropic"},
					launch.Receipt{CreatedAt: when, Accepted: true, Branch: fixture.branch, BuilderFamily: "openai"},
				)
			},
			want: "receipt_resolution=ambiguous",
		},
		{
			name: "receipt family not allowlisted",
			writeReceipts: func(t *testing.T, fixture provenanceReresolutionFixture) {
				fixture.writeReceipts(t, launch.Receipt{
					CreatedAt: fixture.commitTime.Add(-time.Minute), Accepted: true,
					Branch: fixture.branch, BuilderFamily: "unknown",
				})
			},
			want: `builder_family="unknown" allowlisted=false`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newProvenanceReresolutionFixture(t, "unknown")
			tc.writeReceipts(t, fixture)
			before := candidateRecordRows(t, fixture)
			out, err := runProvenanceReresolution(binary, fixture)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("expected refusal containing %q, err=%v\n%s", tc.want, err, out)
			}
			after := candidateRecordRows(t, fixture)
			if len(after) != len(before) {
				t.Fatalf("refusal mutated record history %d -> %d", len(before), len(after))
			}
		})
	}
}

func TestReviewIngestReResolutionOutputDoesNotExposeReceiptContents(t *testing.T) {
	binary := buildHerd(t)
	fixture := newProvenanceReresolutionFixture(t, "")
	const receiptBodySentinel = "fac630-receipt-secret-sentinel"
	when := fixture.commitTime.Add(-time.Minute)
	fixture.writeReceipts(t,
		launch.Receipt{CreatedAt: when, Accepted: true, Branch: fixture.branch, BuilderFamily: "anthropic", Reason: receiptBodySentinel},
		launch.Receipt{CreatedAt: when, Accepted: true, Branch: fixture.branch, BuilderFamily: "openai", Reason: receiptBodySentinel},
	)
	out, err := runProvenanceReresolution(binary, fixture)
	if err == nil {
		t.Fatal("ambiguous receipts unexpectedly resolved")
	}
	if strings.Contains(out, receiptBodySentinel) {
		t.Fatalf("refusal exposed receipt contents: %s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("builder_families=%q", []string{"anthropic", "openai"})) {
		t.Fatalf("refusal omitted safe observed families: %s", out)
	}
}

func TestReviewIngestReResolutionRefusesANewVerdictWithoutMutation(t *testing.T) {
	binary := buildHerd(t)
	fixture := newProvenanceReresolutionFixture(t, "unknown")
	if err := os.WriteFile(fixture.ledgerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewledger.QueuePathFor(fixture.ledgerPath), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeReceipts(t, launch.Receipt{
		CreatedAt: fixture.commitTime.Add(-time.Minute), Accepted: true,
		Branch: fixture.branch, BuilderFamily: "openai", Provider: "codex", Model: "gpt-5.6-sol",
	})

	out, err := runProvenanceReresolution(binary, fixture)
	if err == nil || !strings.Contains(out, "admission_condition=historical_verdict") || !strings.Contains(out, "duplicate_verdict=false") {
		t.Fatalf("new verdict did not receive the historical-only refusal: err=%v\n%s", err, out)
	}
	if rows := candidateRecordRows(t, fixture); len(rows) != 0 {
		t.Fatalf("historical-only refusal mutated ledger: %+v", rows)
	}
}

// FAC-667's landed artifact shape: a literal Verification heading with no
// evidence beneath it still passes ingest and therefore produces an honest
// empty digest. The compiled readiness surface must refuse that durable row,
// name the digest condition, and never fabricate a digest from other prose.
func TestReviewIngestLiteralVerificationWithoutEvidenceStaysDigestlessAndNotReady(t *testing.T) {
	binary := buildHerd(t)
	fixture := newProvenanceReresolutionFixture(t, "openai")
	if err := os.WriteFile(fixture.ledgerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewledger.QueuePathFor(fixture.ledgerPath), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeReceipts(t, launch.Receipt{
		CreatedAt: fixture.commitTime.Add(-time.Minute), Accepted: true,
		Branch: fixture.branch, BuilderFamily: "openai", Provider: "codex", Model: "gpt-5.6-sol",
	})
	body := "sha: " + fixture.sha + "\n" +
		"branch: " + fixture.branch + "\n" +
		"task: FAC-630\n" +
		"reviewer: reviewer-a\n" +
		"reviewer-family: xai\n" +
		"builder-family: openai\n" +
		"verdict: PASS\n" +
		"reviewed-head: " + fixture.sha + "\n" +
		"---\n" +
		"Verification\n" +
		"Rubric:\n" +
		strings.Repeat("The candidate was inspected carefully, but no executed verification command or outcome was recorded. ", 8) + "\n"
	if err := os.WriteFile(fixture.artifact, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ingest := exec.Command(binary, "review-ingest", filepath.Base(fixture.artifact))
	ingest.Dir = fixture.lane
	ingest.Env = append(os.Environ(), "HERD_REVIEW_LEDGER="+fixture.ledgerPath)
	ingestOut, err := ingest.CombinedOutput()
	if err != nil {
		t.Fatalf("literal Verification artifact did not ingest: %v\n%s", err, ingestOut)
	}
	if !strings.Contains(string(ingestOut), "ADMITTED") {
		t.Fatalf("artifact was not admitted by the shipped ingest path:\n%s", ingestOut)
	}

	l, err := reviewledger.NewReadOnlyReviewLedger(fixture.lane, fixture.ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	verdictFound := false
	for _, row := range rows {
		if row.Event == string(reviewledger.EventVerdict) && row.SHA == fixture.sha && row.Reviewer == "reviewer-a" {
			verdictFound = true
			if row.VerificationDigest != "" {
				t.Fatalf("ingest fabricated verification_digest=%q from non-evidence prose", row.VerificationDigest)
			}
		}
	}
	if !verdictFound {
		t.Fatal("ingest produced no verdict row")
	}

	readiness := exec.Command(binary, "review-ledger", "readiness", fixture.sha)
	readiness.Dir = fixture.lane
	readiness.Env = append(os.Environ(), "HERD_REVIEW_LEDGER="+fixture.ledgerPath)
	readinessOut, err := readiness.CombinedOutput()
	if err == nil {
		t.Fatalf("digestless verdict reported ready:\n%s", readinessOut)
	}
	if strings.Contains(string(readinessOut), `"ready":true`) {
		t.Fatalf("digestless verdict reported ready:\n%s", readinessOut)
	}
	if !strings.Contains(string(readinessOut), `verification_digest=\"\"`) {
		t.Fatalf("readiness did not name the exact missing digest condition:\n%s", readinessOut)
	}
}
