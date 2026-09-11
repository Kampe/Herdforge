package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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

// loggingLsof installs a fake lsof that records every invocation's argv.
// A `+D` invocation is a per-target subtree descent; full-table mode is
// `-nP -Ffnp` alone. table is emitted only in full-table mode so a
// regression back into per-target spawning is observable, never silently
// equivalent.
func loggingLsof(t *testing.T, table string, failFullTable bool) (path string, argvLog func() []string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "lsof")
	log := filepath.Join(dir, "argv.log")
	fail := "0"
	if failFullTable {
		fail = "1"
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		"for a in \"$@\"; do [ \"$a\" = \"+D\" ] && exit 9; done\n" +
		"[ " + fail + " = 1 ] && exit 9\n" +
		"[ -n \"$FAKE_LSOF_STDERR\" ] && printf '%s' \"$FAKE_LSOF_STDERR\" >&2\n" +
		"printf '%s' \"$FAKE_LSOF_TABLE\"\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LSOF_TABLE", table)
	return path, func() []string {
		data, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		trimmed := strings.TrimSpace(string(data))
		if trimmed == "" {
			return nil
		}
		return strings.Split(trimmed, "\n")
	}
}

// Production worktree-reap calls InUseMany directly, with no governor to
// hand down a captured population. Running the per-target +D chunks before
// listProcessIDs left the process-list snapshot on an already-expired child
// context and deferred every target. InUseMany must capture the shared
// population ONCE up front and derive every target from that one capture,
// with zero per-target descents.
func TestInUseManySharesOneFullTableCaptureAcrossTargets(t *testing.T) {
	writeSelfPS(t)
	base := t.TempDir()
	active := filepath.Join(base, "wt-active")
	clean := filepath.Join(base, "wt-clean")
	for _, dir := range []string{filepath.Join(active, "sub"), clean} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Clean(resolved)
	}
	activeResolved, cleanResolved := resolve(active), resolve(clean)
	table := fmt.Sprintf("p%d\nf3\nn%s\n", os.Getpid(), filepath.Join(activeResolved, "sub", "graph.db"))
	lsof, argvLog := loggingLsof(t, table, false)
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 30 * time.Second,
		processReferencesManyFn: func(_ context.Context, _ int, paths []string, _ map[int]int) (map[string]bool, error) {
			references := make(map[string]bool, len(paths))
			for _, path := range paths {
				references[path] = false
			}
			return references, nil
		},
	}
	usage, err := inspector.InUseMany(context.Background(), []string{active, clean})
	if err != nil {
		t.Fatalf("bulk census must not hard-error: %v", err)
	}
	invocations := argvLog()
	if len(invocations) != 1 {
		t.Fatalf("multiple targets must share ONE full-table capture, got %d lsof invocation(s): %v", len(invocations), invocations)
	}
	if strings.Contains(invocations[0], "+D") || strings.TrimSpace(invocations[0]) != "-nP -Ffnp" {
		t.Fatalf("the single capture must be full-table, not a per-target descent, got %q", invocations[0])
	}
	activeUsage, ok := usage[activeResolved]
	if !ok || !activeUsage.OpenFile || activeUsage.MetadataUnavailable {
		t.Fatalf("held target must read active from the shared table, got ok=%t usage=%+v", ok, activeUsage)
	}
	cleanUsage, ok := usage[cleanResolved]
	if !ok || cleanUsage.MetadataUnavailable || cleanUsage.OpenFile || cleanUsage.CWD {
		t.Fatalf("unheld target must stay definitively clean, got ok=%t usage=%+v", ok, cleanUsage)
	}
}

// The legacy per-target fallback and its fail-closed semantics survive: when
// no open-file table can be captured, targets are probed the old way and an
// unobservable target stays unknown, never definitively clean.
func TestInUseManyFallsBackFailClosedWithoutTable(t *testing.T) {
	writeSelfPS(t)
	base := t.TempDir()
	targets := []string{filepath.Join(base, "a"), filepath.Join(base, "b")}
	for _, dir := range targets {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lsof, argvLog := loggingLsof(t, "", true)
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 30 * time.Second,
		processReferencesManyFn: func(_ context.Context, _ int, paths []string, _ map[int]int) (map[string]bool, error) {
			references := make(map[string]bool, len(paths))
			for _, path := range paths {
				references[path] = false
			}
			return references, nil
		},
	}
	usage, err := inspector.InUseMany(context.Background(), targets)
	if err != nil {
		t.Fatalf("per-target probe failures are metadata, not batch errors: %v", err)
	}
	descents := 0
	for _, invocation := range argvLog() {
		if strings.Contains(invocation, "+D") {
			descents++
		}
	}
	if descents == 0 {
		t.Fatalf("a failed table capture must still fall back to per-target +D probes, got %v", argvLog())
	}
	for _, target := range targets {
		resolved, resolveErr := filepath.EvalSymlinks(target)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		entry, ok := usage[filepath.Clean(resolved)]
		if !ok || !entry.MetadataUnavailable {
			t.Fatalf("unobservable target %s must stay unknown, never clean, got ok=%t usage=%+v", target, ok, entry)
		}
	}
}

// InUseManyPopulation delegates to InUseMany exactly when the population is
// nil or carries no PIDs. InUseMany must therefore never hand a PID-less
// population back to it: that is an unbounded mutual recursion, and the
// empty-population case has to terminate in the per-target path.
func TestInUseManyDoesNotRecurseOnPIDLessPopulation(t *testing.T) {
	writeSelfPS(t)
	target := t.TempDir()
	lsof := writeSilentLsofAndSelfPS(t)
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 30 * time.Second,
		populationFn: func(context.Context) ([]int, map[int]int, error) {
			return nil, map[int]int{}, nil
		},
		processReferencesManyFn: func(_ context.Context, _ int, paths []string, _ map[int]int) (map[string]bool, error) {
			references := make(map[string]bool, len(paths))
			for _, path := range paths {
				references[path] = false
			}
			return references, nil
		},
	}
	if _, err := inspector.InUseMany(context.Background(), []string{target}); err != nil {
		t.Fatalf("empty population must terminate in the per-target path: %v", err)
	}
}

// A full-table capture that exits 0 while reporting an unknown or permission
// diagnostic has produced a PARTIAL table, and a partial table read as
// authoritative marks every omitted holder's target definitively unheld. The
// per-target probes already treat such diagnostics as observation errors; the
// capture must too — rejecting the table and deferring to those probes, which
// then leave the target unknown rather than clean.
func TestFullTableCaptureRejectsPartialTableDiagnostics(t *testing.T) {
	writeSelfPS(t)
	base := t.TempDir()
	target := filepath.Join(base, "held")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	resolved = filepath.Clean(resolved)
	// The table OMITS the holder of this target, exactly as a truncated or
	// permission-limited capture would.
	lsof, argvLog := loggingLsof(t, fmt.Sprintf("p%d\nf3\nn/usr/lib/dyld\n", os.Getpid()), false)
	t.Setenv("FAKE_LSOF_STDERR", "lsof: WARNING: can't stat() nfs file system /mnt/vol\n      Output information may be incomplete.")
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 30 * time.Second,
		processReferencesManyFn: func(_ context.Context, _ int, paths []string, _ map[int]int) (map[string]bool, error) {
			references := make(map[string]bool, len(paths))
			for _, path := range paths {
				references[path] = false
			}
			return references, nil
		},
	}
	usage, err := inspector.InUseMany(context.Background(), []string{target})
	if err != nil {
		t.Fatalf("a rejected capture defers to the per-target probes, it is not a batch error: %v", err)
	}
	descents := 0
	for _, invocation := range argvLog() {
		if strings.Contains(invocation, "+D") {
			descents++
		}
	}
	if descents == 0 {
		t.Fatalf("a diagnosed partial table must be rejected in favour of per-target probes, got %v", argvLog())
	}
	entry, ok := usage[resolved]
	if !ok || !entry.MetadataUnavailable {
		t.Fatalf("a target observed only through a diagnosed partial table must stay unknown, got ok=%t usage=%+v", ok, entry)
	}
}

// The allowlist must not widen: the two known WSL warnings with their
// continuation lines are still an ordinary success, so an active target is
// still protected and a clean target stays decidable.
func TestFullTableCaptureAcceptsKnownWSLWarnings(t *testing.T) {
	writeSelfPS(t)
	base := t.TempDir()
	active := filepath.Join(base, "wt-active")
	clean := filepath.Join(base, "wt-clean")
	for _, dir := range []string{filepath.Join(active, "sub"), clean} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Clean(resolved)
	}
	activeResolved, cleanResolved := resolve(active), resolve(clean)
	table := fmt.Sprintf("p%d\nf3\nn%s\n", os.Getpid(), filepath.Join(activeResolved, "sub", "graph.db"))
	lsof, argvLog := loggingLsof(t, table, false)
	t.Setenv("FAKE_LSOF_STDERR", "lsof: WARNING: can't stat() mqueue file system /dev/mqueue\n      Output information may be incomplete.")
	inspector := LSOFProcessInspector{
		Executable: lsof, Timeout: 30 * time.Second,
		processReferencesManyFn: func(_ context.Context, _ int, paths []string, _ map[int]int) (map[string]bool, error) {
			references := make(map[string]bool, len(paths))
			for _, path := range paths {
				references[path] = false
			}
			return references, nil
		},
	}
	usage, err := inspector.InUseMany(context.Background(), []string{active, clean})
	if err != nil {
		t.Fatalf("known warnings are not an error: %v", err)
	}
	for _, invocation := range argvLog() {
		if strings.Contains(invocation, "+D") {
			t.Fatalf("known warnings must not reject the table, got per-target descent %q", invocation)
		}
	}
	if entry := usage[activeResolved]; !entry.OpenFile || entry.MetadataUnavailable {
		t.Fatalf("held target must still read active, got %+v", entry)
	}
	if entry := usage[cleanResolved]; entry.MetadataUnavailable || entry.OpenFile || entry.CWD {
		t.Fatalf("unheld target must stay definitively clean, got %+v", entry)
	}
}

// Every other lsof invocation is bounded by the probe knob. The full-table
// capture took its deadline solely from the caller's context, and InUseMany
// is a public entry point that callers do reach with context.Background():
// a stuck lsof then runs with nothing to stop it. The capture must respect an
// outer deadline when there is one and impose a finite fallback when there is
// not, and it must not leave the child behind either way.
func TestFullTableCaptureBoundsAnUnboundedParentContext(t *testing.T) {
	hangingLsof := func(t *testing.T) (string, func() int) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "lsof-hang")
		pidFile := filepath.Join(dir, "lsof.pid")
		// exec replaces the shell, so the recorded pid IS the sleeping
		// process: killing the command kills exactly this pid, and a leak
		// is observable rather than hidden behind a shell wrapper.
		script := "#!/bin/sh\necho $$ > " + pidFile + "\nexec sleep 10\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		return path, func() int {
			data, err := os.ReadFile(pidFile)
			if err != nil {
				return 0
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				return 0
			}
			return pid
		}
	}
	assertBounded := func(t *testing.T, name string, insp LSOFProcessInspector, ctx context.Context, childPID func() int) {
		t.Helper()
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			_, err := insp.captureOpenFiles(ctx)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: a capture that never produced a table must fail, not succeed", name)
			}
			t.Logf("%s: bounded after %s: %v", name, time.Since(start), err)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: captureOpenFiles never returned — the capture is unbounded and leaves lsof running", name)
		}
		pid := childPID()
		if pid == 0 {
			t.Fatalf("%s: fake lsof never recorded its pid", name)
		}
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: fake lsof pid %d still running — the bounded capture leaked its child", name, pid)
	}

	// No outer deadline at all: the probe knob must bound it.
	noDeadlineLsof, noDeadlinePID := hangingLsof(t)
	assertBounded(t, "background parent",
		LSOFProcessInspector{Executable: noDeadlineLsof, Timeout: 300 * time.Millisecond},
		context.Background(), noDeadlinePID)

	// An outer deadline is still authoritative, including when it is far
	// shorter than the knob.
	outerLsof, outerPID := hangingLsof(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	assertBounded(t, "outer deadline",
		LSOFProcessInspector{Executable: outerLsof, Timeout: time.Hour},
		ctx, outerPID)
}

// stubProcessProcFn replaces the /proc metadata read for the duration of one
// test. The census's per-pid decision must be provable without a real /proc or
// a dumpable-cleared process on the host.
func stubProcessProcFn(t *testing.T, fn func(pid int) ([][]byte, error)) {
	t.Helper()
	original := readProcessProcFn
	readProcessProcFn = fn
	t.Cleanup(func() { readProcessProcFn = original })
}

// TestSameOwnerMetadataRefusalStaysFailClosed pins the deletion-authority
// contract of the act-time owner census at the same-owner decision: a
// same-owner pid whose private /proc metadata the kernel REFUSES — permission
// denied, the dumpable-cleared credential state many runner daemons carry —
// is an UNKNOWN observation, not a complete one. The public command surface
// (ps -ww command=) proves argv only and never the environment, so a path
// held only in the environment of a live same-owner process would be
// unobservable if the census accepted an argv-only answer. The refusal must
// propagate as an error (MetadataUnavailable upstream) so the reaper and pool
// guards fail closed. The 44cf27cf correction degraded this refusal to the
// ps probe and was rejected in review for exactly that loss of coverage.
func TestSameOwnerMetadataRefusalStaysFailClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the /proc same-owner refusal surface under test is linux; %s answers the same-owner decision through procargs", runtime.GOOS)
	}
	pid := os.Getpid()
	target := filepath.Join(t.TempDir(), "worktrees", "harvest-stage")
	procErr := &fs.PathError{Op: "open", Path: fmt.Sprintf("/proc/%d/environ", pid), Err: syscall.EACCES}
	stubProcessProcFn(t, func(int) ([][]byte, error) { return nil, procErr })

	references, err := processReferencesManyWithOwners(context.Background(), pid, []string{target}, map[int]int{pid: os.Getuid()})
	if err == nil {
		t.Fatalf("an unreadable same-owner environment must fail the census closed; the census read as complete with %v", references)
	}
	if references[target] {
		t.Fatalf("the failed probe invented a reference: %v", references)
	}

	// The same argv-absent shape with a DIFFERENT refusal keeps the same
	// fail-closed answer: only the classification (gone, foreign) may
	// complete the census, never the read refusal itself.
	otherErr := &fs.PathError{Op: "open", Path: fmt.Sprintf("/proc/%d/environ", pid), Err: syscall.EIO}
	stubProcessProcFn(t, func(int) ([][]byte, error) { return nil, otherErr })
	if _, err := processReferencesManyWithOwners(context.Background(), pid, []string{target}, map[int]int{pid: os.Getuid()}); err == nil {
		t.Fatal("a non-permission metadata failure must stay fail-closed; the census read as complete")
	}
}

// TestEnvironmentOnlyReferenceIsDetectedWhenReadable pins the reference
// surface the fail-closed policy above protects: a live same-owner pid that
// holds the target ONLY in its environment — argv, cwd, open files, and maps
// all clean — is a real reference and must be reported. An implementation
// that answers from the argv-bearing surfaces alone (the rejected 44cf27cf
// fallback) fails this control.
func TestEnvironmentOnlyReferenceIsDetectedWhenReadable(t *testing.T) {
	pid := os.Getpid()
	target := filepath.Join(t.TempDir(), "worktrees", "harvest-stage")
	stubProcessProcFn(t, func(int) ([][]byte, error) {
		return [][]byte{
			[]byte("PATH=/usr/bin\x00GOCACHE=" + target + "\x00"),
			[]byte("/usr/bin/leaf-daemon\x00"),
			[]byte("7f0e0000-7f0f0000 rw-p 00000000 00:00 0\x00"),
		}, nil
	})

	references, err := processReferencesManyWithOwners(context.Background(), pid, []string{target}, map[int]int{pid: os.Getuid()})
	if err != nil {
		t.Fatalf("readable same-owner metadata must answer the census, not fail it: %v", err)
	}
	if !references[target] {
		t.Fatal("an environment-only reference was reported as no reference; the census would clear the reaper guard for a live holder")
	}
}

// TestProcessReferencesManyClassifications pins the classifications that may
// legitimately complete a census answer for a pid the private metadata read
// failed on: a foreign owner is out of scope for a private target, a process
// that is gone cannot hold anything, and an owner missing from the snapshot
// stays fail-closed. The 44cf27cf review requires these refusals preserved
// rather than replaced by a fallback's success.
func TestProcessReferencesManyClassifications(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the /proc-metadata classification under test is the linux surface; darwin answers through procargs")
	}
	target := filepath.Join(t.TempDir(), "worktrees", "harvest-stage")
	procErr := &fs.PathError{Op: "open", Path: "/proc/1/environ", Err: syscall.EACCES}
	stubProcessProcFn(t, func(int) ([][]byte, error) { return nil, procErr })

	// Foreign owner: not deletion authority for a private target, census
	// answer completes without the private metadata.
	references, err := processReferencesManyWithOwners(context.Background(), 1, []string{target}, map[int]int{1: os.Getuid() + 1})
	if err != nil {
		t.Fatalf("a foreign owner must be skipped, not fail the census: %v", err)
	}
	if references[target] {
		t.Fatalf("a foreign pid invented a reference: %v", references)
	}

	// A pid the kernel has already reaped cannot hold a reference: with the
	// owner table gone, the ESRCH classification completes the answer.
	stubProcessProcFn(t, func(int) ([][]byte, error) { return nil, os.ErrNotExist })
	references, err = processReferencesManyWithOwners(context.Background(), 1, []string{target}, nil)
	if err != nil {
		t.Fatalf("a reaped pid must classify as gone, not fail the census: %v", err)
	}
	if references[target] {
		t.Fatalf("a reaped pid invented a reference: %v", references)
	}

	// Owner missing from a present snapshot: the census cannot classify the
	// pid at all, so it must stay fail-closed.
	stubProcessProcFn(t, func(int) ([][]byte, error) { return nil, procErr })
	if _, err := processReferencesManyWithOwners(context.Background(), 1, []string{target}, map[int]int{}); err == nil {
		t.Fatal("an unclassifiable pid must stay fail-closed; the census read as complete")
	}
}

// TestDarwinLiveMetadataRefusalStaysFailClosed pins the darwin twin of the
// same-owner fail-closed contract: a LIVE same-owner pid whose procargs
// argv/environment read the kernel refuses (the classifier deliberately keeps
// live and reused pids protected; only ESRCH resolves as gone) must propagate
// as an error, never as a complete "no reference" answer from the argv-bearing
// public surface alone.
func TestDarwinLiveMetadataRefusalStaysFailClosed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("the procargs refusal surface under test is darwin; %s answers through /proc", runtime.GOOS)
	}
	pid := os.Getpid()
	target := filepath.Join(t.TempDir(), "worktrees", "harvest-stage")
	original := readDarwinProcessArgsFn
	readDarwinProcessArgsFn = func(int) ([]byte, error) {
		return nil, syscall.EINVAL
	}
	t.Cleanup(func() { readDarwinProcessArgsFn = original })
	stubProcessProcFn(t, func(int) ([][]byte, error) { return nil, os.ErrNotExist })

	// os.ErrNotExist short-circuits only off darwin; on darwin the same-owner
	// branch runs and the refused procargs read must fail the census closed.
	references, err := processReferencesManyWithOwners(context.Background(), pid, []string{target}, map[int]int{pid: os.Getuid()})
	if err == nil {
		t.Fatalf("a live same-owner pid with a refused procargs read must fail the census closed; the census read as complete with %v", references)
	}
	if references[target] {
		t.Fatalf("the refused probe invented a reference: %v", references)
	}

	// A pid the kernel has already resolved as gone (ErrProcessDone from the
	// classifier) still completes the answer: the gone classification is
	// preserved.
	readDarwinProcessArgsFn = func(int) ([]byte, error) { return nil, os.ErrProcessDone }
	if _, err := processReferencesManyWithOwners(context.Background(), pid, []string{target}, map[int]int{pid: os.Getuid()}); err != nil {
		t.Fatalf("a classifier-resolved gone pid must complete the census, not fail it: %v", err)
	}
}
