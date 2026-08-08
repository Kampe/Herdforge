package sync

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

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

func TestMergeEvidence(t *testing.T) {
	dir, mainSHA, strandedSHA := fixtureRepo(t)

	t.Run("commit naming ref is proof", func(t *testing.T) {
		proof, err := MergeEvidence(dir, "FAC-18", "")
		if err != nil || !strings.Contains(proof, "FAC-18") {
			t.Fatalf("want proof for FAC-18, got %q err %v", proof, err)
		}
	})

	t.Run("digit boundary: FAC-1 does not match FAC-18", func(t *testing.T) {
		proof, err := MergeEvidence(dir, "FAC-1", "")
		if err != nil || proof != "" {
			t.Fatalf("FAC-1 must not inherit FAC-18's commit, got %q err %v", proof, err)
		}
	})

	t.Run("explicit ancestor evidence is proof", func(t *testing.T) {
		proof, err := MergeEvidence(dir, "FAC-99", mainSHA)
		if err != nil || !strings.Contains(proof, "ancestor") {
			t.Fatalf("want ancestor proof, got %q err %v", proof, err)
		}
	})

	t.Run("non-ancestor evidence is a hard refusal", func(t *testing.T) {
		if _, err := MergeEvidence(dir, "FAC-99", strandedSHA); err == nil {
			t.Fatal("stranded commit must be refused as evidence")
		}
	})

	t.Run("unmerged ref has no evidence", func(t *testing.T) {
		proof, err := MergeEvidence(dir, "FAC-99", "")
		if err != nil || proof != "" {
			t.Fatalf("FAC-99 is not on origin/main, got %q err %v", proof, err)
		}
	})
}

func TestBoardDone(t *testing.T) {
	dir, _, _ := fixtureRepo(t)
	ctx := context.Background()

	newBoard := func(ref string) *provider.MemoryProvider {
		mp := provider.NewMemoryProvider()
		mp.AddTask(&provider.Task{ID: "id-" + ref, Ref: ref, Title: "t", Status: "in-review", ProjectID: "p1"})
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
		})
		// Use a provider wrapper that hides the Ref from ListTasks but exposes
		// it through GetTask — this is the directly-created-card shape.
		hidden := &refHiddenProvider{MemoryProvider: mp, hiddenRefs: map[string]bool{"kaneo-uuid-211": true}}
		_, err := BoardDone(ctx, hidden, DoneRequest{
			RepoDir: dir, ProjectID: "p1", Ref: "FAC-211",
			Override: &OverrideRequest{Actor: "kampe", Reason: "directly created", Evidence: "sha-211", Policy: "abandoned-scope"},
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
