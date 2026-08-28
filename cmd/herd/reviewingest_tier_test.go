package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/classify"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-631: Admit reads its risk tier ONLY from an EventRecord row's Tier
// field (Ledger.Tier), and review-ingest never wrote one -- only
// herd review-classify's dispatch-time write did. Live evidence (CHA-3465,
// 0591cbac): builder_family, reviewer_family and verification_digest were all
// present and correct, and admission still could not reach the candidate,
// because tier="" on every row ingest had written.
//
// This drives the SHIPPED `review-ingest` binary end to end and asserts on
// the ledger ROW it wrote -- never on stdout -- that the record row now
// carries the SAME deterministic tier herd review-classify would have
// computed for this diff, so admission's `if tier == "" { continue }` can
// actually be reached.
func TestReviewIngestRecordsADeterministicRiskTier(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := testgit.Command(root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "fac631@test.invalid")
	git("config", "user.name", "fac631")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("branch", "origin/main")

	changed := "pkg/widget/widget.go"
	if err := os.MkdirAll(filepath.Join(root, "pkg", "widget"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, changed), []byte("package widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", changed)
	git("commit", "-q", "-m", "add widget")
	sha := strings.TrimSpace(runOutput(t, root, "rev-parse", "HEAD"))

	// The SAME classifier review-ingest must now use, computed independently
	// here from the same paths, so this test does not assume a specific rule
	// fired -- only that ingest and review-classify agree.
	want := string(classify.Classify(classify.Input{CandidateSHA: sha, Paths: []string{changed}}).Tier)
	if want == "" {
		t.Fatal("test fixture: classifier produced no tier at all; fixture is broken")
	}

	body := strings.Repeat("Independently verified the fix end to end against the reviewed commit. ", 6)
	artifact := "sha: " + sha + "\n" +
		"branch: main\n" +
		"task: FAC-631\n" +
		"reviewer: independent-reviewer\n" +
		"reviewer-family: openai\n" +
		"builder-family: anthropic\n" +
		"verdict: PASS\n" +
		"reviewed-head: " + sha + "\n" +
		"---\n" + body + "\n"
	artifactPath := filepath.Join(root, "verdict.md")
	if err := os.WriteFile(artifactPath, []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "review-ingest", "verdict.md")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("review-ingest: %v: %s", err, out)
	}

	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, reviewledger.DefaultPath(root))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	rows, err := ledger.AllRows()
	if err != nil {
		t.Fatalf("read ledger rows: %v", err)
	}
	var record *reviewledger.LedgerRow
	for i := range rows {
		if rows[i].SHA == sha && rows[i].Event == string(reviewledger.EventRecord) {
			record = &rows[i]
		}
	}
	if record == nil {
		t.Fatalf("no record row for %s was admitted: %s", sha, out)
	}
	if record.Tier != want {
		t.Fatalf("record.tier = %q, want %q (the tier review-classify would compute for the same diff); "+
			"ledger rows: %+v\ncommand output: %s", record.Tier, want, rows, out)
	}

	// Reachability, not just presence: Ledger.Tier is what Admit's
	// `if tier == "" { continue }` actually calls.
	reachable, err := ledger.Tier(sha)
	if err != nil {
		t.Fatalf("Tier(%s): %v", sha, err)
	}
	if reachable == "" {
		t.Fatalf("Ledger.Tier(%s) returned empty; admission remains structurally unreachable for this candidate", sha)
	}
}

// A RETIRED artifact carries no PASS verdict to admit and must not be forced
// through the classifier at all -- Retire builds its own opts and never
// touches RecordOpts.Tier.
func TestReviewIngestRetiredArtifactDoesNotRequireATier(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := testgit.Command(root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "fac631@test.invalid")
	git("config", "user.name", "fac631")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("branch", "origin/main")
	sha := strings.TrimSpace(runOutput(t, root, "rev-parse", "HEAD"))

	body := strings.Repeat("This branch is settled and superseded; retiring without an independent read. ", 4)
	artifact := "sha: " + sha + "\n" +
		"branch: main\n" +
		"authority: herdforge-orchestrator\n" +
		"verdict: RETIRED\n" +
		"---\n" + body + "\n"
	artifactPath := filepath.Join(root, "retire.md")
	if err := os.WriteFile(artifactPath, []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "review-ingest", "retire.md")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("review-ingest: %v: %s", err, out)
	}
}
