package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestFAC763AckOnlyRecoversWithoutVerdictOrPoolMutation(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	for _, key := range []string{"HERD_ROOT", "HERD_REPO_ROOT", "HERD_PROJECT_ROOT", "HERD_CANONICAL_ROOT"} {
		t.Setenv(key, root)
	}
	t.Setenv("HERD_REVIEW_LEDGER", filepath.Join(root, ".herd", "review-ledger.jsonl"))
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(root, ".herd", "launch-receipts.jsonl"))
	t.Setenv("HERD_REVIEW_ROOT", "")
	sha := strings.Repeat("a", 40)
	reviewer := "review-fac-763"
	body := []byte("sha: " + sha + "\nbranch: fix/fac763\ntask: FAC-763\nreviewer: " + reviewer + "\nreviewer-family: anthropic\nbuilder-family: openai\nverdict: PASS\nreviewed-head: " + sha + "\n---\n## Tests run\nAlready performed verification.\n")
	rel := ".herd/review/inbox/retained.md"
	artifact := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(artifact), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, body, 0600); err != nil {
		t.Fatal(err)
	}
	row := reviewledger.LedgerRow{Event: "verdict", SHA: sha, Reviewer: reviewer, Artifact: rel, ArtifactDigest: reviewack.ArtifactDigest(body), Task: "FAC-763", Verdict: "PASS", BuilderFamily: "openai", ReviewerFamily: "anthropic"}
	ledgerBytes, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	ledgerBytes = append(ledgerBytes, '\n')
	ledgerPath := os.Getenv("HERD_REVIEW_LEDGER")
	if err := os.WriteFile(ledgerPath, ledgerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	// The legacy ack is for the prior artifact; no mutation may discard it.
	if err := reviewack.Emit(root, reviewack.Ack{SHA: sha, Reviewer: reviewer, ArtifactDigest: reviewack.ArtifactDigest([]byte("prior"))}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"review-ingest"}, args...)...)
		cmd.Dir = root
		return cmd.CombinedOutput()
	}
	if out, err := run("--ack-only", "--dry-run", artifact); err != nil {
		t.Fatalf("dry-run: %v %s", err, out)
	}
	if out, err := run("--ack-only", "--dry-run", "--json", artifact); err != nil {
		t.Fatalf("dry-run JSON: %v %s", err, out)
	} else {
		var result struct {
			Admitted int `json:"admitted"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		if result.Admitted != 0 {
			t.Fatalf("ack recovery claims new admission: %s", out)
		}
	}
	ackPath := reviewack.ArtifactPath(root, sha, reviewer, row.ArtifactDigest)
	if _, err := os.Stat(ackPath); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote ack", err)
	}
	for i := 0; i < 2; i++ {
		if out, err := run("--ack-only", artifact); err != nil {
			t.Fatalf("recovery/replay: %v %s", err, out)
		}
	}
	if got := reviewack.Consume(root, sha, reviewer, row.ArtifactDigest, reviewer); !got.OK {
		t.Fatalf("recovered ack unusable: %+v", got)
	}
	after, err := os.ReadFile(ledgerPath)
	if err != nil || !bytes.Equal(after, ledgerBytes) {
		t.Fatal("ack recovery mutated ledger", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".herd", "pool")); !os.IsNotExist(err) {
		t.Fatal("ack recovery touched pool", err)
	}
	// Modified bytes, even with the same YAML identity, cannot acquire an ack.
	changed := append(append([]byte{}, body...), []byte("new assertions\n")...)
	request := filepath.Join(root, "changed.md")
	if err := os.WriteFile(request, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run("--ack-only", request); err == nil || !strings.Contains(string(out), "exact current admitted") {
		t.Fatalf("unadmitted bytes accepted: %v %s", err, out)
	}
	foreign := filepath.Join(t.TempDir(), "foreign-ledger.jsonl")
	if err := os.WriteFile(foreign, ledgerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run("--ack-only", "--ledger", foreign, artifact); err == nil || !strings.Contains(string(out), "mixed-project") {
		t.Fatalf("foreign ledger allowed: %v %s", err, out)
	}
	// A retained file changing underneath the ledger must also fail closed.
	if err := os.WriteFile(request, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run("--ack-only", request); err == nil || !strings.Contains(string(out), "retained artifact bytes differ") {
		t.Fatalf("corrupt retention accepted: %v %s", err, out)
	}
	if out, err := run("--ack-only", "--sweep"); err == nil {
		t.Fatalf("corpus recovery allowed: %s", out)
	}
}

// This also preserves FAC-581: replay of any verdict polarity must remain a
// duplicate without rewriting history. Enqueue status is not admission status.
func TestFAC763LiveAdmissionAckFailureIsStructuredAndRecoverable(t *testing.T) {
	binary := buildHerd(t)
	for _, verdict := range []string{"PASS", "FAIL", "BLOCKED"} {
		t.Run(verdict, func(t *testing.T) {
			repo, sha := corroborationRepo(t)
			for _, key := range []string{"HERD_ROOT", "HERD_REPO_ROOT", "HERD_PROJECT_ROOT", "HERD_CANONICAL_ROOT"} {
				t.Setenv(key, repo)
			}
			ledgerPath := filepath.Join(repo, ".herd", "review-ledger.jsonl")
			t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
			t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(repo, ".herd", "launch-receipts.jsonl"))
			t.Setenv("HERD_REVIEW_ROOT", "")
			writeVerdict(t, repo, sha, "unknown")
			source := filepath.Join(repo, ".herd", "review", "inbox", "sha-review-corroboration.md")
			body, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.Replace(body, []byte("verdict: PASS"), []byte("verdict: "+verdict), 1)
			if err := os.WriteFile(source, body, 0600); err != nil {
				t.Fatal(err)
			}
			blockedAck := filepath.Join(repo, reviewack.DirRel)
			if err := os.WriteFile(blockedAck, []byte("publication failure fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "review-ingest", "--json", source)
			cmd.Dir = repo
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, runErr := cmd.Output()
			if runErr == nil {
				t.Fatalf("ack failure returned success: %s %s", out, stderr.String())
			}
			var result struct {
				Admitted int                   `json:"admitted"`
				Refused  int                   `json:"refused"`
				Outcomes []reviewIngestOutcome `json:"outcomes"`
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("JSON: %v %s stderr=%s", err, out, stderr.String())
			}
			if result.Admitted != 0 || result.Refused != 1 || len(result.Outcomes) != 1 {
				t.Fatalf("one artifact counted incorrectly: %s stderr=%s", out, stderr.String())
			}
			if _, err := os.Stat(mail.CallbackMailPath(repo)); !os.IsNotExist(err) {
				t.Fatalf("unacknowledged admission posted completion callback: %v", err)
			}
			outcome := result.Outcomes[0]
			if outcome.Disposition != "admitted_unacked" || !strings.Contains(outcome.Reason, "--ack-only") || outcome.SHA != sha {
				t.Fatalf("ack failure is not actionable: %s stderr=%s", out, stderr.String())
			}
			ledger, err := reviewledger.NewReadOnlyReviewLedger(repo, ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			row, found, err := ledger.VerdictForReviewer(sha, "review-corroboration")
			if err != nil || !found || row.Verdict != verdict {
				t.Fatalf("verdict was not actually admitted: %+v %v", row, err)
			}
			before, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(blockedAck); err != nil {
				t.Fatal(err)
			}
			cmd = exec.Command(binary, "review-ingest", "--ack-only", filepath.Join(repo, row.Artifact))
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("native recovery failed: %v %s", err, out)
			}
			after, err := os.ReadFile(ledgerPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("recovery changed verdict history", err)
			}
			if got := reviewack.Consume(repo, sha, row.Reviewer, row.ArtifactDigest, row.Reviewer); !got.OK {
				t.Fatalf("recovery ack unusable: %+v", got)
			}
			cmd = exec.Command(binary, "review-ingest", "--json", filepath.Join(repo, row.Artifact))
			cmd.Dir = repo
			replay, err := cmd.Output()
			if err != nil {
				t.Fatalf("ordinary replay: %v %s", err, replay)
			}
			if err := json.Unmarshal(replay, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Outcomes) != 1 || result.Outcomes[0].Disposition != "duplicate" || result.Refused != 0 {
				t.Fatalf("historical verdict reapplied: %s", replay)
			}
			after, err = os.ReadFile(ledgerPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("ordinary replay changed verdict history", err)
			}

		})
	}
}
