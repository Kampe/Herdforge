package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/resources"
)

// reapPulseRepo builds a hermetic repository whose paths are already
// canonical. macOS hands back a symlinked TempDir, and git reports the
// resolved path, so resolving here keeps the fixture's own assertions and the
// reaper's observed identities comparable.
func reapPulseRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	reapPulseGit(t, root, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reapPulseGit(t, root, "add", ".")
	reapPulseGit(t, root, "commit", "-qm", "base")
	return root
}

func reapPulseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := testgit.Command(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// reapPulseLandedWorktree adds a worktree whose branch holds nothing the base
// does not: the trivially retirable shape.
func reapPulseLandedWorktree(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	reapPulseGit(t, root, "worktree", "add", "-q", "-b", name, dir)
	return dir
}

// reapPulseCleanRetirer routes the seam through the REAL retirement authority
// with a clean owner census, so a positive test proves actual removal without
// depending on a live lsof census.
func reapPulseCleanRetirer(t *testing.T) {
	t.Helper()
	original := reapPulseRetirer
	t.Cleanup(func() { reapPulseRetirer = original })
	reapPulseRetirer = func(root string, landed []reapRow) (retired, failed []map[string]string) {
		clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
			return resources.ProcessUsage{}, nil
		})
		return retireLandedWithInspector(root, landed, clean)
	}
}

// reapPulseRecordingRetirer records what a beat WOULD retire without removing
// anything, so budget and protection assertions cannot be satisfied by a
// removal that merely happened to fail.
func reapPulseRecordingRetirer(t *testing.T, seen *[]reapRow) {
	t.Helper()
	original := reapPulseRetirer
	t.Cleanup(func() { reapPulseRetirer = original })
	reapPulseRetirer = func(root string, landed []reapRow) (retired, failed []map[string]string) {
		*seen = append(*seen, landed...)
		for _, row := range landed {
			retired = append(retired, map[string]string{"path": row.Path, "branch": row.Branch})
		}
		return retired, nil
	}
}

func reapPulseBranchExists(t *testing.T, root, branch string) bool {
	t.Helper()
	return testgit.Command(root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// The positive case: a landed worktree is actually retired by one beat, and
// the claim is checked against the filesystem and the ref store rather than
// against the beat's own report.
func TestReapPulseRetiresLandedWorktreeOnBeat(t *testing.T) {
	root := reapPulseRepo(t)
	dir := reapPulseLandedWorktree(t, root, "landed-lane")
	reapPulseCleanRetirer(t)

	report, err := runReapPulseTick(context.Background(), root, "main", true)
	if err != nil {
		t.Fatalf("beat failed: %v", err)
	}
	if report.Landed != 1 || report.Retired != 1 || report.Failed != 0 {
		t.Fatalf("landed worktree was not retired by the beat: %+v", report)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("beat reported a retirement while %s still exists (err=%v)", dir, statErr)
	}
	if reapPulseBranchExists(t, root, "landed-lane") {
		t.Fatal("beat reported a retirement while its branch still exists")
	}
	if report.NextCursor == "" {
		t.Fatal("an acting beat must persist its rotation cursor")
	}
}

// Every protection is asserted causally: the surface still exists afterwards
// AND the beat never even offered it to the retirement authority. Removing any
// one guard makes the matching subtest fail.
func TestReapPulseProtectsLiveDirtyLockedUnknownAndUnmerged(t *testing.T) {
	t.Run("dirty untracked evidence is kept", func(t *testing.T) {
		root := reapPulseRepo(t)
		dir := reapPulseLandedWorktree(t, root, "dirty-lane")
		// A receipt an agent left behind. Its branch is otherwise landed, so
		// only the cleanliness guard can save it.
		if err := os.WriteFile(filepath.Join(dir, "TASK-CONTEXT.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var offered []reapRow
		reapPulseRecordingRetirer(t, &offered)

		report, err := runReapPulseTick(context.Background(), root, "main", true)
		if err != nil {
			t.Fatalf("beat failed: %v", err)
		}
		if len(offered) != 0 || report.Landed != 0 || report.Retired != 0 {
			t.Fatalf("a worktree holding uncommitted evidence was offered for retirement: %+v offered=%v", report, offered)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("protected worktree disappeared: %v", statErr)
		}
	})

	t.Run("locked surface is never inspected or offered", func(t *testing.T) {
		root := reapPulseRepo(t)
		dir := reapPulseLandedWorktree(t, root, "locked-lane")
		reapPulseGit(t, root, "worktree", "lock", "--reason", "held by an operator", dir)
		var offered []reapRow
		reapPulseRecordingRetirer(t, &offered)

		report, err := runReapPulseTick(context.Background(), root, "main", true)
		if err != nil {
			t.Fatalf("beat failed: %v", err)
		}
		if len(offered) != 0 || report.Retired != 0 {
			t.Fatalf("a locked worktree was offered for retirement: %+v offered=%v", report, offered)
		}
		// A locked surface is excluded from registration metadata alone, so it
		// must not have consumed the beat's status budget either.
		if report.Eligible != 0 || report.Inspected != 0 {
			t.Fatalf("locked worktree consumed the inspection window: %+v", report)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("protected worktree disappeared: %v", statErr)
		}
	})

	t.Run("unknown status is kept", func(t *testing.T) {
		root := reapPulseRepo(t)
		dir := reapPulseLandedWorktree(t, root, "unknown-lane")
		original := reapStatusRunner
		t.Cleanup(func() { reapStatusRunner = original })
		reapStatusRunner = func(path string, args ...string) (string, error) {
			return "", fmt.Errorf("status is unreadable")
		}
		var offered []reapRow
		reapPulseRecordingRetirer(t, &offered)

		report, err := runReapPulseTick(context.Background(), root, "main", true)
		if err != nil {
			t.Fatalf("beat failed: %v", err)
		}
		if len(offered) != 0 || report.Landed != 0 || report.Retired != 0 {
			t.Fatalf("an unreadable worktree was offered for retirement: %+v offered=%v", report, offered)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("protected worktree disappeared: %v", statErr)
		}
	})

	t.Run("unique unmerged work is kept", func(t *testing.T) {
		root := reapPulseRepo(t)
		dir := reapPulseLandedWorktree(t, root, "unmerged-lane")
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("only copy\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		reapPulseGit(t, dir, "add", ".")
		reapPulseGit(t, dir, "commit", "-qm", "unique unmerged work")
		var offered []reapRow
		reapPulseRecordingRetirer(t, &offered)

		report, err := runReapPulseTick(context.Background(), root, "main", true)
		if err != nil {
			t.Fatalf("beat failed: %v", err)
		}
		if len(offered) != 0 || report.Landed != 0 || report.Retired != 0 {
			t.Fatalf("unmerged work was offered for retirement: %+v offered=%v", report, offered)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "work.txt")); statErr != nil {
			t.Fatalf("the only copy of unique work disappeared: %v", statErr)
		}
		if !reapPulseBranchExists(t, root, "unmerged-lane") {
			t.Fatal("the branch holding unique unmerged work was deleted")
		}
	})
}

// The budget is causal: with more landed worktrees than the per-beat bound,
// exactly the bound may be retired. An unbounded beat would retire them all.
func TestReapPulseHonorsRetireBudgetPerBeat(t *testing.T) {
	root := reapPulseRepo(t)
	total := reapPulseRetireBudget + 3
	for i := 0; i < total; i++ {
		reapPulseLandedWorktree(t, root, fmt.Sprintf("landed-lane-%02d", i))
	}
	var offered []reapRow
	reapPulseRecordingRetirer(t, &offered)

	report, err := runReapPulseTick(context.Background(), root, "main", true)
	if err != nil {
		t.Fatalf("beat failed: %v", err)
	}
	if report.Landed != total {
		t.Fatalf("classification should see every landed worktree in the window: %+v", report)
	}
	if len(offered) != reapPulseRetireBudget || report.Retired != reapPulseRetireBudget {
		t.Fatalf("beat exceeded its retirement budget %d: retired=%d offered=%d",
			reapPulseRetireBudget, report.Retired, len(offered))
	}
}

// A dry run must leave the repository byte-identical: nothing retired, and no
// cursor written. Persisting a cursor from an observe-only pass would silently
// skip that slice on the next acting beat.
func TestReapPulseDryRunRemovesNothingAndWritesNoCursor(t *testing.T) {
	root := reapPulseRepo(t)
	dir := reapPulseLandedWorktree(t, root, "dry-run-lane")
	var offered []reapRow
	reapPulseRecordingRetirer(t, &offered)

	report, err := runReapPulseTick(context.Background(), root, "main", false)
	if err != nil {
		t.Fatalf("beat failed: %v", err)
	}
	if report.Landed != 1 {
		t.Fatalf("dry run must still classify the landed worktree: %+v", report)
	}
	if len(offered) != 0 || report.Retired != 0 {
		t.Fatalf("dry run retired something: %+v offered=%v", report, offered)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("dry run removed %s: %v", dir, statErr)
	}
	if _, statErr := os.Stat(reapPulseStatePath(root, reapPulseCursorFile)); !os.IsNotExist(statErr) {
		t.Fatalf("dry run persisted a rotation cursor (err=%v)", statErr)
	}
}

// The whole point of the cursor: one beat pays status for at most the window,
// and the next beat continues past it instead of re-inspecting the same head
// of the fleet forever.
func TestReapPulseBoundsInspectionAndRotatesAcrossBeats(t *testing.T) {
	root := reapPulseRepo(t)
	total := reapPulseInspectWindow + 4
	for i := 0; i < total; i++ {
		reapPulseLandedWorktree(t, root, fmt.Sprintf("rotate-lane-%02d", i))
	}
	var inspectedCounts []int
	var windows [][]string
	originalInspector := reapEntryInspector
	t.Cleanup(func() { reapEntryInspector = originalInspector })
	reapEntryInspector = func(entries []worktreeEntry) ([]worktreeEntry, error) {
		inspectedCounts = append(inspectedCounts, len(entries))
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			paths = append(paths, entry.Path)
		}
		windows = append(windows, paths)
		return originalInspector(entries)
	}
	var offered []reapRow
	reapPulseRecordingRetirer(t, &offered)

	first, err := runReapPulseTick(context.Background(), root, "main", true)
	if err != nil {
		t.Fatalf("first beat failed: %v", err)
	}
	second, err := runReapPulseTick(context.Background(), root, "main", true)
	if err != nil {
		t.Fatalf("second beat failed: %v", err)
	}
	if first.Registered <= reapPulseInspectWindow {
		t.Fatalf("fixture is vacuous: only %d registrations", first.Registered)
	}
	for beat, count := range inspectedCounts {
		if count > reapPulseInspectWindow {
			t.Fatalf("beat %d statused %d registrations, exceeding the window %d: a scheduled beat must never sweep the fleet",
				beat, count, reapPulseInspectWindow)
		}
	}
	if len(windows) != 2 {
		t.Fatalf("expected two inspection windows, got %d", len(windows))
	}
	// The second beat must RESUME past the first beat's cursor, not restart at
	// the head of the fleet. Without a persisted cursor both windows would be
	// identical and this comparison fails.
	if windows[1][0] <= first.NextCursor {
		t.Fatalf("second beat restarted at %s instead of resuming past cursor %s", windows[1][0], first.NextCursor)
	}
	// Everything not yet visited must be visited before the rotation wraps: the
	// fresh remainder leads the second window.
	fresh := first.Eligible - len(windows[0])
	if fresh <= 0 || fresh > len(windows[1]) {
		t.Fatalf("fixture should leave unvisited registrations: eligible=%d firstWindow=%d", first.Eligible, len(windows[0]))
	}
	seen := map[string]bool{}
	for _, path := range windows[0] {
		seen[path] = true
	}
	for _, path := range windows[1][:fresh] {
		if seen[path] {
			t.Fatalf("second beat re-inspected %s before visiting the rest of the fleet (first=%v second=%v)", path, windows[0], windows[1])
		}
	}
	if second.NextCursor == first.NextCursor {
		t.Fatalf("cursor did not move between beats: %q", second.NextCursor)
	}
}

// A cancelled beat abandons its queued retirements instead of starting
// destructive work for a caller that has already gone away.
func TestReapPulseCancelledBeatAbandonsQueuedRetirements(t *testing.T) {
	root := reapPulseRepo(t)
	dir := reapPulseLandedWorktree(t, root, "cancelled-lane")
	var offered []reapRow
	reapPulseRecordingRetirer(t, &offered)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := runReapPulseTick(ctx, root, "main", true)
	if err == nil {
		t.Fatal("a cancelled beat must report its cancellation, not a clean result")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must surface as context.Canceled, got: %v", err)
	}
	if len(offered) != 0 || report.Retired != 0 {
		t.Fatalf("a cancelled beat still retired queued work: %+v offered=%v", report, offered)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("cancelled beat removed %s: %v", dir, statErr)
	}
}

// The persisted cursor must be relative to the repository root: an absolute
// path in durable state pins the repo to one machine's layout and makes every
// registration compare unequal after a move.
func TestReapPulseCursorIsPersistedRelativeToRoot(t *testing.T) {
	root := reapPulseRepo(t)
	dir := reapPulseLandedWorktree(t, root, "cursor-lane")
	var offered []reapRow
	reapPulseRecordingRetirer(t, &offered)

	if _, err := runReapPulseTick(context.Background(), root, "main", true); err != nil {
		t.Fatalf("beat failed: %v", err)
	}
	raw, err := os.ReadFile(reapPulseStatePath(root, reapPulseCursorFile))
	if err != nil {
		t.Fatalf("acting beat did not persist a cursor: %v", err)
	}
	if strings.Contains(string(raw), root) || strings.Contains(string(raw), string(filepath.Separator)+"private") {
		t.Fatalf("cursor persisted an absolute path: %s", raw)
	}
	if !strings.Contains(string(raw), "cursor-lane") {
		t.Fatalf("cursor did not record the visited registration: %s", raw)
	}
	// The relative form must still resolve back to the observed identity.
	restored, err := readReapPulseCursor(root)
	if err != nil {
		t.Fatal(err)
	}
	if restored != filepath.Clean(dir) {
		t.Fatalf("relative cursor did not round-trip: got %q want %q", restored, dir)
	}
}

// The base is configuration, not a constant: a project whose default branch is
// not "main" must be compared against its own integration ref.
func TestReapPulseBaseRefFollowsConfiguredDefaultBranch(t *testing.T) {
	root := t.TempDir()
	if got := reapPulseBaseRef(root); got != "origin/main" {
		t.Fatalf("an unconfigured project must fall back to the repository default: got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"),
		[]byte("version: \"1\"\nproject:\n  name: fixture\n  default_branch: trunk\ntask_provider:\n  type: memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := reapPulseBaseRef(root); got != "origin/trunk" {
		t.Fatalf("configured default branch ignored: got %q want %q", got, "origin/trunk")
	}

	// A config that does not parse or validate must not invent a base: it falls
	// back to the repository default, and an unresolvable base keeps every
	// worktree rather than retiring against a ref nobody confirmed.
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte("project:\n  default_branch: trunk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := reapPulseBaseRef(root); got != "origin/main" {
		t.Fatalf("an invalid config must fall back, not adopt an unvalidated branch: got %q", got)
	}
}

// A beat started from inside a linked worktree must never offer the
// repository's own checkout: there, IsMain is set relative to the beat's own
// directory, so identity is the only thing standing between a schedule and the
// main checkout.
func TestReapPulseNeverOffersTheRepositoryCheckout(t *testing.T) {
	root := reapPulseRepo(t)
	lane := reapPulseLandedWorktree(t, root, "linked-lane")
	var offered []reapRow
	reapPulseRecordingRetirer(t, &offered)

	// Run the beat FROM the linked worktree, which is what a stray
	// `herd pulse --act` inside a task worktree would do.
	report, err := runReapPulseTick(context.Background(), lane, "main", true)
	if err != nil {
		t.Fatalf("beat failed: %v", err)
	}
	for _, row := range offered {
		if reapPulseSamePath(row.Path, root) {
			t.Fatalf("the repository's own checkout was offered for retirement: %+v", row)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, "base.txt")); statErr != nil {
		t.Fatalf("the repository checkout was damaged: %v", statErr)
	}
	if report.Registered < 2 {
		t.Fatalf("fixture is vacuous: %+v", report)
	}
}

// Two cleanup beats must never run together. The loser defers, and the
// pulse-facing hook reports that deferral as success rather than a failure.
func TestReapPulseDefersToConcurrentCleanupBeat(t *testing.T) {
	root := reapPulseRepo(t)
	reapPulseLandedWorktree(t, root, "contended-lane")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := acquireReapPulseTickLock(reapPulseStatePath(root, reapPulseLockFile))
	if err != nil {
		t.Fatalf("could not hold the tick lock: %v", err)
	}
	defer func() { _ = held.Close() }()

	var offered []reapRow
	reapPulseRecordingRetirer(t, &offered)
	if _, err := runReapPulseTick(context.Background(), root, "main", true); err == nil {
		t.Fatal("a second concurrent beat acquired the tick lock")
	} else if !strings.Contains(err.Error(), errReapPulseTickBusy.Error()) {
		t.Fatalf("deferral must be reported as busy, got: %v", err)
	}
	if len(offered) != 0 {
		t.Fatalf("a deferred beat still offered retirements: %v", offered)
	}

	t.Setenv("HERD_ROOT", root)
	logFile, err := os.CreateTemp(t.TempDir(), "pulse-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	if !reapLandedWorktreesOnPulse(context.Background(), logFile) {
		t.Fatal("losing the tick race is benign and must not fail the pulse beat")
	}
}
