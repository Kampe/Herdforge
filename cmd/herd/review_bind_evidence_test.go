package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

func TestParseReviewBindEvidenceArgs_AcceptsRefThenFlags(t *testing.T) {
	t.Parallel()
	got, err := parseReviewBindEvidenceArgs([]string{
		"FAC-618", "--candidate", "46be267dd2cc0a42acb70141838d0e3f5645605b",
		"--receipt", "sha256:fc246f616cafc394432b8f291ef8c2940826b9c1b7826f1a613db4c61a48de82",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Task != "FAC-618" || got.Candidate != "46be267dd2cc0a42acb70141838d0e3f5645605b" {
		t.Fatalf("parsed %+v", got)
	}
	if !strings.HasPrefix(got.Receipt, "sha256:fc246f616cafc394") {
		t.Fatalf("receipt %q", got.Receipt)
	}
}

func TestParseReviewBindEvidenceArgs_RefusesCorpusAndMissing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "sweep", args: []string{"FAC-618", "--candidate", "abc", "--receipt", "sha256:dead", "--sweep"}, want: "corpus"},
		{name: "corpus flag", args: []string{"--corpus", "FAC-618", "--candidate", "abc", "--receipt", "sha256:dead"}, want: "corpus"},
		{name: "missing ref", args: []string{"--candidate", "abc", "--receipt", "sha256:dead"}, want: "usage"},
		{name: "extra positional", args: []string{"FAC-618", "FAC-619", "--candidate", "abc", "--receipt", "sha256:dead"}, want: "usage"},
		{name: "missing candidate", args: []string{"FAC-618", "--receipt", "sha256:dead"}, want: "usage"},
		{name: "missing receipt", args: []string{"FAC-618", "--candidate", "abc"}, want: "usage"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseReviewBindEvidenceArgs(tc.args)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("args %v: err=%v want %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestReviewBindEvidenceCLI_ExactPairAndRefusals(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := testgit.Command(root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "fac751@test.invalid")
	git("config", "user.name", "fac751")
	git("commit", "-q", "--allow-empty", "-m", "base")

	sha := "46be267dd2cc0a42acb70141838d0e3f5645605b"
	ledgerDir := filepath.Join(root, ".herd")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(map[string]any{
		"ts": "2026-09-05T23:30:00.000000Z", "event": "record", "sha": sha,
		"reviewer": "pool-02", "builder_family": "openai", "builder_identity": "builder-openai",
		"reviewer_family": "anthropic", "tier": "R2", "task": "FAC-618",
	})
	verdict, _ := json.Marshal(map[string]any{
		"ts": "2026-09-05T23:32:46.874718Z", "event": "verdict", "sha": sha, "reviewer": "pool-02",
		"verdict": "PASS", "builder_family": "openai", "reviewer_family": "anthropic", "task": "FAC-618",
	})
	ledgerPath := filepath.Join(ledgerDir, "review-ledger.jsonl")
	original := string(record) + "\n" + string(verdict) + "\n"
	if err := os.WriteFile(ledgerPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ledgerDir, "harvest-queue.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	receipt := verifier.Receipt{
		Version: 1, TaskRef: "FAC-618", LeaseGeneration: "2", CandidateSHA: sha,
		Command:           []string{"go", "test", "-timeout=30m0s", "./..."},
		EnvironmentPolicy: verifier.EnvironmentPolicyInherited, Outcome: verifier.OutcomePASS,
	}
	receipt.Digest = receipt.ComputeDigest()
	store, err := verifier.NewFileReceiptStore(filepath.Join(root, defaultReceiptDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = root
		cmd.Stdin = nil
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("review-bind-evidence", "FAC-618", "--candidate", sha, "--receipt", receipt.Digest)
	if err != nil {
		t.Fatalf("legitimate bind: %v\n%s", err, out)
	}
	body, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), original) {
		t.Fatal("CLI bind rewrote historical rows")
	}
	l, err := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[2].Event != string(reviewledger.EventEvidenceBind) {
		t.Fatalf("rows=%d last=%+v", len(rows), rows)
	}
	q, err := os.ReadFile(filepath.Join(ledgerDir, "harvest-queue.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 0 {
		t.Fatalf("binding wrote queue: %s", q)
	}

	mid := append([]byte(nil), body...)
	out, err = run("review-bind-evidence", "FAC-618", "--candidate", sha, "--receipt", receipt.Digest)
	if err != nil {
		t.Fatalf("idempotent retry: %v\n%s", err, out)
	}
	again, _ := os.ReadFile(ledgerPath)
	if string(again) != string(mid) {
		t.Fatal("idempotent CLI retry wrote")
	}

	before := string(mid)
	out, err = run("review-bind-evidence", "FAC-618", "--candidate", sha, "--receipt", receipt.Digest, "--sweep")
	if err == nil || !strings.Contains(strings.ToLower(out+err.Error()), "corpus") {
		t.Fatalf("sweep must refuse, got err=%v out=%s", err, out)
	}
	afterSweep, _ := os.ReadFile(ledgerPath)
	if string(afterSweep) != before {
		t.Fatal("sweep wrote")
	}

	missing := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	out, err = run("review-bind-evidence", "FAC-618", "--candidate", sha, "--receipt", missing)
	if err == nil {
		t.Fatalf("missing receipt must refuse, out=%s", out)
	}
	afterMissing, _ := os.ReadFile(ledgerPath)
	if string(afterMissing) != before {
		t.Fatal("missing receipt wrote")
	}

	corruptDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	corruptPath := filepath.Join(root, defaultReceiptDir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json")
	if err := os.WriteFile(corruptPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = run("review-bind-evidence", "FAC-618", "--candidate", sha, "--receipt", corruptDigest)
	if err == nil {
		t.Fatalf("corrupt receipt must refuse, out=%s", out)
	}
	afterCorrupt, _ := os.ReadFile(ledgerPath)
	if string(afterCorrupt) != before {
		t.Fatal("corrupt receipt wrote")
	}

	prose := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := os.WriteFile(filepath.Join(root, "NOTES.md"), []byte("# Tests\nall pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = run("review-bind-evidence", "FAC-618", "--candidate", sha, "--receipt", prose)
	if err == nil {
		t.Fatalf("prose-only digest must refuse, out=%s", out)
	}
	afterProse, _ := os.ReadFile(ledgerPath)
	if string(afterProse) != before {
		t.Fatal("prose-only wrote")
	}
}
