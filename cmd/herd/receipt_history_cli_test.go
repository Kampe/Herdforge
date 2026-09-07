package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

func TestHarvestVerifyLandedReceiptHistoryCLI(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	remote := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		b, e := testgit.Command(root, args...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if e := os.MkdirAll(filepath.Dir(p), 0o755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte(body), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	git("init", "-q", "-b", "work")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "test")
	git("init", "--bare", "-q", remote)
	git("remote", "add", "origin", remote)
	write(".gitignore", ".herd/\n")
	git("add", ".")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	write(".herd/herd.yaml", "version: \"1\"\nproject:\n  name: test\ntask_provider:\n  type: memory\nmerge_policy:\n  protected: true\n  required_checks: [gate]\n  require_different_family_review: true\n  require_pull_request_reviews: true\n  remote_ci:\n    required: true\n    required_checks: [gate]\n")
	ledgerPath := filepath.Join(root, ".herd/review-ledger.jsonl")
	ledger, e := reviewledger.NewReviewLedger(root, ledgerPath)
	if e != nil {
		t.Fatal(e)
	}
	admit := func(sha, reviewer string, v reviewledger.Verdict) {
		t.Helper()
		if e := ledger.Record(reviewledger.RecordOpts{SHA: sha, Reviewer: reviewer, BuilderFamily: "openai", ReviewerFamily: "google", Gate: "independent", Tier: "R2", Task: "FAC-761", Lease: "fixture-lease"}); e != nil {
			t.Fatal(e)
		}
		if _, e := ledger.Verdict(reviewledger.VerdictOpts{SHA: sha, CandidateSHA: sha, Reviewer: reviewer, BuilderFamily: "openai", ReviewerFamily: "google", Task: "FAC-761", Verdict: v, Artifact: "fixture.md", VfyDigest: "executed-fixture-checks", Lease: "fixture-lease", PatchURL: "fixture-patch"}); e != nil {
			t.Fatal(e)
		}
	}
	run := func(sha, base, prior string) ([]byte, error) {
		args := []string{"harvest-merge", "fixture", "--branch", "work", "--verify-landed", "--ref", "FAC-761", "--candidate", sha, "--base-sha", base, "--pr", "1"}
		if prior != "" {
			args = append(args, "--supersedes-receipt", prior)
		}
		c := exec.Command(binary, args...)
		c.Dir = root
		c.Env = append(os.Environ(), "HERD_PROJECT_ROOT="+root, "HERD_CANONICAL_ROOT="+root, "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath)
		return c.CombinedOutput()
	}
	write("first.txt", "first\n")
	git("add", ".")
	git("commit", "-qm", "first")
	first := git("rev-parse", "HEAD")
	git("push", "-q", "origin", "HEAD:main")
	admit(first, "review-first", reviewledger.VerdictPASS)
	if out, e := run(first, base, ""); e != nil {
		t.Fatalf("first receipt: %v %s", e, out)
	}
	receiptPath := hsync.ReceiptPath(root, "FAC-761")
	old, e := hsync.LoadReceipt(receiptPath)
	if e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(receiptPath)
	if e != nil {
		t.Fatal(e)
	}
	write("second.txt", "second\n")
	git("add", ".")
	git("commit", "-qm", "second")
	second := git("rev-parse", "HEAD")
	git("push", "-q", "origin", "HEAD:main")
	admit(second, "review-second", reviewledger.VerdictPASS)
	for _, prior := range []string{"", strings.Repeat("a", 64)} {
		if out, e := run(second, first, prior); e == nil {
			t.Fatalf("unapproved replacement accepted: %s", out)
		}
	}
	unchanged, _ := os.ReadFile(receiptPath)
	if !bytes.Equal(before, unchanged) {
		t.Fatal("refusal replaced prior receipt")
	}
	if out, e := run(second, first, old.Digest); e != nil {
		t.Fatalf("follow-up: %v %s", e, out)
	}
	if out, e := run(second, first, old.Digest); e != nil {
		t.Fatalf("replay: %v %s", e, out)
	}
	now, e := hsync.LoadReceipt(receiptPath)
	if e != nil || now.CandidateSHA != second {
		t.Fatalf("wrong current: %+v %v", now, e)
	}
	archived, e := os.ReadFile(filepath.Join(root, ".herd/receipts/history/FAC-761", old.Digest+".json"))
	if e != nil || !bytes.Equal(archived, before) {
		t.Fatalf("prior history changed: %v", e)
	}
	admit(second, "review-dissent", reviewledger.VerdictFAIL)
	if out, e := run(second, first, old.Digest); e == nil {
		t.Fatalf("dissent ignored: %s", out)
	}
	if _, e := os.Stat(filepath.Join(root, ".herd/board-done.jsonl")); !os.IsNotExist(e) {
		t.Fatal("receipt command touched board completion")
	}
}
