package mutationprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/resources"
)

// hermeticOrigin builds a temp-origin repository and returns (origin, sha).
// All git worktree mutations must run against this fixture so the developer
// repository's worktree list stays untouched (FAC-157 / FAC-152 invariant).
func hermeticOrigin(t *testing.T) (origin, sha string) {
	t.Helper()
	origin = t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@h.local"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		c := testgit.Command(origin, args...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(origin, "candidate.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "candidate.txt"},
		{"commit", "-q", "-m", "feat: candidate"},
	} {
		c := testgit.Command(origin, args...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	c := testgit.Command(origin, "rev-parse", "HEAD")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	return origin, strings.TrimSpace(string(out))
}

func allowAllDisk() resources.DiskAdmission {
	return resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		return resources.DiskDecision{Allowed: true, State: resources.DiskReady}
	})
}

// hermeticExec wraps git invocations with the testgit env policy while
// preserving CommandContext's signature for Manager.
func hermeticExec(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name != "git" {
		return exec.CommandContext(ctx, name, args...)
	}
	// testgit.Command does not take context; bind cancel via CommandContext
	// process group when ctx ends by using a thin wrapper that still inherits
	// testgit's env. Reconstruct: base = testgit.Command with dummy dir.
	base := testgit.Command(".", args...)
	cmd := exec.CommandContext(ctx, base.Path, base.Args[1:]...)
	cmd.Env = base.Env
	return cmd
}

func newHermeticManager(t *testing.T, origin string) *Manager {
	t.Helper()
	m := NewManager(origin, t.TempDir())
	m.DiskAdmission = allowAllDisk()
	m.execCommandContext = hermeticExec
	return m
}

func worktreeLeaves(t *testing.T, origin string) []string {
	t.Helper()
	c := testgit.Command(origin, "worktree", "list", "--porcelain")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("worktree list: %v\n%s", err, out)
	}
	var leaves []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			leaves = append(leaves, filepath.Base(strings.TrimPrefix(line, "worktree ")))
		}
	}
	return leaves
}

func containsLeaf(leaves []string, name string) bool {
	for _, l := range leaves {
		if l == name {
			return true
		}
	}
	return false
}

func TestCreateAndCleanupSuccessLeavesZeroResidue(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)

	probe, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "g1", CandidateSHA: sha, ProbeID: "ok1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !containsLeaf(worktreeLeaves(t, origin), probe.ProbeName) {
		t.Fatalf("probe not registered in origin worktree list: %v", worktreeLeaves(t, origin))
	}
	if _, err := os.Stat(probe.Path); err != nil {
		t.Fatalf("probe dir missing: %v", err)
	}

	if err := EnsureCleanup(context.Background(), store, m, probe.ID, "success"); err != nil {
		t.Fatalf("EnsureCleanup: %v", err)
	}
	if containsLeaf(worktreeLeaves(t, origin), probe.ProbeName) {
		t.Fatalf("disposable probe still registered after cleanup: %v", worktreeLeaves(t, origin))
	}
	if _, err := os.Stat(probe.Path); !os.IsNotExist(err) {
		t.Fatalf("probe dir still present: %v", err)
	}
	got, err := store.Get(probe.ID)
	if err != nil || got == nil || got.State != StateRemoved || !got.AbsenceProved {
		t.Fatalf("receipt = %+v err=%v", got, err)
	}
}

func TestDirtyProbeIsPreservedByteForByteAndBlocked(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)

	probe, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "g1", CandidateSHA: sha, ProbeID: "dirty1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	marker := []byte("unique-uncommitted-evidence\n")
	if err := os.WriteFile(filepath.Join(probe.Path, "candidate.txt"), marker, 0o644); err != nil {
		t.Fatal(err)
	}

	err = EnsureCleanup(context.Background(), store, m, probe.ID, "success")
	if !errors.Is(err, ErrProbePreserved) {
		t.Fatalf("err = %v, want ErrProbePreserved", err)
	}
	if !strings.Contains(err.Error(), "FAC-157") || !strings.Contains(err.Error(), "dirty1") {
		t.Fatalf("BLOCKED error must carry task/probe identity: %v", err)
	}
	// Byte-for-byte preservation.
	got, err := os.ReadFile(filepath.Join(probe.Path, "candidate.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(marker) {
		t.Fatalf("dirty content clobbered: %q", got)
	}
	if !containsLeaf(worktreeLeaves(t, origin), probe.ProbeName) {
		t.Fatal("dirty probe must remain registered")
	}
	receipt, err := store.Get(probe.ID)
	if err != nil || receipt.State != StatePreserved || receipt.Class != ClassDirty {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestUniqueCommittedProbeIsPreserved(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)

	probe, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "g1", CandidateSHA: sha, ProbeID: "uniq1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(probe.Path, "extra.txt"), []byte("unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "extra.txt"},
		{"commit", "-q", "-m", "feat: unique probe commit"},
	} {
		c := testgit.Command(probe.Path, args...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	err = EnsureCleanup(context.Background(), store, m, probe.ID, "failed")
	if !errors.Is(err, ErrProbePreserved) {
		t.Fatalf("err = %v, want ErrProbePreserved", err)
	}
	receipt, err := store.Get(probe.ID)
	if err != nil || receipt.State != StatePreserved || receipt.Class != ClassUnique {
		t.Fatalf("receipt = %+v", receipt)
	}
	if !containsLeaf(worktreeLeaves(t, origin), probe.ProbeName) {
		t.Fatal("unique probe must remain registered")
	}
}

// TestRemovingCleanupBarrierLeaksRegisteredWorktree is the non-vacuity
// mutation: if EnsureCleanup's reap barrier is skipped, the suite must fail
// because the worktree remains registered and the temp dir still exists.
func TestRemovingCleanupBarrierLeaksRegisteredWorktree(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)
	m.SkipCleanup = true

	probe, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "g1", CandidateSHA: sha, ProbeID: "leak1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupErr := EnsureCleanup(context.Background(), store, m, probe.ID, "success")
	if cleanupErr == nil {
		t.Fatal("skip-cleanup seam must surface an error, not succeed")
	}
	// The mutation assertion: without the reap barrier the probe leaks.
	if !containsLeaf(worktreeLeaves(t, origin), probe.ProbeName) {
		t.Fatal("mutation probe expected a leaked registered worktree; barrier is vacuous if already gone")
	}
	if _, err := os.Stat(probe.Path); err != nil {
		t.Fatalf("mutation probe expected temp dir residue: %v", err)
	}
	// Prove the real barrier reclaims it (suite remains hermetic for later tests).
	m.SkipCleanup = false
	if err := EnsureCleanup(context.Background(), store, m, probe.ID, "success"); err != nil {
		t.Fatalf("real cleanup after seam: %v", err)
	}
	if containsLeaf(worktreeLeaves(t, origin), probe.ProbeName) {
		t.Fatal("real cleanup must reclaim the leaked probe")
	}
}

func TestWithProbeCleanupOnSuccessFailureAndCancel(t *testing.T) {
	type outcome string
	const (
		outSuccess outcome = "success"
		outFailure outcome = "failure"
		outCancel  outcome = "cancel"
	)
	cases := []struct {
		name    string
		kind    outcome
		wantErr bool
	}{
		{name: "success", kind: outSuccess},
		{name: "failure", kind: outFailure, wantErr: true},
		{name: "cancel", kind: outCancel, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origin, sha := hermeticOrigin(t)
			store := newTestStore(t)
			m := newHermeticManager(t, origin)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			err := WithProbe(ctx, store, m, CreateRequest{
				TaskRef: "FAC-157", Generation: "g-" + tc.name, CandidateSHA: sha, ProbeID: "wp-" + tc.name,
			}, func(ctx context.Context, _ *Probe) error {
				switch tc.kind {
				case outSuccess:
					return nil
				case outFailure:
					return errors.New("mutation failed")
				case outCancel:
					// Cancel only after Create has returned the probe, so the
					// worktree is fully registered before the run aborts.
					cancel()
					<-ctx.Done()
					return ctx.Err()
				default:
					return errors.New("unknown case")
				}
			})
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			// Disposable probes must leave zero residue regardless of outcome.
			// Cleanup errors are also failures: a cancelled run that leaks is red.
			if containsLeaf(worktreeLeaves(t, origin), "herd-mutprobe.wp-"+tc.name) {
				t.Fatalf("probe residue after %s: %v", tc.name, worktreeLeaves(t, origin))
			}
			all, _ := store.ListAll()
			for _, r := range all {
				if r.ProbeID == "wp-"+tc.name && r.State != StateRemoved {
					t.Fatalf("receipt state = %s after %s (err=%v)", r.State, tc.name, err)
				}
			}
		})
	}
}

func TestReconcileReclaimsOrphanedDisposableAndPreservesDirty(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)

	clean, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "dead", CandidateSHA: sha, ProbeID: "orphan-clean",
	})
	if err != nil {
		t.Fatalf("clean create: %v", err)
	}
	dirty, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "dead", CandidateSHA: sha, ProbeID: "orphan-dirty",
	})
	if err != nil {
		t.Fatalf("dirty create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirty.Path, "candidate.txt"), []byte("keep-me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Live generation still owns a third probe — must be skipped.
	live, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "live", CandidateSHA: sha, ProbeID: "still-live",
	})
	if err != nil {
		t.Fatalf("live create: %v", err)
	}

	liveFn := func(taskRef, generation string) bool {
		return generation == "live"
	}
	report, err := Reconcile(context.Background(), store, m, liveFn)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !contains(report.Reclaimed, clean.ID) {
		t.Fatalf("clean orphan not reclaimed: %+v", report)
	}
	if !contains(report.Preserved, dirty.ID) {
		t.Fatalf("dirty orphan not preserved: %+v", report)
	}
	if !contains(report.Skipped, live.ID) {
		t.Fatalf("live generation must be skipped: %+v", report)
	}
	if !containsLeaf(worktreeLeaves(t, origin), dirty.ProbeName) {
		t.Fatal("dirty probe removed by reconcile")
	}
	if containsLeaf(worktreeLeaves(t, origin), clean.ProbeName) {
		t.Fatal("clean orphan still registered")
	}
	// Cleanup live for hermetic exit.
	if err := EnsureCleanup(context.Background(), store, m, live.ID, "success"); err != nil {
		t.Fatalf("cleanup live: %v", err)
	}
}

func TestFAC83ShapeHasReadOnlyRecoveryReport(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)

	// Materialize the observed FAC-83 leaf shape under TempRoot as a real
	// dirty worktree, without going through Create's herd-mutprobe prefix.
	leaf := "fac83-probe.34VRM1"
	path := filepath.Join(m.TempRoot, leaf)
	cmd := hermeticExec(context.Background(), "git", "worktree", "add", "--detach", path, sha)
	cmd.Dir = origin
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed fac83 probe: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		// Best-effort: only remove if still clean; dirty path is preserved
		// for the assertion and cleaned only after we force-write back.
		_ = os.WriteFile(filepath.Join(path, "candidate.txt"), []byte("original\n"), 0o644)
		c := hermeticExec(context.Background(), "git", "worktree", "remove", path)
		c.Dir = origin
		_ = c.Run()
	})
	if err := os.WriteFile(filepath.Join(path, "pkg_review_review_extended_test.go"), []byte("dirty evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := RecoverReport(context.Background(), store, m, path)
	if err != nil {
		t.Fatalf("RecoverReport: %v", err)
	}
	if report.PathLeaf != leaf || report.ProbeName != leaf {
		t.Fatalf("leaf = %+v", report)
	}
	if !report.DirectoryExists || !report.Registered {
		t.Fatalf("report must see dir+registry: %+v", report)
	}
	if report.Class != ClassDirty && report.Class != ClassUnknown {
		// Dirty file added; class should be dirty.
		t.Fatalf("class = %s, want dirty/unknown", report.Class)
	}
	if !strings.Contains(report.PreserveAction, "no automatic deletion") {
		t.Fatalf("must refuse auto-deletion: %q", report.PreserveAction)
	}
	// Read-only: content and registration still present.
	if !containsLeaf(worktreeLeaves(t, origin), leaf) {
		t.Fatal("RecoverReport must not remove the FAC-83 probe")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("RecoverReport deleted directory: %v", err)
	}
}

func TestRecoverReportRefusesNonProbeLeaves(t *testing.T) {
	origin, _ := hermeticOrigin(t)
	m := newHermeticManager(t, origin)
	_, err := RecoverReport(context.Background(), nil, m, "herd-fac-64")
	if err == nil {
		t.Fatal("must refuse non-probe worktree names")
	}
}

func TestRegistryScopedToHermeticOriginCannotTouchDeveloperRepo(t *testing.T) {
	// Snapshot the live developer worktree list (this process may be inside
	// a Herdforge worktree). After operating on a hermetic origin, the live
	// list must be unchanged.
	liveRoot := findLiveGitRoot(t)
	before := liveWorktreeSnapshot(t, liveRoot)

	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)
	probe, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "g1", CandidateSHA: sha, ProbeID: "scope1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := EnsureCleanup(context.Background(), store, m, probe.ID, "success"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	after := liveWorktreeSnapshot(t, liveRoot)
	if before != after {
		t.Fatalf("hermetic probe ops mutated developer worktree list\nbefore=%s\nafter=%s", before, after)
	}
}

func TestRaceStressCreateCleanup(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)

	// Four concurrent owners is enough to exercise the Manager mutex without
	// saturating the host during a full `go test ./...` package fan-out.
	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("race%02d", i)
			err := WithProbe(context.Background(), store, m, CreateRequest{
				TaskRef: "FAC-157", Generation: "g-race", CandidateSHA: sha, ProbeID: id,
			}, func(_ context.Context, p *Probe) error {
				// Tiny mutation then restore so still disposable.
				path := filepath.Join(p.Path, "candidate.txt")
				orig, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte("mutant\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(path, orig, 0o644)
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("race worker: %v", err)
		}
	}
	// Zero disposable residue in origin worktree list.
	for _, leaf := range worktreeLeaves(t, origin) {
		if strings.HasPrefix(leaf, "herd-mutprobe.") {
			t.Fatalf("leaked probe leaf after race: %s (all=%v)", leaf, worktreeLeaves(t, origin))
		}
	}
}

func TestCreateRefusesNonHexSHA(t *testing.T) {
	origin, _ := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)
	_, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "g1", CandidateSHA: "not-a-sha",
	})
	if err == nil {
		t.Fatal("expected SHA validation error")
	}
}

func TestEnsureCleanupIdempotentOnRemoved(t *testing.T) {
	origin, sha := hermeticOrigin(t)
	store := newTestStore(t)
	m := newHermeticManager(t, origin)
	probe, err := m.Create(context.Background(), store, CreateRequest{
		TaskRef: "FAC-157", Generation: "g1", CandidateSHA: sha, ProbeID: "idem1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureCleanup(context.Background(), store, m, probe.ID, "success"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCleanup(context.Background(), store, m, probe.ID, "success"); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func findLiveGitRoot(t *testing.T) string {
	t.Helper()
	// Prefer the process cwd's git common dir — this is the developer /
	// worktree checkout we must not mutate.
	c := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Skipf("no live git root available: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func liveWorktreeSnapshot(t *testing.T, root string) string {
	t.Helper()
	c := exec.Command("git", "worktree", "list", "--porcelain")
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("live worktree list: %v\n%s", err, out)
	}
	return string(out)
}
