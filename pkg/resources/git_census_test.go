package resources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

func TestSQLiteLifecycleEvidenceReadsCanonicalClaimAndReviewRecords(t *testing.T) {
	root := t.TempDir()
	launchPath := filepath.Join(root, ".herd", "launch-claims.db")
	launch, err := claim.NewSQLiteLeaseStore(launchPath)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	recoveryPath := filepath.Join(root, ".herd", "herdforge.db")
	recovery, err := claim.NewSQLiteLeaseStore(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	taskPath := filepath.Join(root, ".herd", "claim", "leases.db")
	tasks, err := claim.NewSQLiteLeaseStore(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.Close()
	host := "host-a"
	worktree := filepath.Join(root, ".herd", "worktrees", "FAC-613")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	launchLease, err := launch.AcquireWithIdentity(context.Background(), claim.LeaseKey{Repo: "repo-a", Provider: "kaneo", Project: "project-a", TaskRef: "FAC-613"}, "native-launch", "worker", worktree, "repo-a", "worker", "smith", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	recoveryLease, err := recovery.AcquireWithIdentity(context.Background(), claim.LeaseKey{Repo: "repo-a", Provider: "kaneo", Project: "project-a", TaskRef: "FAC-613:recovery"}, "coordinator-recovery", "recovery", "", "repo-a", "coordinator", "recovery", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// A higher generation for an unrelated task key must not mask either
	// authenticated native lease.
	_, err = tasks.AcquireWithIdentity(context.Background(), claim.LeaseKey{Repo: "repo-a", Provider: "kaneo", Project: "project-a", TaskRef: "FAC-999"}, "old", "worker", worktree+"-other", "repo-a", "worker", "smith", time.Now().Add(-2*time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = tasks.Release(context.Background(), claim.LeaseKey{Repo: "repo-a", Provider: "kaneo", Project: "project-a", TaskRef: "FAC-999"}, "old", 1, time.Now()); err != nil {
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
	reader := SQLiteLifecycleEvidence{
		ClaimsPath: launchPath, LaunchClaimsPath: launchPath, RecoveryClaimsPath: recoveryPath, TaskClaimsPath: taskPath,
		LedgerPath: ledger, RepoID: "repo-a", HostID: host,
	}
	callbackCalls := 0
	reader.SignedTarget = func(_ context.Context, _ string, _ RegisteredWorktree) (SignedTarget, error) {
		callbackCalls++
		lease := launchLease
		if callbackCalls > 1 {
			lease = recoveryLease
		}
		return SignedTarget{LeaseID: "claim:" + strconv.FormatInt(lease.ID, 10), LeaseGeneration: lease.Generation, LeaseTaskRef: lease.TaskRef, Repository: "repo-a", CandidateSHA: sha, Authenticated: true}, nil
	}
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
	missing := filepath.Join(root, "missing.db")
	reader := SQLiteLifecycleEvidence{ClaimsPath: missing, LedgerPath: filepath.Join(root, "ledger.jsonl"), RepoID: "repo-a", HostID: "host-a"}
	if _, err := reader.Read(context.Background(), root, "host-a", RegisteredWorktree{Path: filepath.Join(root, "worktree"), Head: strings.Repeat("a", 40)}); err == nil {
		t.Fatal("missing canonical claim record was treated as disposable")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("read-only census created or changed absent claim store: stat=%v", err)
	}
}
