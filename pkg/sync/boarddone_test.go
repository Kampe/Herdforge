package sync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func gitInTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFileTest(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"FAC-018":  "FAC-18",
		"FAC-648":  "FAC-648",
		"FAC-0648": "FAC-648",
		"FAC-61":   "FAC-61",
		"CHA-018":  "CHA-18",
	}
	for in, want := range cases {
		if got := NormalizeRef(in); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// fixtureRepo builds a repo whose origin/main carries one commit naming
// FAC-18, plus a dangling branch commit that is NOT on origin/main.
func fixtureRepo(t *testing.T) (dir, mainSHA, strandedSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@herdforge.local")
	run("config", "user.name", "herdforge-test")
	// Disable GPG signing for fixture repos — 1Password SSH agent may not be available.
	run("config", "commit.gpgsign", "false")
	run("config", "tag.gpgsign", "false")
	run("commit", "--allow-empty", "-q", "-m", "feat: land board-done gate (FAC-18)")
	mainSHA = run("rev-parse", "HEAD")
	// Simulate origin/main without a network remote.
	run("update-ref", "refs/remotes/origin/main", mainSHA)
	// A commit that never reached origin/main.
	run("checkout", "-q", "-b", "stranded")
	run("commit", "--allow-empty", "-q", "-m", "feat: unmerged work (FAC-99)")
	strandedSHA = run("rev-parse", "HEAD")
	return dir, mainSHA, strandedSHA
}

func TestLandedProof(t *testing.T) {
	// landedRepo builds a bare origin, a main working clone, and a lane
	// worktree. The lane starts with one unique commit NOT on origin/main.
	// Tests merge the lane's work onto main in various modes and then call
	// LandedProof to verify it detects the landing.
	landedRepo := func(t *testing.T) (root, origin, lane string) {
		t.Helper()
		tmp := t.TempDir()
		origin = filepath.Join(tmp, "origin.git")
		root = filepath.Join(tmp, "work")
		lane = filepath.Join(tmp, "lane")

		run := func(d string, args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = d
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v in %s: %v\n%s", args, d, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		write := func(d, name, body string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		// Bare origin.
		run(tmp, "init", "-q", "--bare", "-b", "main", origin)
		// Main working clone.
		run(tmp, "clone", "-q", origin, root)
		run(root, "config", "user.email", "test@herdforge.local")
		run(root, "config", "user.name", "herdforge-test")
		run(root, "config", "commit.gpgsign", "false")
		run(root, "config", "tag.gpgsign", "false")
		write(root, "base.txt", "base\n")
		run(root, "add", "-A")
		run(root, "commit", "-q", "-m", "chore: base")
		run(root, "push", "-q", "origin", "main")

		// Lane worktree off origin/main.
		run(root, "worktree", "add", "-b", "herd/lane", lane, "origin/main")
		run(lane, "config", "user.email", "test@herdforge.local")
		run(lane, "config", "user.name", "herdforge-test")
		run(lane, "config", "commit.gpgsign", "false")
		write(lane, "feature.txt", "the actual work\n")
		run(lane, "add", "-A")
		run(lane, "commit", "-q", "-m", "feat: lane work (FAC-213)")
		return root, origin, lane
	}

	// mergeLaneToMain cherry-picks the lane tip onto main and pushes, so
	// origin/main carries the lane's content under the same SHAs (merge-commit
	// mode).
	mergeLaneToMain := func(t *testing.T, root, lane string) {
		t.Helper()
		laneSHA := gitInTest(t, lane, "rev-parse", "HEAD")
		gitInTest(t, root, "checkout", "-q", "main")
		gitInTest(t, root, "cherry-pick", laneSHA)
		gitInTest(t, root, "push", "-q", "origin", "main")
	}

	// rebaseMergeLaneToMain simulates GitHub's rebase-merge: replay the lane's
	// commits onto main with NEW SHAs, then push. The lane's original SHAs are
	// NOT on origin/main, but the PATCHES are.
	rebaseMergeLaneToMain := func(t *testing.T, root, lane string) {
		t.Helper()
		laneSHA := gitInTest(t, lane, "rev-parse", "HEAD")
		baseSHA := gitInTest(t, lane, "rev-parse", "origin/main")
		gitInTest(t, root, "checkout", "-q", "main")
		// Rebase the lane tip onto main: this creates new SHAs with the same
		// patches, simulating GitHub's rebase-merge.
		gitInTest(t, root, "rebase", "--onto", "main", baseSHA, laneSHA)
		// Move main to the rebased tip and push.
		newTip := gitInTest(t, root, "rev-parse", "HEAD")
		gitInTest(t, root, "branch", "-f", "main", newTip)
		gitInTest(t, root, "push", "-q", "origin", "main")
		// Return to main (rebase leaves HEAD detached).
		gitInTest(t, root, "checkout", "-q", "main")
	}

	// squashMergeLaneToMain simulates GitHub's squash-merge: collapse the
	// lane's commits into one commit on main, then push. The lane's individual
	// patch-ids do NOT match the squash commit.
	squashMergeLaneToMain := func(t *testing.T, root, lane string) {
		t.Helper()
		laneSHA := gitInTest(t, lane, "rev-parse", "HEAD")
		baseSHA := gitInTest(t, lane, "rev-parse", "origin/main")
		gitInTest(t, root, "checkout", "-q", "main")
		// Squash the lane's range into one commit on main.
		gitInTest(t, root, "merge", "--squash", laneSHA)
		gitInTest(t, root, "commit", "-q", "-m", "feat: squashed lane work (FAC-213)")
		gitInTest(t, root, "push", "-q", "origin", "main")
		_ = baseSHA // base is origin/main already
	}

	t.Run("merge-commit: work on main passes", func(t *testing.T) {
		root, _, lane := landedRepo(t)
		mergeLaneToMain(t, root, lane)
		if err := LandedProof(lane); err != nil {
			t.Fatalf("LandedProof must pass after merge-commit: %v", err)
		}
	})

	t.Run("rebase-merge: rewritten SHAs pass", func(t *testing.T) {
		root, _, lane := landedRepo(t)
		rebaseMergeLaneToMain(t, root, lane)
		// The lane's original SHAs are NOT on origin/main (they were
		// rewritten), but the patches are. LandedProof must still pass.
		if err := LandedProof(lane); err != nil {
			t.Fatalf("LandedProof must pass after rebase-merge (SHA rewrite): %v", err)
		}
	})

	t.Run("squash-merge: combined patch passes", func(t *testing.T) {
		root, _, lane := landedRepo(t)
		squashMergeLaneToMain(t, root, lane)
		// The lane's individual patch-ids do NOT match the squash commit.
		// LandedProof must still pass because the resulting tree matches.
		if err := LandedProof(lane); err != nil {
			t.Fatalf("LandedProof must pass after squash-merge: %v", err)
		}
	})

	t.Run("unmerged work fails", func(t *testing.T) {
		_, _, lane := landedRepo(t)
		// Do NOT merge the lane's work to main.
		err := LandedProof(lane)
		if err == nil {
			t.Fatal("LandedProof must fail when work is NOT on origin/main")
		}
		if !errors.Is(err, ErrNotLanded) {
			t.Fatalf("want ErrNotLanded, got %v", err)
		}
	})

	t.Run("grep false positive does not pass (defect 1)", func(t *testing.T) {
		root, origin, lane := landedRepo(t)
		// Add a commit to main whose BODY mentions FAC-213 but whose SUBJECT
		// names a different ref. The old grep would match FAC-213 in the body.
		gitInTest(t, root, "checkout", "-q", "main")
		writeFileTest(t, root, "unrelated.txt", "unrelated\n")
		gitInTest(t, root, "add", "-A")
		gitInTest(t, root, "commit", "-q", "-m", "fix: unrelated fix (FAC-999)", "-m", "relates to FAC-213")
		gitInTest(t, root, "push", "-q", "origin", "main")
		_ = origin
		// The lane's FAC-213 work is NOT on main — only a body mention is.
		// LandedProof must fail; the grep would have falsely passed.
		err := LandedProof(lane)
		if err == nil {
			t.Fatal("LandedProof must fail: a body mention of FAC-213 is not a merge")
		}
	})

	t.Run("stale remote branch does not cause false negative (defect 3)", func(t *testing.T) {
		root, origin, lane := landedRepo(t)
		// Push the lane branch to origin, then rebase the lane locally (new
		// SHAs) but do NOT force-push. origin/lane is now stale.
		gitInTest(t, root, "push", "-q", origin, "herd/lane:herd/lane")
		// Add a second commit to the lane (so the local lane diverges).
		writeFileTest(t, lane, "second.txt", "second\n")
		gitInTest(t, lane, "add", "-A")
		gitInTest(t, lane, "commit", "-q", "-m", "feat: second lane commit (FAC-213)")
		// Squash-merge ALL lane work to main.
		squashMergeLaneToMain(t, root, lane)
		// The remote origin/lane is stale (it only has the first commit, pre-
		// squash). LandedProof must still pass because it operates on the
		// LOCAL worktree, not the stale remote branch.
		if err := LandedProof(lane); err != nil {
			t.Fatalf("LandedProof must pass despite stale remote branch: %v", err)
		}
	})

	t.Run("fetch failure is a hard error", func(t *testing.T) {
		// A repo with no remote: fetch fails, and LandedProof must not
		// degrade to a stale local ref.
		dir := t.TempDir()
		gitInTest(t, dir, "init", "-q", "-b", "main")
		gitInTest(t, dir, "config", "user.email", "t@h.local")
		gitInTest(t, dir, "config", "user.name", "t")
		gitInTest(t, dir, "config", "commit.gpgsign", "false")
		gitInTest(t, dir, "commit", "--allow-empty", "-q", "-m", "base")
		err := LandedProof(dir)
		if err == nil {
			t.Fatal("LandedProof must fail when fetch fails (no remote)")
		}
		if !strings.Contains(err.Error(), "fetch") {
			t.Fatalf("fetch failure must mention fetch, got: %v", err)
		}
	})
}

func TestBoardDone(t *testing.T) {
	dir, _, _ := fixtureRepo(t)
	ctx := context.Background()

	newBoard := func(ref string) *provider.MemoryProvider {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{ID: "id-" + ref, Ref: ref, Title: "t", Status: "in-review", ProjectID: "p1", Description: testAcceptanceDescription})
		return mp
	}

	// FAC-132: the commit-subject oracle is gone. This fixture's origin/main
	// carries a commit naming FAC-18 — under the old contract that closed the
	// card. It must now refuse.
	t.Run("a commit naming the ref no longer closes it", func(t *testing.T) {
		mp := newBoard("FAC-18")
		_, err := BoardDone(ctx, mp, DoneRequest{RepoDir: dir, ProjectID: "p1", Ref: "FAC-018"})
		if !errors.Is(err, ErrNoEvidence) {
			t.Fatalf("want ErrNoEvidence refusal, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "id-FAC-18")
		if got.Status != "in-review" {
			t.Fatalf("refused card must not move, status = %q", got.Status)
		}
	})

	t.Run("refuses without any authority", func(t *testing.T) {
		mp := newBoard("FAC-99")
		_, err := BoardDone(ctx, mp, DoneRequest{RepoDir: dir, ProjectID: "p1", Ref: "FAC-99"})
		if !errors.Is(err, ErrNoEvidence) || !strings.Contains(err.Error(), "no completion receipt") {
			t.Fatalf("want ErrNoEvidence refusal, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "id-FAC-99")
		if got.Status != "in-review" {
			t.Fatalf("refused card must not move, status = %q", got.Status)
		}
	})

	t.Run("unknown ref errors even under a valid override", func(t *testing.T) {
		mp := newBoard("FAC-18")
		_, err := BoardDone(ctx, mp, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-500",
			Override: &OverrideRequest{Actor: "kampe", Reason: "scope withdrawn", Evidence: "n/a", Policy: "abandoned-scope"},
		})
		if err == nil {
			t.Fatal("unknown ref must error")
		}
	})

	// FAC-211: a card created directly (coordinator-authored, CI repair, split
	// ticket) may have an empty Ref field — `kaneo task create` has no --ref
	// flag. BoardDone must still close it when the provider's GetTask can
	// resolve the ref. The fallback through GetTask is what makes a card the
	// provider knows about closeable with evidence.
	t.Run("directly-created card is closeable via GetTask fallback", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		// Simulate a directly-created card: the kaneo UUID is the ID, Ref is
		// empty (kaneo task create does not set it), but GetTask can resolve
		// by the ref the coordinator assigned post-hoc.
		mp.AddTask(&provider.Task{
			ID: "kaneo-uuid-211", Ref: "FAC-211",
			Title: "fix board-done ref resolution", Status: "in-review", ProjectID: "p1",
			Description: testAcceptanceDescription,
		})
		// Use a provider wrapper that hides the Ref from ListTasks but exposes
		// it through GetTask — this is the directly-created-card shape.
		hidden := &refHiddenProvider{MemoryProvider: mp, hiddenRefs: map[string]bool{"kaneo-uuid-211": true}}
		_, err := BoardDone(ctx, hidden, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-211",
			AcceptanceEvidence: testAcceptanceEvidence,
			Override:           &OverrideRequest{Actor: "kampe", Reason: "directly created", Evidence: "sha-211", Policy: "abandoned-scope"},
		})
		if err != nil {
			t.Fatalf("directly-created card must be closeable via GetTask fallback, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "kaneo-uuid-211")
		if got.Status != "done" {
			t.Fatalf("card must be done, got %q", got.Status)
		}
	})

	t.Run("card invisible to both ListTasks and GetTask is refused", func(t *testing.T) {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{ID: "t-1", Ref: "FAC-1", Title: "t", Status: "to-do", ProjectID: "p1"})
		_, err := BoardDone(ctx, mp, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-999",
			Override: &OverrideRequest{Actor: "kampe", Reason: "test", Evidence: "n/a", Policy: "abandoned-scope"},
		})
		if err == nil || !strings.Contains(err.Error(), "no task with ref") {
			t.Fatalf("want 'no task with ref' refusal, got %v", err)
		}
	})
}

// refHiddenProvider wraps MemoryProvider and strips the Ref from ListTasks
// output for specified task IDs, while leaving GetTask untouched. This
// simulates a directly-created kaneo card whose Ref field is empty in list
// responses but resolvable through GetTask.
type refHiddenProvider struct {
	*provider.MemoryProvider
	hiddenRefs map[string]bool
}

func (r *refHiddenProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	tasks, err := r.MemoryProvider.ListTasks(ctx, projectID, status)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if r.hiddenRefs[t.ID] {
			t.Ref = ""
		}
	}
	return tasks, nil
}
