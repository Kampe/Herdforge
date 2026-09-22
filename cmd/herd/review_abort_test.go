package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func writeAbortManifest(t *testing.T, dir string) (string, herdr.ReviewRetirementManifest) {
	t.Helper()
	m := herdr.NewReviewRetirementManifest(time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC), herdr.ReviewRetirementManifest{
		Repository: "herdforge", TaskRef: "FAC-855", TaskID: "task-855",
		CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Branch: "review/FAC-855",
		Worktree: ".herd/reviews/fac-855", Pool: ".herd/pool", Slot: "pool-99", LeaseGeneration: 7,
		Workspace: "wK", TabID: "wK:tABORTTEST", PaneID: "wK:pABORTTEST", TerminalID: "term-abort-test", SessionID: "sess-abort-fixture",
		Reviewer: "review-abort-fixture", ReviewerFamily: "google", ReviewerModel: "gemini-3.1-pro-high",
		PromptArtifact: ".herd/review/prompts/fac-855.md", Generation: "pool-99-1", Nonce: "pool-99-1",
	})
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, m
}

func abortCLIEnv(ledgerPath string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HERD_REVIEW_LEDGER=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "HERD_REVIEW_LEDGER="+ledgerPath)
}

func TestReviewAbortCLI_SessionMismatch(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	path, _ := writeAbortManifest(t, dir)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	cmd := exec.Command(binary, "review-abort", "--manifest", path, "--session", "other-session", "--reason", "quota-exhausted")
	cmd.Dir = dir
	cmd.Env = abortCLIEnv(ledgerPath)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "session mismatch") {
		t.Fatalf("want session mismatch, got err=%v\n%s", err, out)
	}
}

func TestReviewAbortCLI_DryRunDoesNotWriteLedger(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	path, m := writeAbortManifest(t, dir)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	cmd := exec.Command(binary, "review-abort", "--manifest", path, "--session", m.SessionID, "--reason", "quota-exhausted", "--dry-run")
	cmd.Dir = dir
	cmd.Env = abortCLIEnv(ledgerPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if _, err := os.Stat(ledgerPath); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create a ledger")
	}
}

func TestReviewAbortCLI_ActWritesAbortNotVerdict(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	path, m := writeAbortManifest(t, dir)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	cmd := exec.Command(binary, "review-abort", "--manifest", path, "--session", m.SessionID, "--reason", "quota-exhausted", "--act")
	cmd.Dir = dir
	cmd.Env = abortCLIEnv(ledgerPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("act: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"event":"coordinator-abort"`) || strings.Contains(string(raw), `"event":"verdict"`) {
		t.Fatalf("act must write coordinator-abort only, got %s", raw)
	}
	q := reviewledger.QueuePathFor(ledgerPath)
	if data, err := os.ReadFile(q); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("act must not enqueue harvest, queue=%s", data)
	}
}

func TestReviewAbortCLI_ActRefusesPASS(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	path, m := writeAbortManifest(t, dir)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := reviewledger.NewReviewLedger(dir, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: m.CandidateSHA, CandidateSHA: m.CandidateSHA, Reviewer: m.Reviewer,
		Verdict: reviewledger.VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai",
		ArtifactDigest: strings.Repeat("c", 64),
	}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "review-abort", "--manifest", path, "--session", m.SessionID, "--reason", "quota-exhausted", "--act")
	cmd.Dir = dir
	cmd.Env = abortCLIEnv(ledgerPath)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "existing PASS") {
		t.Fatalf("want PASS refuse, got err=%v\n%s", err, out)
	}
}
