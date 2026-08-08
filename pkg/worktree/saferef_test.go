package worktree

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestSafeRefFor verifies the safe ref naming convention matches AnchorRefFor:
// lowercased task ref so macOS case-insensitive storage cannot alias
// FAC-172 and fac-172.
func TestSafeRefFor(t *testing.T) {
	if got := SafeRefFor("FAC-214"); got != "refs/herd/safe/fac-214" {
		t.Fatalf("SafeRefFor(FAC-214) = %q, want refs/herd/safe/fac-214", got)
	}
	if got := SafeRefFor("fac-214"); got != "refs/herd/safe/fac-214" {
		t.Fatalf("SafeRefFor(fac-214) = %q, want refs/herd/safe/fac-214", got)
	}
}

// TestCreateTaskWorktree_WritesInitialSafeRef proves CreateTaskWorktree
// writes refs/herd/safe/<task> at the anchor commit tip. Without this, a
// lane that runs `git reset --hard origin/main` before the coordinator
// advances the safe ref has no recovery ref at all.
func TestCreateTaskWorktree_WritesInitialSafeRef(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	if wi.SafeRef != SafeRefFor("FAC-214") {
		t.Fatalf("SafeRef = %q, want %q", wi.SafeRef, SafeRefFor("FAC-214"))
	}
	safeSHA := gitOut(t, tmpDir, "rev-parse", "--verify", wi.SafeRef)
	if safeSHA == "" {
		t.Fatal("safe ref must exist after CreateTaskWorktree")
	}
	if safeSHA != wi.Commit {
		t.Fatalf("safe ref SHA = %s, want worktree HEAD %s", safeSHA, wi.Commit)
	}
}

// TestWriteSafeRef_CreatesAndAdvances proves WriteSafeRef creates the ref
// when absent and advances it when the tip moves. A regression that makes
// WriteSafeRef a no-op or refuses to advance would fail here.
func TestWriteSafeRef_CreatesAndAdvances(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-A")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	initial := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if err := wm.WriteSafeRef(context.Background(), "FAC-214-A", initial); err != nil {
		t.Fatalf("WriteSafeRef initial: %v", err)
	}
	got := gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-A"))
	if got != initial {
		t.Fatalf("after initial write: safe ref = %s, want %s", got, initial)
	}

	// Commit real work and advance the safe ref.
	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: real work"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	advanced := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if advanced == initial {
		t.Fatal("setup failed: HEAD did not advance after commit")
	}
	if err := wm.WriteSafeRef(context.Background(), "FAC-214-A", advanced); err != nil {
		t.Fatalf("WriteSafeRef advance: %v", err)
	}
	got = gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-A"))
	if got != advanced {
		t.Fatalf("after advance: safe ref = %s, want %s", got, advanced)
	}
}

// TestWriteSafeRef_RejectsEmptyInputs proves the fail-closed guards: an
// empty task ref or empty SHA is a hard error, not a silent skip.
func TestWriteSafeRef_RejectsEmptyInputs(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	if err := wm.WriteSafeRef(context.Background(), "", "abc"); err == nil {
		t.Fatal("empty task ref must fail")
	}
	if err := wm.WriteSafeRef(context.Background(), "FAC-214", ""); err == nil {
		t.Fatal("empty head SHA must fail")
	}
}

// TestReadSafeRef_ReturnsSHAOrEmpty proves ReadSafeRef returns the SHA when
// the ref exists and an empty string (not an error) when it does not.
func TestReadSafeRef_ReturnsSHAOrEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	// No safe ref yet → empty, no error.
	got, err := wm.ReadSafeRef(context.Background(), "FAC-214-R")
	if err != nil {
		t.Fatalf("ReadSafeRef on absent ref must not error: %v", err)
	}
	if got != "" {
		t.Fatalf("ReadSafeRef on absent ref = %q, want empty", got)
	}

	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-R")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	got, err = wm.ReadSafeRef(context.Background(), "FAC-214-R")
	if err != nil {
		t.Fatalf("ReadSafeRef after create: %v", err)
	}
	if got != wi.Commit {
		t.Fatalf("ReadSafeRef = %s, want %s", got, wi.Commit)
	}
}

// TestSafeRef_SurvivesHardReset is the core FAC-214 regression test. It
// reproduces the exact sequence that destroyed commits on FAC-172 and
// FAC-133: a lane commits real work, then runs `git reset --hard
// origin/main`. Without a safe ref the commits are reachable only from
// reflog. With a safe ref they remain reachable from a durable ref that
// the reset cannot touch.
//
// A regression that makes the safe ref a branch alias (moved by reset) or
// fails to write it would fail the "still reachable" assertion.
func TestSafeRef_SurvivesHardReset(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-S")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Lane commits real work.
	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: real lane work FAC-214-S"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tipSHA := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if tipSHA == "" {
		t.Fatal("setup failed: no HEAD after commit")
	}

	// Coordinator captures the tip in the safe ref before any rebase.
	if err := wm.WriteSafeRef(context.Background(), "FAC-214-S", tipSHA); err != nil {
		t.Fatalf("WriteSafeRef: %v", err)
	}

	// Lane runs the destructive sequence: rebase --abort then reset --hard.
	_ = runCmd(wi.Path, "git", "rebase", "--abort")
	originMain := gitOut(t, wi.Path, "rev-parse", "origin/main")
	if err := runCmd(wi.Path, "git", "reset", "--hard", "origin/main"); err != nil {
		t.Fatalf("reset --hard origin/main: %v", err)
	}
	headAfter := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if headAfter != originMain {
		t.Fatalf("reset did not move HEAD to origin/main: HEAD=%s origin/main=%s", headAfter, originMain)
	}

	// The safe ref must still point at the captured tip — the reset cannot
	// touch refs/herd/safe/<task> because it is not the branch ref.
	safeSHA := gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-S"))
	if safeSHA != tipSHA {
		t.Fatalf("safe ref = %s after reset, want captured tip %s — safe ref must survive git reset --hard", safeSHA, tipSHA)
	}

	// The real work commit must still be reachable from the safe ref.
	subject := gitOut(t, tmpDir, "log", "-1", "--format=%s", SafeRefFor("FAC-214-S"))
	if !strings.Contains(subject, "real lane work") {
		t.Fatalf("safe ref tip subject = %q, expected real lane work commit", subject)
	}
}

// TestSafeRef_AbsentMeansUnreachable proves the safe ref is structurally
// necessary, not decorative: without it, the same `git reset --hard
// origin/main` makes the real work commit unreachable from any ref (only
// reflog keeps it alive). This is the negative assertion that proves the
// safe ref is not vacuous coverage — if the safe ref did nothing, this test
// would show the commit is reachable, which would be wrong.
func TestSafeRef_AbsentMeansUnreachable(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	// Suppress the initial safe ref by using a manager whose execCommandContext
	// is mocked to skip the safe-ref update-ref. We cannot easily isolate the
	// safe ref write from CreateTaskWorktree, so instead we create the worktree
	// normally and then delete the safe ref to simulate a pre-FAC-214 lane.
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-U")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Remove the safe ref to simulate a pre-FAC-214 lane.
	_ = runCmd(tmpDir, "git", "update-ref", "-d", SafeRefFor("FAC-214-U"))

	// Lane commits real work (no safe ref capture).
	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: unprotected work FAC-214-U"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tipSHA := gitOut(t, wi.Path, "rev-parse", "HEAD")

	// Destructive reset.
	if err := runCmd(wi.Path, "git", "reset", "--hard", "origin/main"); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// No ref should contain the tip commit — it is unreachable from all refs.
	refsContaining := gitOut(t, tmpDir, "for-each-ref", "--contains", tipSHA, "--format=%(refname:short)")
	if refsContaining != "" {
		t.Fatalf("tip %s is reachable from refs [%s] — test setup did not reproduce the unreachability that makes the safe ref necessary", tipSHA, refsContaining)
	}

	// The safe ref must not exist.
	if got := gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-U")); got != "" {
		t.Fatalf("safe ref should not exist for pre-FAC-214 lane, got %s", got)
	}
}

// TestDetectDroppedWork_AfterHardReset proves DetectDroppedWork alarms when
// a lane's HEAD has fallen back to origin/main while the safe ref still
// holds divergent commits — the exact signature of the FAC-172/FAC-133
// data loss.
func TestDetectDroppedWork_AfterHardReset(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-D")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Lane commits real work and coordinator captures it.
	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: dropped work FAC-214-D"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tipSHA := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if err := wm.WriteSafeRef(context.Background(), "FAC-214-D", tipSHA); err != nil {
		t.Fatalf("WriteSafeRef: %v", err)
	}

	originMain := gitOut(t, wi.Path, "rev-parse", "origin/main")

	// Destructive reset.
	if err := runCmd(wi.Path, "git", "reset", "--hard", "origin/main"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	headAfter := gitOut(t, wi.Path, "rev-parse", "HEAD")

	report, err := wm.DetectDroppedWork(context.Background(), "FAC-214-D", headAfter, originMain)
	if err != nil {
		t.Fatalf("DetectDroppedWork: %v", err)
	}
	if !report.Dropped {
		t.Fatal("Dropped must be true after reset to origin/main with divergent safe ref")
	}
	if !report.Recoverable {
		t.Fatal("Recoverable must be true when the safe ref is still resolvable")
	}
	if report.SafeRefSHA != tipSHA {
		t.Fatalf("SafeRefSHA = %s, want captured tip %s", report.SafeRefSHA, tipSHA)
	}
	foundWork := false
	for _, s := range report.UniqueSubjects {
		if strings.Contains(s, "dropped work") {
			foundWork = true
			break
		}
	}
	if !foundWork {
		t.Fatalf("UniqueSubjects = %v, expected to contain the dropped work subject", report.UniqueSubjects)
	}
}

// TestDetectDroppedWork_HeadRetained proves DetectDroppedWork does NOT
// alarm when the lane still holds its work (HEAD diverges from origin/main).
// A regression that always sets Dropped=true would fail here.
func TestDetectDroppedWork_HeadRetained(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-H")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: retained work"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tipSHA := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if err := wm.WriteSafeRef(context.Background(), "FAC-214-H", tipSHA); err != nil {
		t.Fatalf("WriteSafeRef: %v", err)
	}

	originMain := gitOut(t, wi.Path, "rev-parse", "origin/main")
	head := gitOut(t, wi.Path, "rev-parse", "HEAD")

	report, err := wm.DetectDroppedWork(context.Background(), "FAC-214-H", head, originMain)
	if err != nil {
		t.Fatalf("DetectDroppedWork: %v", err)
	}
	if report.Dropped {
		t.Fatal("Dropped must be false when HEAD != origin/main (lane still holds work)")
	}
}

// TestDetectDroppedWork_NoSafeRef proves DetectDroppedWork returns
// Dropped=false when no safe ref exists (a pre-FAC-214 lane). This is NOT
// a false negative — it is an honest "cannot determine" that tells the
// coordinator the lane has no recovery baseline.
func TestDetectDroppedWork_NoSafeRef(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-N")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Delete the safe ref to simulate a pre-FAC-214 lane.
	_ = runCmd(tmpDir, "git", "update-ref", "-d", SafeRefFor("FAC-214-N"))

	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: work"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	originMain := gitOut(t, wi.Path, "rev-parse", "origin/main")
	if err := runCmd(wi.Path, "git", "reset", "--hard", "origin/main"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	headAfter := gitOut(t, wi.Path, "rev-parse", "HEAD")

	report, err := wm.DetectDroppedWork(context.Background(), "FAC-214-N", headAfter, originMain)
	if err != nil {
		t.Fatalf("DetectDroppedWork: %v", err)
	}
	if report.Dropped {
		t.Fatal("Dropped must be false when no safe ref exists — cannot determine without a baseline")
	}
	if report.SafeRefSHA != "" {
		t.Fatalf("SafeRefSHA = %q, want empty when no safe ref exists", report.SafeRefSHA)
	}
}

// TestDetectDroppedWork_HeadAtOriginMain_NoDivergence proves the edge case
// where HEAD == origin/main and the safe ref also == origin/main (e.g., a
// freshly created lane with no real work). This must NOT alarm.
func TestDetectDroppedWork_HeadAtOriginMain_NoDivergence(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-E")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	originMain := gitOut(t, wi.Path, "rev-parse", "origin/main")
	// Reset the worktree to origin/main (no real work was ever committed).
	if err := runCmd(wi.Path, "git", "reset", "--hard", "origin/main"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	headAfter := gitOut(t, wi.Path, "rev-parse", "HEAD")

	// The safe ref was written at creation to the anchor commit, which is
	// NOT origin/main (it's an empty commit on top). But we can simulate
	// the no-divergence case by pointing the safe ref at origin/main.
	if err := wm.WriteSafeRef(context.Background(), "FAC-214-E", originMain); err != nil {
		t.Fatalf("WriteSafeRef: %v", err)
	}

	report, err := wm.DetectDroppedWork(context.Background(), "FAC-214-E", headAfter, originMain)
	if err != nil {
		t.Fatalf("DetectDroppedWork: %v", err)
	}
	if report.Dropped {
		t.Fatal("Dropped must be false when safe ref == origin/main (no divergent work to lose)")
	}
}
