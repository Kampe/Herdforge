package resources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

func TestSQLiteLifecycleEvidenceReadsCanonicalClaimAndReviewRecords(t *testing.T) {
	root := t.TempDir()
	claimsPath := filepath.Join(root, ".herd", "claim", "leases.db")
	claims, err := claim.NewSQLiteLeaseStore(claimsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer claims.Close()
	host := "host-a"
	worktree := filepath.Join(root, ".herd", "worktrees", "FAC-613")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	_, err = claims.AcquireWithIdentity(context.Background(), claim.LeaseKey{Repo: "repo-a", Provider: "kaneo", Project: "project-a", TaskRef: "FAC-613"}, "coordinator-recovery", "worker", worktree, "repo-a", "worker", "smith", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(root, ".herd", "review-ledger.jsonl")
	file, err := os.Create(ledger)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []map[string]string{
		{"event": "record", "sha": sha, "reviewer": "reviewer-1", "host": "review-host", "task": "FAC-613", "lease": "lease-1", "pane": "w4:t1", "builder_family": "openai"},
		{"event": "verdict", "sha": sha, "candidate_sha": sha, "reviewer": "reviewer-1", "host": "review-host", "task": "FAC-613", "verdict": "FAIL"},
		{"event": "verdict", "sha": sha, "candidate_sha": sha, "reviewer": "reviewer-1", "host": "review-host", "task": "FAC-613", "verdict": "PASS"},
	} {
		if err := json.NewEncoder(file).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	queue, err := os.Create(filepath.Join(filepath.Dir(ledger), "harvest-queue.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(queue).Encode(map[string]string{"event": "enqueue", "sha": sha, "reviewer": "reviewer-1", "host": "review-host", "task": "FAC-613", "lane": "reviewer", "status": "queued"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	reader := SQLiteLifecycleEvidence{ClaimsPath: claimsPath, LedgerPath: ledger, RepoID: "repo-a", HostID: host}
	evidence, err := reader.Read(context.Background(), root, host, RegisteredWorktree{Path: worktree, Head: sha})
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.ActiveLease || !evidence.FailedCandidate || !evidence.ReviewHandoffAdmitted {
		t.Fatalf("canonical evidence=%+v", evidence)
	}
}

func TestSQLiteLifecycleEvidenceMissingClaimFailsClosed(t *testing.T) {
	root := t.TempDir()
	reader := SQLiteLifecycleEvidence{ClaimsPath: filepath.Join(root, "missing.db"), LedgerPath: filepath.Join(root, "ledger.jsonl"), RepoID: "repo-a", HostID: "host-a"}
	if _, err := reader.Read(context.Background(), root, "host-a", RegisteredWorktree{Path: filepath.Join(root, "worktree"), Head: strings.Repeat("a", 40)}); err == nil {
		t.Fatal("missing canonical claim record was treated as disposable")
	}
}
