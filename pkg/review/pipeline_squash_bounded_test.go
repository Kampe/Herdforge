package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// FAC-767 (drain side): the squash-range recheck enters
// mergeadmit.ProveEquivalentLanded and mergeadmit.ReplayTree, whose git probes
// (merge-base, range walks, patch ids, merge-tree, tree reads) previously ran
// without any context, so once the recheck was entered the per-item deadline
// could not stop it and one slow repository starved the whole drain. The
// helper now pins origin/main to one observed commit (a concurrent ref move
// must not combine a proof against one snapshot with containment against
// another) and propagates ctx into every subprocess. These tests pin that
// contract with a controlled slow-git shim.

// realGitOrSkip resolves the host git ONCE, before any PATH manipulation, so
// the shim can delegate to it. A missing binary is an environment difference,
// not a test failure (FAC-215).
func realGitOrSkip(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available for controlled slow-git shim: %v", err)
	}
	return real
}

// slowRangeProofShim installs a git shim that passes every command straight
// through to the real git except `rev-list --reverse` -- the range walk that
// is unique to the whole-range proof (freshness uses rev-list --count and
// ContentMerged uses log). The hook records the shim process pid and a
// started marker, then blocks for delay so the test can prove the caller's
// deadline kills the probe (a leaked child would survive to write the
// finished marker and stay alive). It returns the marker directory.
func slowRangeProofShim(t *testing.T, real string, delay string) string {
	t.Helper()
	bin := t.TempDir()
	markers := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "rev-list" ] && [ "$2" = "--reverse" ]; then
  echo $$ > %q/child.pid
  : > %q/started
  sleep %s
  : > %q/finished
fi
exec %q "$@"
`, markers, markers, delay, markers, real)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return markers
}

// moveMainRefShim installs a git shim that, at the range proof's first
// rev-list --reverse (after the helper pinned mainRef), repoints origin/main
// to moveSHA and then delegates. With one pinned snapshot the verdict is
// unaffected; re-resolving the moving ref at each step combines a proof
// against the old tip with containment against the new one.
func moveMainRefShim(t *testing.T, real, root, moveSHA string) {
	t.Helper()
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "rev-list" ] && [ "$2" = "--reverse" ]; then
  %q -C %q update-ref refs/remotes/origin/main %q || exit 1
fi
exec %q "$@"
`, real, root, moveSHA, real)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// assertChildKilled proves the blocked shim process did not leak: the started
// marker exists, the finished marker does not (the sleep never completed), and
// the recorded pid is gone.
func assertChildKilled(t *testing.T, markers string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(markers, "started")); err != nil {
		t.Fatalf("slow range-proof probe was never entered; the deadline guard would be vacuous: %v", err)
	}
	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("shim survived to completion; the deadline did not stop the probe")
	}
	if runtime.GOOS == "windows" {
		return
	}
	pidData, err := os.ReadFile(filepath.Join(markers, "child.pid"))
	if err != nil {
		t.Fatalf("shim pid not recorded: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(pidData)), "%d", &pid); err != nil {
		t.Fatalf("unreadable shim pid %q: %v", pidData, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p, findErr := os.FindProcess(pid)
		if findErr == nil && p.Signal(syscall.Signal(0)) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("leaked child %d still alive after the scan returned", pid)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return
	}
}

// Both tips are genuinely unique (never merged anywhere), so the scan enters
// the whole-range recheck for each. With the shim blocking inside that
// recheck, the per-item deadline must stop the probe, classify the tip as
// slow, and reach the SUBSEQUENT tip -- not starve the drain inside a
// contextless git call.
func TestPipelineContract_SlowRangeProofDeadlineStopsProbeAndReachesNextTip(t *testing.T) {
	real := realGitOrSkip(t)
	root, lane := setupPipelineRepo(t)
	for i, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(lane, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitDrain(t, lane, "add", name)
		gitDrain(t, lane, "commit", "-q", "-m", fmt.Sprintf("reviewed %d", i))
	}
	tip1 := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))
	lane2 := filepath.Join(root, "lane2")
	gitDrain(t, root, "worktree", "add", "-q", "-b", "lane2", lane2)
	if err := os.WriteFile(filepath.Join(lane2, "c"), []byte("c\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitDrain(t, lane2, "add", "c")
	gitDrain(t, lane2, "commit", "-q", "-m", "reviewed 2")
	tip2 := strings.TrimSpace(gitDrain(t, lane2, "rev-parse", "HEAD"))

	budget := 1 * time.Second
	markers := slowRangeProofShim(t, real, "30")

	start := time.Now()
	r, err := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), Cap: 8, ProbeBudget: budget}).
		Scan(context.Background(), []harvest.UnmergedWork{
			{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip1}},
			{WorktreePath: lane2, Branch: "lane2", Unmerged: []string{tip2}},
		})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("per-item slow probes must not fail the scan: %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("scan starved inside the range proof for %v; the per-item deadline did not stop it", elapsed)
	}
	if r.ScannedTips != 2 || r.TotalTips != 2 {
		t.Fatalf("scan did not reach the subsequent tip: ScannedTips=%d TotalTips=%d", r.ScannedTips, r.TotalTips)
	}
	if !r.ScanTruncated {
		t.Fatalf("slow probes must mark the report truncated: %+v", r)
	}
	if len(r.SlowTips) != 2 {
		t.Fatalf("both deadline-stopped tips must carry the bounded diagnostic: %+v", r.SlowTips)
	}
	for _, st := range r.SlowTips {
		if st.Budget != budget.String() {
			t.Fatalf("slow tip %s recorded budget %q, want %q", st.SHA, st.Budget, budget.String())
		}
	}
	if len(r.Shas.ContentMerged) != 0 {
		t.Fatalf("an unproven tip was marked content-merged on a timeout: %+v", r.Shas)
	}
	assertChildKilled(t, markers)
}

// The OUTER scan deadline must stay a hard stop even when the per-item budget
// is generous: a parent deadline landing inside the whole-range proof fails
// closed with the truncated partial report instead of a verdict from
// interrupted evidence, and the probe's child does not leak.
func TestPipelineContract_OuterDeadlineInsideRangeProofFailsClosed(t *testing.T) {
	real := realGitOrSkip(t)
	root, lane := setupPipelineRepo(t)
	for i, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(lane, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitDrain(t, lane, "add", name)
		gitDrain(t, lane, "commit", "-q", "-m", fmt.Sprintf("reviewed %d", i))
	}
	tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))

	markers := slowRangeProofShim(t, real, "30")

	parentCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	r, err := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), Cap: 8, ProbeBudget: 10 * time.Second}).
		Scan(parentCtx, []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("an outer deadline inside the range proof must fail closed, got report %+v", r)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("outer deadline error must surface as DeadlineExceeded, got %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("scan ignored the parent deadline inside the range proof for %v", elapsed)
	}
	if !r.ScanTruncated {
		t.Fatalf("truncated scan must be marked as such: %+v", r)
	}
	if r.ScannedTips >= 1 {
		t.Fatalf("no tip may keep a disposition derived from interrupted evidence: %+v", r)
	}
	assertChildKilled(t, markers)
}

// origin/main must be pinned to ONE observed commit for the whole
// proof+containment recheck. Here the squash-landed content is on origin/main
// (verdict must be merged), a revert commit already exists on the main branch,
// and the shim repoints origin/main to that revert DURING the proof. Pinned
// semantics keep the merged verdict; re-resolving the moving ref would combine
// a proof against the squash tip with containment against the revert and
// wrongly report the landing as stale.
func TestPipelineContract_RangeProofPinsMainSnapshotAgainstRefMovement(t *testing.T) {
	real := realGitOrSkip(t)
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
	gitDrain(t, root, "update-ref", "refs/remotes/origin/main", squashSHA)
	// A revert exists on the main branch but origin/main still names the
	// squash tip; the shim moves it only once the proof is running.
	gitDrain(t, root, "revert", "--no-edit", squashSHA)
	revertSHA := strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD"))
	if revertSHA == squashSHA {
		t.Fatal("revert must be a distinct commit for the pin test to be meaningful")
	}

	moveMainRefShim(t, real, root, revertSHA)

	r, err := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), Cap: 8}).
		Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Shas.ContentMerged) != 1 || r.Shas.ContentMerged[0] != tip {
		t.Fatalf("a ref moved mid-proof changed the verdict from one pinned snapshot: %+v", r.Shas)
	}
	if r.NeedReview != 0 || r.Harvestable != 0 {
		t.Fatalf("squash-landed range leaked into the review/harvest queue after ref movement: %+v", r)
	}
}
