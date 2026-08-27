package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-625: FAC-620 wired provenance reconciliation into ingest using
// launch.DefaultReceiptPath(), which is CWD-RELATIVE -- the exact defect FAC-646
// had already fixed one function away, in builderFamilyFromReceipts.
//
// Live consequence on Chainseer: the CHA-3211 candidate had a real launch
// receipt, but ingest ran from a lane worktree, read that worktree's receipt log
// instead of the project's, found nothing reaching the commit, and recorded
// provenance_unrecorded=1. A PASS was rendered unmergeable by a path bug.
//
// This drives the SHIPPED command with cwd set to a LANE WORKTREE while the
// receipt lives at the project root. A test run from the root would pass with
// the bug present, which is how the defect survived FAC-620's review.

// TestIngestReadsTheReceiptLogFromTheProjectRootNotTheCwd is red when
// runReviewIngest resolves the receipt log relative to the process cwd.
func TestIngestReadsTheReceiptLogFromTheProjectRootNotTheCwd(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()

	git := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git(root, "init", "-q", "-b", "main")
	git(root, "commit", "-q", "--allow-empty", "-m", "base")
	git(root, "update-ref", "refs/remotes/origin/main", "HEAD")

	// A candidate on a lane branch, in a real linked worktree -- the shape the
	// live fleet uses and the cwd ingest actually ran from.
	lane := filepath.Join(root, "lane-wt")
	git(root, "worktree", "add", "-q", "-b", "wt/lane", lane, "HEAD")
	if err := os.WriteFile(filepath.Join(lane, "candidate.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(lane, "add", "candidate.txt")
	git(lane, "commit", "-q", "-m", "candidate")
	sha := git(lane, "rev-parse", "HEAD")

	// The receipt lives at the PROJECT root, written before the commit.
	created, err := time.Parse(time.RFC3339, git(lane, "show", "-s", "--format=%cI", sha))
	if err != nil {
		t.Fatal(err)
	}
	receipt := map[string]any{
		"created_at": created.Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		"task_ref":   "lane", "lane": "lane", "branch": "wt/lane",
		"provider": "claude", "model": "claude-sonnet-5",
		"builder_family": "anthropic", "accepted": true,
	}
	line, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "launch-receipts.jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// The verdict states NO family, so the only way the ledger can carry
	// "anthropic" is if ingest found the project-root receipt.
	inbox := filepath.Join(root, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "sha: " + sha + "\nbranch: wt/lane\ntask: FAC-625\n" +
		"reviewer: review-receipt-root\nreviewer-family: openai\n" +
		"verdict: PASS\nreviewed-head: " + sha + "\n---\n" +
		strings.Repeat("Body long enough to clear the minimum-length gate. ", 12) + "\n"
	if err := os.WriteFile(filepath.Join(inbox, "sha-review-receiptroot.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// THE point of the test: run from the LANE WORKTREE, not the project root.
	cmd := exec.Command(binary, "review-ingest", "--sweep")
	cmd.Dir = lane
	cmd.Env = append(os.Environ(), "HERD_ROOT="+lane, "HERD_REPO_ROOT="+lane)
	out, _ := cmd.CombinedOutput()

	// The ledger's own root is a separate question from the receipt log's, so
	// accept either location here: this test is about which RECEIPT log was
	// read, and conflating the two would make it fail for the wrong reason.
	ledger := ""
	for _, p := range []string{
		filepath.Join(root, ".herd", "review-ledger.jsonl"),
		filepath.Join(lane, ".herd", "review-ledger.jsonl"),
	} {
		if b, rerr := os.ReadFile(p); rerr == nil && len(strings.TrimSpace(string(b))) > 0 {
			ledger = string(b)
			break
		}
	}
	if ledger == "" {
		t.Fatalf("no ledger row written from a lane cwd\ningest output:\n%s", out)
	}

	if !strings.Contains(ledger, `"builder_family":"anthropic"`) {
		t.Fatalf("ingest run from a lane worktree did not resolve the PROJECT-root receipt.\n"+
			"The candidate's provenance exists and reaches the commit, so recording it as "+
			"unrecorded makes a passing review unmergeable -- the live CHA-3211 failure.\n"+
			"ledger:\n%s\ningest output:\n%s", ledger, out)
	}
	if strings.Contains(ledger, "unrecorded") {
		t.Fatalf("provenance was recorded as unrecorded despite a reaching receipt:\n%s", ledger)
	}
}
