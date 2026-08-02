package sync

import (
	"context"
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

	t.Run("moves to done with commit evidence and read-back", func(t *testing.T) {
		mp := newBoard("FAC-18")
		res, err := BoardDone(ctx, mp, dir, "p1", "FAC-018", "", false)
		if err != nil {
			t.Fatalf("BoardDone: %v", err)
		}
		if res.Ref != "FAC-18" || !strings.Contains(res.Proof, "FAC-18") {
			t.Fatalf("unexpected result %+v", res)
		}
		got, _ := mp.GetTask(ctx, "id-FAC-18")
		if got.Status != "done" {
			t.Fatalf("status = %q, want done", got.Status)
		}
	})

	t.Run("refuses without evidence", func(t *testing.T) {
		mp := newBoard("FAC-99")
		_, err := BoardDone(ctx, mp, dir, "p1", "FAC-99", "", false)
		if err == nil || !strings.Contains(err.Error(), "no merge evidence") {
			t.Fatalf("want ErrNoEvidence refusal, got %v", err)
		}
		got, _ := mp.GetTask(ctx, "id-FAC-99")
		if got.Status != "in-review" {
			t.Fatalf("refused card must not move, status = %q", got.Status)
		}
	})

	t.Run("force overrides with honest proof text", func(t *testing.T) {
		mp := newBoard("FAC-99")
		res, err := BoardDone(ctx, mp, dir, "p1", "FAC-99", "", true)
		if err != nil {
			t.Fatalf("BoardDone --force: %v", err)
		}
		if !strings.Contains(res.Proof, "--force") || !res.Forced {
			t.Fatalf("forced result must say so: %+v", res)
		}
	})

	t.Run("unknown ref errors", func(t *testing.T) {
		mp := newBoard("FAC-18")
		if _, err := BoardDone(ctx, mp, dir, "p1", "FAC-500", "", true); err == nil {
			t.Fatal("unknown ref must error")
		}
	})
}
