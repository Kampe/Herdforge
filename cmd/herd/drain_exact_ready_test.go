package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/readyindex"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-603: default runDrainCommand must take the exact-ready index path, not
// harvest-scan. Revert proof: remove the !doRepair branch and this fails.
func TestDrainDefaultUsesExactReadyIndexNotHarvestScan(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main")
	runGitT(t, root, "config", "user.email", "fac603@test")
	runGitT(t, root, "config", "user.name", "fac603")
	runGitT(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	runGitT(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	ledger := filepath.Join(root, ".herd", "review-ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", ledger)
	t.Setenv("HERD_STATE_DIR", filepath.Join(root, "state"))

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var out, errOut bytes.Buffer
	code := runDrainCommand([]string{"--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "phase=exact-ready-index") {
		t.Fatalf("default drain must use exact-ready index; stderr=%s", stderr)
	}
	if strings.Contains(stderr, "phase=harvest-scan") {
		t.Fatalf("default drain must not full harvest-scan; stderr=%s", stderr)
	}
}

// FAC-603: --repair keeps the full scan and rebuilds the projection.
func TestDrainRepairUsesHarvestScanAndRebuildsIndex(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main")
	runGitT(t, root, "config", "user.email", "fac603@test")
	runGitT(t, root, "config", "user.name", "fac603")
	runGitT(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	runGitT(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	ledger := filepath.Join(root, ".herd", "review-ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", ledger)
	t.Setenv("HERD_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("HERD_DRAIN_TIMEOUT", "30s")
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "30s")

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var out, errOut bytes.Buffer
	code := runDrainCommand([]string{"--repair", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, errOut.String(), out.String())
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "phase=harvest-scan") {
		t.Fatalf("--repair must harvest-scan; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "exact-ready index rebuilt") {
		t.Fatalf("--repair must rebuild exact-ready index; stderr=%s", stderr)
	}
	idxPath := readyindex.PathFor(ledger)
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("repair must write projection: %v", err)
	}
}

// FAC-603: PASS verdict side-writes the exact-ready projection; Consumed clears it.
func TestVerdictIngestUpdatesExactReadyIndex(t *testing.T) {
	root := t.TempDir()
	ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
	l, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: "feat/x", Reviewer: "reviewer-1",
		ReviewerFamily: "openai", Provider: "openai", Model: "m",
		Artifact: "art", Gate: "mechanical", Task: "FAC-603",
	}); err != nil {
		t.Fatal(err)
	}
	enqueued, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: "reviewer-1", Verdict: reviewledger.VerdictPASS,
		Artifact: "art", ReviewerFamily: "openai", Branch: "feat/x", Lane: "lane-a",
		Task: "FAC-603",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enqueued {
		t.Fatal("PASS must enqueue")
	}
	entries, err := readyindex.List(readyindex.PathFor(ledgerPath))
	if err != nil {
		t.Fatalf("index after PASS: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.SHA == sha {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("PASS verdict must upsert exact-ready entry; got %+v", entries)
	}
	if err := l.Consumed(sha, strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	entries, err = readyindex.List(readyindex.PathFor(ledgerPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.SHA == sha {
			t.Fatal("Consumed must remove exact-ready entry")
		}
	}
}
