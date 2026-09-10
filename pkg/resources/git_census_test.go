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
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skipf("lsof unavailable: %v", err)
	}
	root := t.TempDir()
	inspector := LSOFProcessInspector{Timeout: 8 * time.Second, MaxOutputBytes: 1 << 20}
	usage, err := inspector.InUse(context.Background(), root)
	if err != nil {
		t.Fatalf("unused private directory census failed: %v", err)
	}
	if usage.MetadataUnavailable {
		t.Skipf("optional native census cannot inspect all process metadata: %+v", usage)
	}
	if usage.CWD || usage.OpenFile || usage.ReferencedPath || usage.MetadataUnavailable {
		t.Fatalf("unused private directory was treated as referenced: %+v", usage)
	}
}

func TestLSOFProcessInspectorPreservesPositiveExitOneOwnerEvidence(t *testing.T) {
	root := t.TempDir()
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nprintf 'p99999\\nfcwd\\nn%s\\n' \"$4\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Deterministic process table: the real host pid table's walk duration
	// is a host-load property, unrelated to the lsof-exit-1 contract here.
	writeSelfPS(t)
	inspector := LSOFProcessInspector{Executable: lsof, Timeout: 15 * time.Second}
	usage, err := inspector.InUse(context.Background(), root)
	if err != nil {
		t.Fatalf("positive lsof exit 1 should remain usable owner evidence: %v", err)
	}
	if !usage.CWD {
		t.Fatalf("positive lsof evidence=%+v", usage)
	}
}

func TestLSOFProcessInspectorPositiveExitOneWithoutNameMarksMetadataUnavailable(t *testing.T) {
	root := t.TempDir()
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nprintf 'p99999\\nf3\\n'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSelfPS(t)
	inspector := LSOFProcessInspector{Executable: lsof, Timeout: 15 * time.Second}
	usage, err := inspector.InUse(context.Background(), root)
	if err != nil {
		t.Fatalf("positive lsof exit 1 should not fail hard when parsed: %v", err)
	}
	if !usage.MetadataUnavailable {
		t.Fatalf("expected MetadataUnavailable for positive exit 1 without name, got usage=%+v", usage)
	}
}

func TestProcessOwnerViaPSClassifiesOwnerEvidence(t *testing.T) {
	foreignUID := 0
	if os.Getuid() == 0 {
		foreignUID = 1
	}
	tests := []struct {
		name        string
		output      string
		exit        string
		wantForeign bool
		wantGone    bool
		wantErr     bool
	}{
		{name: "foreign uid", output: strconv.Itoa(foreignUID) + "\n", wantForeign: true},
		{name: "same uid", output: strconv.Itoa(os.Getuid()) + "\n"},
		{name: "unknown uid", output: "not-a-uid\n", wantErr: true},
		{name: "ps failure", exit: "1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "ps")
			body := "#!/bin/sh\n"
			if tc.output != "" {
				body += "printf '%s' '" + tc.output + "'\n"
			}
			if tc.exit != "" {
				body += "exit " + tc.exit + "\n"
			}
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			foreign, gone, err := processOwnerViaPS(context.Background(), os.Getpid())
			if foreign != tc.wantForeign || gone != tc.wantGone || (err != nil) != tc.wantErr {
				t.Fatalf("processOwnerViaPS()=(foreign=%t,gone=%t,err=%v), want (%t,%t,%t)", foreign, gone, err, tc.wantForeign, tc.wantGone, tc.wantErr)
			}
		})
	}
}

func TestSnapshotProcessOwnersUsesBulkPIDUIDFields(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	psPath := filepath.Join(dir, "ps")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > '" + argsPath + "'\nprintf '123 456\\n'\n"
	if err := os.WriteFile(psPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	owners, err := snapshotProcessOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if owners[123] != 456 {
		t.Fatalf("owner snapshot=%v", owners)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(args); got != "-axo pid=,uid=" {
		t.Fatalf("ps argv=%q", got)
	}
}

// writeSilentLsofAndSelfPS installs deterministic fake lsof/ps binaries. The
// fake lsof reports no open-file evidence; the fake ps reports only the test
// process itself (same uid), so the census walk is tiny and its outcome is
// fully determined by the test, not the host process table.
func writeSilentLsofAndSelfPS(t *testing.T) (lsofPath string) {
	t.Helper()
	lsofDir := t.TempDir()
	lsofPath = filepath.Join(lsofDir, "lsof")
	if err := os.WriteFile(lsofPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSelfPS(t)
	return lsofPath
}

// writeSelfPS installs a fake ps reporting this test process plus one
// phantom pid. The phantom answers the owner query as gone (empty uid
// output), so the walk is deterministic and never depends on the host
// process table -- whose walk duration is a host-load property.
func writeSelfPS(t *testing.T) {
	t.Helper()
	psDir := t.TempDir()
	// Kept out of the probed path: the ps binary dir lands in the test
	// process's own PATH environment, which the Linux /proc/self/environ
	// walk reads, and must never substring-match the probed directory.
	psScript := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *\"pid=,uid=\"*) printf '%s %s\\n' \"$FAKE_PS_SELF_PID\" \"$FAKE_PS_SELF_UID\" ;;\n" +
		"  *-axo*) printf '%s\\n999998\\n' \"$FAKE_PS_SELF_PID\" ;;\n" +
		"  *\"-o uid=\"*) if [ \"$2\" = \"$FAKE_PS_SELF_PID\" ]; then printf '%s\\n' \"$FAKE_PS_SELF_UID\"; fi ;;\n" +
		"  *) printf '%s\\n' \"$FAKE_PS_SELF_UID\" ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(psDir, "ps"), []byte(psScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", psDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PS_SELF_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("FAKE_PS_SELF_UID", strconv.Itoa(os.Getuid()))
}

// writeSelfAndChildPS installs a fake ps whose census walk consults exactly
// two same-uid pids: this test process and one live fixture child. The
// child's real argv carries the probed path (the fixture launches it with
// `cd <path>`), so walk-derived reference evidence stays exercised.
func writeSelfAndChildPS(t *testing.T, childPID int) {
	t.Helper()
	psDir := t.TempDir()
	psScript := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *\"pid=,uid=\"*) printf '%s %s\\n%s %s\\n' \"$FAKE_PS_SELF_PID\" \"$FAKE_PS_SELF_UID\" \"$FAKE_PS_CHILD_PID\" \"$FAKE_PS_SELF_UID\" ;;\n" +
		"  *-axo*) printf '%s\\n%s\\n' \"$FAKE_PS_SELF_PID\" \"$FAKE_PS_CHILD_PID\" ;;\n" +
		"  *\"-o uid=\"*) printf '%s\\n' \"$FAKE_PS_SELF_UID\" ;;\n" +
		"  *) printf '%s\\n' \"$FAKE_PS_SELF_UID\" ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(psDir, "ps"), []byte(psScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", psDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PS_SELF_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("FAKE_PS_SELF_UID", strconv.Itoa(os.Getuid()))
	t.Setenv("FAKE_PS_CHILD_PID", strconv.Itoa(childPID))
}

// Cancellation landing exactly between PID iterations previously left the
// walk's partial result reading as a definitive no-owner census. A GC that
// trusts such a census can remove a slot whose remaining pids were never
// consulted. The walk must fail closed instead: mark the census
// unavailable, and a genuinely ownerless candidate must still come back
// definitively clean so reclamation is not poisoned.

func TestLSOFProcessInspectorInUseCancellationBetweenIterationsMarksMetadataUnavailable(t *testing.T) {
	lsof := writeSilentLsofAndSelfPS(t)
	root := t.TempDir()
	inspector := LSOFProcessInspector{Executable: lsof, Timeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	inspector.processReferencesFn = func(ctx context.Context, pid int, path string) (bool, error) {
		calls++
		if calls == 1 {
			cancel() // expiry lands before the next iteration's top-of-loop check
		}
		return false, nil
	}
	usage, err := inspector.InUse(ctx, root)
	if err != nil {
		t.Fatalf("incomplete walk must fail closed via metadata, not a hard error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("reference walk continued past cancellation, probes=%d", calls)
	}
	if !usage.MetadataUnavailable {
		t.Fatalf("cancellation between iterations must mark MetadataUnavailable, got usage=%+v", usage)
	}
}

func TestLSOFProcessInspectorInUseManyCancellationBetweenIterationsMarkMetadataUnavailable(t *testing.T) {
	lsof := writeSilentLsofAndSelfPS(t)
	root := filepath.Clean(t.TempDir())
	inspector := LSOFProcessInspector{Executable: lsof, Timeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	inspector.processReferencesManyFn = func(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error) {
		calls++
		if calls == 1 {
			cancel() // expiry lands before the next iteration's top-of-loop check
		}
		return map[string]bool{root: false}, nil
	}
	usage, err := inspector.InUseMany(ctx, []string{root})
	if err == nil {
		t.Fatalf("incomplete walk must propagate cancellation, got usage=%+v err=<nil>", usage)
	}
	if calls != 1 {
		t.Fatalf("reference walk continued past cancellation, probes=%d", calls)
	}
	entry, ok := usage[root]
	if !ok || !entry.MetadataUnavailable {
		t.Fatalf("cancellation between iterations must mark MetadataUnavailable, got ok=%t usage=%+v", ok, entry)
	}
}

func TestLSOFProcessInspectorCleanCandidateRemainsDefinitive(t *testing.T) {
	lsof := writeSilentLsofAndSelfPS(t)
	root := t.TempDir()
	inspector := LSOFProcessInspector{Executable: lsof, Timeout: 30 * time.Second}
	usage, err := inspector.InUse(context.Background(), root)
	if err != nil {
		t.Fatalf("clean candidate census failed: %v", err)
	}
	if usage.CWD || usage.OpenFile || usage.ReferencedPath || usage.MetadataUnavailable {
		t.Fatalf("genuinely ownerless clean candidate must stay definitively reclaimable, got usage=%+v", usage)
	}
}

func TestLsofNoMatchDistinguishesNamespaceDiagnostics(t *testing.T) {
	noMatch := func(t *testing.T, script string, stdout, stderr []byte, want bool) {
		t.Helper()
		err := exec.Command("sh", "-c", script).Run()
		if got := lsofNoMatch(err, stdout, stderr); got != want {
			t.Fatalf("lsofNoMatch(%q, stdout=%q, stderr=%q)=%v, want %v", script, stdout, stderr, got, want)
		}
	}
	noMatch(t, "exit 1", nil, nil, true)
	noMatch(t, "exit 1", nil, []byte("lsof: WARNING: can't stat() hugetlbfs file system /dev/hugepages\n      Output information may be incomplete.\nlsof: WARNING: can't stat() mqueue file system /dev/mqueue\n      Output information may be incomplete."), true)
	noMatch(t, "exit 1", nil, []byte("lsof: cannot open /proc: Permission denied"), false)
	noMatch(t, "exit 1", nil, []byte("lsof: WARNING: can't stat() unexpectedfs file system /run\n      Output information may be incomplete."), false)
	noMatch(t, "exit 1", nil, []byte("lsof: WARNING: can't stat() mqueue file system /dev/mqueue"), false)
	noMatch(t, "exit 1", []byte("p123\nf1\n"), nil, false)
	noMatch(t, "exit 2", nil, nil, false)
}
