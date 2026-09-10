package resources

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// The census must tolerate ordinary non-root hosts. PID 1 and other foreign
// processes may deny argv/maps access, but that unrelated denial must not turn
// an owner-only unused cache into a permanent false active-process finding.
func TestLSOFProcessInspectorUnusedPrivateDirectory(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil && runtime.GOOS != "darwin" {
		t.Skipf("native process census is unavailable: %v", err)
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	root := t.TempDir()
	inspector := LSOFProcessInspector{Timeout: 8 * time.Second, MaxOutputBytes: 1 << 20}
	usage, err := inspector.InUse(context.Background(), root)
	if err != nil {
		t.Fatalf("unused private directory census failed: %v", err)
	}
	if usage.CWD || usage.OpenFile || usage.ReferencedPath || usage.MetadataUnavailable {
		t.Fatalf("unused private directory was treated as referenced: %+v", usage)
	}
}

func TestLsofNoMatchDistinguishesNamespaceDiagnostics(t *testing.T) {
	target := "/private/cache"
	noMatch := func(t *testing.T, script string, stdout, stderr []byte, want bool) {
		t.Helper()
		err := exec.Command("sh", "-c", script).Run()
		if got := lsofNoMatch(err, stdout, stderr, target); got != want {
			t.Fatalf("lsofNoMatch(%q, stdout=%q, stderr=%q)=%v, want %v", script, stdout, stderr, got, want)
		}
	}
	noMatch(t, "exit 1", nil, nil, true)
	noMatch(t, "exit 1", nil, []byte("can't stat unrelated /proc path"), true)
	noMatch(t, "exit 1", nil, []byte("can't stat /private/cache"), false)
	noMatch(t, "exit 1", []byte("p123\nf1\n"), nil, false)
	noMatch(t, "exit 2", nil, nil, false)
}
