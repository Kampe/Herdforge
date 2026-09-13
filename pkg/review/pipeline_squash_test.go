package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/harvest"
)

// FAC-805 (drain side): harvest.ContentMerged is git cherry-pick equivalence
// over a single tip, which cannot survive a squash merge -- the reviewed
// range's per-commit patches are replaced by one combined patch on main, so
// every original commit reports unique forever even though the content
// landed. These three cases pin the fix's contract: a genuinely
// squash-landed range must be excluded from the review/harvest queues, and
// every case that only LOOKS like one (extra unreviewed content, a reverted
// landing) must stay exactly as protected as before.
func TestPipelineContract_SquashRangeContentMerged(t *testing.T) {
	root, lane := setupPipelineRepo(t)
	for i, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(lane, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitDrain(t, lane, "add", name)
		gitDrain(t, lane, "commit", "-q", "-m", fmt.Sprintf("lane commit %d", i))
	}
	tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))

	gitDrain(t, root, "merge", "--squash", "-q", "lane")
	gitDrain(t, root, "commit", "-q", "-m", "squash lane")
	mainTip := strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD"))
	gitDrain(t, root, "update-ref", "refs/remotes/origin/main", mainTip)

	if err := testgit.Command(root, "merge-base", "--is-ancestor", tip, "origin/main").Run(); err == nil {
		t.Fatal("lane tip unexpectedly an ancestor of origin/main; squash test is vacuous")
	}

	r, err := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), Cap: 8}).
		Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Shas.ContentMerged) != 1 || r.Shas.ContentMerged[0] != tip {
		t.Fatalf("squash-landed range not recognized as content-merged: %+v", r.Shas)
	}
	if r.NeedReview != 0 || r.Harvestable != 0 || r.HarvestReady != 0 {
		t.Fatalf("squash-landed range leaked into the review/harvest queue: %+v", r)
	}
}

func TestPipelineContract_SquashRangeExtraUniqueCommitStaysUnmerged(t *testing.T) {
	root, lane := setupPipelineRepo(t)
	for i, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(lane, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitDrain(t, lane, "add", name)
		gitDrain(t, lane, "commit", "-q", "-m", fmt.Sprintf("reviewed %d", i))
	}
	gitDrain(t, root, "merge", "--squash", "-q", "lane")
	gitDrain(t, root, "commit", "-q", "-m", "squash lane")
	mainTip := strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD"))
	gitDrain(t, root, "update-ref", "refs/remotes/origin/main", mainTip)

	// Genuinely new, unreviewed content added to the lane AFTER the reviewed
	// range was squashed onto main. It must not ride along on the fix's
	// whole-range proof.
	if err := os.WriteFile(filepath.Join(lane, "extra"), []byte("extra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitDrain(t, lane, "add", "extra")
	gitDrain(t, lane, "commit", "-q", "-m", "extra unreviewed work")
	tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))

	r, err := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), Cap: 8}).
		Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Shas.ContentMerged) != 0 {
		t.Fatalf("extra unreviewed commit falsely marked content-merged: %+v", r.Shas)
	}
	if r.NeedReview != 1 {
		t.Fatalf("extra unreviewed commit did not stay queued for review: %+v", r)
	}
}

func TestPipelineContract_RevertedSquashStaysUnmerged(t *testing.T) {
	root, lane := setupPipelineRepo(t)
	for i, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(lane, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitDrain(t, lane, "add", name)
		gitDrain(t, lane, "commit", "-q", "-m", fmt.Sprintf("reviewed %d", i))
	}
	tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))

	gitDrain(t, root, "merge", "--squash", "-q", "lane")
	gitDrain(t, root, "commit", "-q", "-m", "squash lane")
	squashSHA := strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD"))
	gitDrain(t, root, "revert", "--no-edit", squashSHA)
	mainTip := strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD"))
	gitDrain(t, root, "update-ref", "refs/remotes/origin/main", mainTip)

	r, err := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), Cap: 8}).
		Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Shas.ContentMerged) != 0 {
		t.Fatalf("reverted squash landing falsely marked content-merged: %+v", r.Shas)
	}
	if r.NeedReview != 1 {
		t.Fatalf("reverted squash landing did not stay queued for review: %+v", r)
	}
}
