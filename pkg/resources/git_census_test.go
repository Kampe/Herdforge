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
	claims, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "launch-claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer claims.Close()
	host := "host-a"
	worktree := filepath.Join(root, ".herd", "worktrees", "FAC-613")
	sha := strings.Repeat("a", 40)
	if _, err := claims.Acquire(context.Background(), claim.LeaseKey{Repo: "repo-a", Provider: "kaneo", Project: "project-a", TaskRef: "FAC-613"}, host+"-owner", "worker", worktree, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(root, ".herd", "review-ledger.jsonl")
	file, err := os.Create(ledger)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []map[string]string{
		{"event": "verdict", "sha": sha, "verdict": "FAIL"},
		{"event": "enqueue", "sha": sha},
	} {
		if err := json.NewEncoder(file).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reader := SQLiteLifecycleEvidence{ClaimsPath: filepath.Join(root, ".herd", "launch-claims.db"), LedgerPath: ledger, RepoID: "repo-a", HostID: host}
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
