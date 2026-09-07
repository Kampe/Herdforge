package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
