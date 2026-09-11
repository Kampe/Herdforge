package resources

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func landingSeamRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	landingSeamGit(t, root, "init", "-q", "-b", "main", ".")
	landingSeamGit(t, root, "config", "user.email", "seam@herdforge.local")
	landingSeamGit(t, root, "config", "user.name", "seam")
	landingSeamGit(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	landingSeamGit(t, root, "add", ".")
	landingSeamGit(t, root, "commit", "-qm", "base")
	return root
}

func landingSeamGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seam", "GIT_AUTHOR_EMAIL=seam@herdforge.local",
		"GIT_COMMITTER_NAME=seam", "GIT_COMMITTER_EMAIL=seam@herdforge.local",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// landingSeamLane adds a worktree holding one unique commit, so ancestry
// reports it unmerged and the content predicate is the only thing that can
// change the verdict.
func landingSeamLane(t *testing.T, root, name string) RegisteredWorktree {
	t.Helper()
	dir := filepath.Join(root, name)
	landingSeamGit(t, root, "worktree", "add", "-q", "-b", name, dir)
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	landingSeamGit(t, dir, "add", ".")
	landingSeamGit(t, dir, "commit", "-qm", "work on "+name)
	head := strings.TrimSpace(landingSeamGit(t, dir, "rev-parse", "HEAD"))
	return RegisteredWorktree{Path: dir, Branch: name, Head: head}
}

func landingSeamEnumerator(landing LandingPredicate) GitWorktreeEnumerator {
	return GitWorktreeEnumerator{Now: time.Now, HostID: "seam", Landing: landing}
}

// A nil predicate must behave exactly as the census always has: ancestry only.
// This is what keeps every consumer that does not wire one unchanged,
// including the governor's own default enumerator.
func TestLandingSeamDefaultsToAncestryWhenUnwired(t *testing.T) {
	root := landingSeamRepo(t)
	lane := landingSeamLane(t, root, "unwired-lane")

	merged, err := landingSeamEnumerator(nil).landed(context.Background(), lane, "main", "")
	if err != nil {
		t.Fatalf("ancestry default must answer: %v", err)
	}
	if merged {
		t.Fatal("a lane with a unique commit is not an ancestor of the base")
	}
}

// Ancestry stays the authoritative cheap yes: a predicate must not be paid for
// a lane the base already contains.
func TestLandingSeamSkipsPredicateWhenAncestryAlreadyProves(t *testing.T) {
	root := landingSeamRepo(t)
	dir := filepath.Join(root, "ancestor-lane")
	landingSeamGit(t, root, "worktree", "add", "-q", "-b", "ancestor-lane", dir)
	head := strings.TrimSpace(landingSeamGit(t, dir, "rev-parse", "HEAD"))
	lane := RegisteredWorktree{Path: dir, Branch: "ancestor-lane", Head: head}

	called := 0
	merged, err := landingSeamEnumerator(func(context.Context, LandingProbe) (bool, error) {
		called++
		return false, errors.New("predicate must not run for an ancestor")
	}).landed(context.Background(), lane, "main", "deadbeef")
	if err != nil || !merged {
		t.Fatalf("ancestry must answer yes without the predicate: merged=%v err=%v", merged, err)
	}
	if called != 0 {
		t.Fatalf("predicate ran %d time(s) for a lane ancestry already proved", called)
	}
}

// The probe must carry PINNED identities, not refs that can move underneath
// the proof.
func TestLandingSeamPinsHeadAndBaseIntoTheProbe(t *testing.T) {
	root := landingSeamRepo(t)
	lane := landingSeamLane(t, root, "pinned-lane")
	basePin := strings.TrimSpace(landingSeamGit(t, root, "rev-parse", "main"))

	var seen LandingProbe
	merged, err := landingSeamEnumerator(func(_ context.Context, probe LandingProbe) (bool, error) {
		seen = probe
		return true, nil
	}).landed(context.Background(), lane, "main", basePin)
	if err != nil || !merged {
		t.Fatalf("a predicate that proves landing must be believed: merged=%v err=%v", merged, err)
	}
	if seen.HeadSHA != lane.Head || seen.BaseSHA != basePin {
		t.Fatalf("probe was not pinned: head=%q want %q base=%q want %q", seen.HeadSHA, lane.Head, seen.BaseSHA, basePin)
	}
	if seen.Branch != lane.Branch || seen.WorktreePath != lane.Path || seen.BaseRef != "main" {
		t.Fatalf("probe identity is incomplete: %+v", seen)
	}
}

// An unpinnable base refuses rather than proving against a ref that may move.
func TestLandingSeamRefusesWithoutPinnedBase(t *testing.T) {
	root := landingSeamRepo(t)
	lane := landingSeamLane(t, root, "unpinned-lane")

	called := 0
	merged, err := landingSeamEnumerator(func(context.Context, LandingProbe) (bool, error) {
		called++
		return true, nil
	}).landed(context.Background(), lane, "main", "  ")
	if err == nil || merged {
		t.Fatalf("an unpinned base must be unknown: merged=%v err=%v", merged, err)
	}
	if called != 0 {
		t.Fatalf("predicate ran %d time(s) without a pinned base", called)
	}
}

// A predicate error is UNKNOWN and never lands the lane.
func TestLandingSeamPredicateErrorIsUnknown(t *testing.T) {
	root := landingSeamRepo(t)
	lane := landingSeamLane(t, root, "unknown-lane")
	basePin := strings.TrimSpace(landingSeamGit(t, root, "rev-parse", "main"))

	merged, err := landingSeamEnumerator(func(context.Context, LandingProbe) (bool, error) {
		return true, errors.New("proof is unreadable")
	}).landed(context.Background(), lane, "main", basePin)
	if err == nil {
		t.Fatal("a predicate error must surface as unknown")
	}
	if merged {
		t.Fatal("a predicate that errored must never land the lane, even when it also returned true")
	}
}

// Cancellation is observed on BOTH sides of the proof: a cancelled census does
// not start one, and does not trust one that finished after the caller gave up.
func TestLandingSeamObservesCancellationAroundTheProof(t *testing.T) {
	root := landingSeamRepo(t)
	lane := landingSeamLane(t, root, "cancelled-lane")
	basePin := strings.TrimSpace(landingSeamGit(t, root, "rev-parse", "main"))

	t.Run("cancelled before the proof does not start it", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := 0
		merged, err := landingSeamEnumerator(func(context.Context, LandingProbe) (bool, error) {
			called++
			return true, nil
		}).landed(ctx, lane, "main", basePin)
		if !errors.Is(err, context.Canceled) || merged {
			t.Fatalf("cancelled census must refuse: merged=%v err=%v", merged, err)
		}
		if called != 0 {
			t.Fatalf("predicate ran %d time(s) after cancellation", called)
		}
	})

	t.Run("cancelled during the proof discards its result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		merged, err := landingSeamEnumerator(func(context.Context, LandingProbe) (bool, error) {
			// The caller gives up while the (uninterruptible) proof runs.
			cancel()
			return true, nil
		}).landed(ctx, lane, "main", basePin)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a result produced after cancellation must not be trusted: err=%v", err)
		}
		if merged {
			t.Fatal("a lane was landed on a proof its caller had already abandoned")
		}
	})
}

// countingLanes is the enumerator seam used to prove the ACT recheck re-lists
// through the same interface the census used, rather than answering from a
// second, ancestry-only path of its own.
type countingLanes struct {
	mu    sync.Mutex
	lanes []RegisteredWorktree
	calls int
}

func (c *countingLanes) List(context.Context, string, string) ([]RegisteredWorktree, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return append([]RegisteredWorktree(nil), c.lanes...), nil
}

func (c *countingLanes) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// The census and the act recheck must share ONE landing definition. They do
// because both reach it through WorktreeEnumerator.List: applyTargets
// re-lists and re-inspects rather than consulting a separate merge check. If
// a future change gave the act path its own ancestry-only lookup, this count
// would stop rising.
func TestLandingSeamActRecheckRelistsThroughTheSameEnumerator(t *testing.T) {
	root := landingSeamRepo(t)
	lane := landingSeamLane(t, root, "recheck-lane")
	lane.Category, lane.State = LaneTask, LaneIdle
	target := filepath.Join(lane.Path, "generated")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "artifact.bin"), []byte("regenerable\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The relisted lane carries the verdict the landing predicate produces.
	// Unmerged is what laneBlockReason consults, so this is the exact state a
	// refused proof leaves behind.
	lane.Unmerged = true
	lanes := &countingLanes{lanes: []RegisteredWorktree{lane}}
	g := &Governor{
		Policy: GovernorPolicy{
			RepositoryRoot: root, BaseRef: "main", GeneratedDirectories: []string{"generated"},
			ReapBatchLimit: 1,
		},
		Worktrees: lanes,
	}
	report := &GovernorReport{Targets: []TargetReport{{
		WorktreePath: lane.Path, Path: target, RelativePath: "generated", Decision: TargetWouldReap,
	}}}

	before := lanes.count()
	// The act path must consult the enumerator again. Removal itself is not
	// exercised here: the recheck is the assertion, and this lane is blocked
	// by its own unmerged state, which is exactly the protection under test.
	_ = g.applyTargets(context.Background(), report, 1)
	if lanes.count() <= before {
		t.Fatal("act recheck did not re-list through the enumerator: it is answering landing from somewhere else")
	}
	if report.Targets[0].Decision == TargetReaped {
		t.Fatalf("an unmerged lane's artifacts were reaped by the act path: %+v", report.Targets[0])
	}
}
