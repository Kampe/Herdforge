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
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-631: review-ingest is the normal path from a reviewer artifact to the
// durable ledger. Receipt reconciliation requires a tier on that row, so the
// shipped ingest command must classify the exact candidate before recording it.
func TestReviewIngestRiskTierReachesReceiptReconcile(t *testing.T) {
	binary := buildHerd(t)
	tests := []struct {
		name     string
		path     string
		wantTier string
	}{
		{name: "documentation", path: "docs/fac-631.md", wantTier: "R0"},
		{name: "control plane", path: "cmd/herd/fac631.go", wantTier: "R3"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
			git("remote", "add", "origin", "https://example.invalid/herdforge.git")
			git("commit", "-q", "--allow-empty", "-m", "base")
			base := strings.TrimSpace(runOutput(t, root, "rev-parse", "HEAD"))
			git("update-ref", "refs/remotes/origin/main", base)

			branch := "fac-631-" + strings.ReplaceAll(tc.name, " ", "-")
			git("checkout", "-q", "-b", branch)
			candidatePath := filepath.Join(root, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(candidatePath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(candidatePath, []byte("FAC-631 candidate\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", tc.path)
			git("commit", "-q", "-m", "candidate")
			sha := strings.TrimSpace(runOutput(t, root, "rev-parse", "HEAD"))
			commitISO := strings.TrimSpace(runOutput(t, root, "show", "-s", "--format=%cI", sha))
			commitTime, err := time.Parse(time.RFC3339, commitISO)
			if err != nil {
				t.Fatalf("parse commit time %q: %v", commitISO, err)
			}

			if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
				t.Fatal(err)
			}
			receipt := fmt.Sprintf(
				`{"created_at":%q,"task_ref":"FAC-631","lane":%q,"provider":"claude","model":"claude-sonnet-5","builder_family":"anthropic","branch":%q,"accepted":true}`,
				commitTime.Add(-time.Minute).UTC().Format(time.RFC3339), branch, branch)
			if err := os.WriteFile(filepath.Join(root, ".herd", "launch-receipts.jsonl"), []byte(receipt+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			body := "Verification: go test ./... passed for the exact reviewed commit.\n" +
				strings.Repeat("The independent review checked the candidate behavior and its failure path. ", 6)
			artifact := "sha: " + sha + "\n" +
				"branch: " + branch + "\n" +
				"task: FAC-631\n" +
				"reviewer: independent-reviewer\n" +
				"reviewer-family: openai\n" +
				"verdict: PASS\n" +
				"reviewed-base: " + base + "\n" +
				"reviewed-head: " + sha + "\n" +
				"---\n" + body + "\n"
			if err := os.WriteFile(filepath.Join(root, "verdict.md"), []byte(artifact), 0o600); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(binary, "review-ingest", "verdict.md")
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("review-ingest: %v: %s", err, out)
			}

			ledger, err := reviewledger.NewReviewLedger(root, reviewledger.DefaultPath(root))
			if err != nil {
				t.Fatalf("open ledger: %v", err)
			}
			rows, err := ledger.AllRows()
			if err != nil {
				t.Fatalf("read ledger: %v", err)
			}
			var gotTier string
			for _, row := range rows {
				if row.Event == string(reviewledger.EventRecord) && row.SHA == sha {
					gotTier = row.Tier
				}
			}
			if gotTier != tc.wantTier {
				t.Fatalf("ingested record tier = %q, want %q; deleting the shipped tier write must fail here\nrows: %+v", gotTier, tc.wantTier, rows)
			}

			gate := &mergeadmit.Gate{
				RepoDir: root,
				Ledger:  ledger,
				Policy:  preflight.DefaultProtectedPolicy(),
				Live:    mergeadmit.LiveState{OriginMain: mergeadmit.StaticProbe(sha)},
			}
			completed, err := gate.ReconcileLanded(mergeadmit.Request{
				Ref:          "FAC-631",
				CandidateSHA: sha,
				BaseSHA:      base,
				ReducedProvenance: &mergeadmit.ReducedProvenance{
					PullRequest:  631,
					VerifyLanded: true,
				},
			})
			if err != nil {
				t.Fatalf("receipt reconcile refused the normally ingested candidate: %v", err)
			}
			if completed.RiskTier != tc.wantTier {
				t.Fatalf("completion receipt tier = %q, want %q", completed.RiskTier, tc.wantTier)
			}
		})
	}
}
