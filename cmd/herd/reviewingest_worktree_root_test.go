package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-625 defect 1: `herd review-ingest` resolved the receipt log relative to
// the process cwd. Run from a LANE WORKTREE (the shipped call shape: a
// reviewer's verdict tool runs from wherever it was launched, not the project
// root), that read the worktree's own empty .herd/launch-receipts.jsonl,
// found no receipt reaching the candidate, and recorded provenance as if none
// existed -- even though a qualifying receipt sat one hop away at the project
// root the whole time.
//
// This drives the SHIPPED `review-ingest` binary from a lane worktree cwd and
// asserts on the ledger ROW it wrote, never on stdout: a helper-only test
// (pkg/reviewingest/provenance_test.go) already proves the join logic itself,
// but proved nothing about which receipt log the production command actually
// opens -- exactly the "helper is correct, production caller reads the wrong
// path" shape this card exists to close.
func TestReviewIngestResolvesReceiptsFromProjectRootInAWorktree(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git(root, "init", "-q", "-b", "main")
	git(root, "config", "user.email", "fac625@test.invalid")
	git(root, "config", "user.name", "fac625")
	git(root, "commit", "-q", "--allow-empty", "-m", "base")
	// ValidatePassDiff needs a real "origin/main" ref to diff the candidate
	// against; a local branch of that literal name is sufficient.
	git(root, "branch", "origin/main")

	lane := filepath.Join(root, ".herd", "worktrees", "fac-625-lane")
	git(root, "worktree", "add", "-q", "-b", "fac-625-lane", lane)
	if err := os.WriteFile(filepath.Join(lane, "change.txt"), []byte("fac-625\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(lane, "add", "change.txt")
	git(lane, "commit", "-q", "-m", "fac-625 change")
	sha := strings.TrimSpace(runOutput(t, lane, "rev-parse", "HEAD"))
	commitISO := strings.TrimSpace(runOutput(t, lane, "show", "-s", "--format=%cI", sha))
	commitTime, err := time.Parse(time.RFC3339, commitISO)
	if err != nil {
		t.Fatalf("parse commit time %q: %v", commitISO, err)
	}

	// A launch receipt at the PROJECT ROOT, naming the lane branch, recorded
	// before the commit -- exactly what BuilderFamilyReachingSHA requires to
	// prove provenance.
	receiptLine := fmt.Sprintf(
		`{"created_at":%q,"task_ref":"FAC-625","lane":"fac-625-lane","provider":"claude","model":"claude-sonnet-5","builder_family":"anthropic","branch":"fac-625-lane","accepted":true}`,
		commitTime.Add(-time.Minute).UTC().Format(time.RFC3339))
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "launch-receipts.jsonl"), []byte(receiptLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := strings.Repeat("Independently verified the fix end to end against the reviewed commit. ", 6)
	artifact := "sha: " + sha + "\n" +
		"branch: fac-625-lane\n" +
		"task: FAC-625\n" +
		"reviewer: independent-reviewer\n" +
		"reviewer-family: openai\n" +
		"verdict: PASS\n" +
		"reviewed-head: " + sha + "\n" +
		"---\n" + body + "\n"
	artifactPath := filepath.Join(lane, "verdict.md")
	if err := os.WriteFile(artifactPath, []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "review-ingest", "verdict.md")
	cmd.Dir = lane // the shipped call shape: ingest runs FROM the lane worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("review-ingest: %v: %s", err, out)
	}

	ledger, err := reviewledger.NewReadOnlyReviewLedger(lane, reviewledger.DefaultPath(lane))
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
	if record.BuilderFamily != "anthropic" {
		t.Fatalf("record builder_family = %q, want anthropic resolved from the project-root receipt "+
			"(ingest ran from a lane worktree cwd); got ledger rows: %+v\ncommand output: %s",
			record.BuilderFamily, rows, out)
	}
	if record.Gate == reviewledger.GateProvenanceUnrecorded {
		t.Fatalf("gate = %s; a receipt reaching the exact commit proved the family and must not be "+
			"downgraded to unrecorded", record.Gate)
	}
}

func runOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := testgit.Command(dir, args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
