package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

// FAC-625: two live defects in `herd launch-record`, both driven from the
// SHIPPED command with a lane-worktree cwd, asserting on the receipt ROW
// written -- never on stdout, which is exactly what lied before this fix (the
// success line printed "family=anthropic" while the written receipt omitted
// BuilderFamily entirely).
func launchRecordRepo(t *testing.T) (root, lane string) {
	t.Helper()
	root = t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := testgit.Command(root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "fac625@test.invalid")
	git("config", "user.name", "fac625")
	git("commit", "-q", "--allow-empty", "-m", "base")
	lane = filepath.Join(root, ".herd", "worktrees", "fac-625-lane")
	if out, err := testgit.Command(root, "worktree", "add", "-q", "-b", "fac-625-lane", lane).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	return root, lane
}

// The receipt must carry the builder family launch-record ITSELF derived and
// printed -- the defect left every field but this one written.
func TestLaunchRecordPersistsDerivedBuilderFamily(t *testing.T) {
	binary := buildHerd(t)
	root, lane := launchRecordRepo(t)

	cmd := exec.Command(binary, "launch-record",
		"--lane", "fac-625-lane", "--cwd", lane, "--provider", "claude", "--model", "claude-sonnet-5")
	cmd.Dir = lane // the shipped call shape: launch-record runs FROM the lane worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launch-record: %v: %s", err, out)
	}

	receipts := readReceipts(t, root)
	if len(receipts) != 1 {
		t.Fatalf("got %d receipts at the project root, want 1: %+v", len(receipts), receipts)
	}
	if receipts[0].BuilderFamily != "anthropic" {
		t.Fatalf("receipt builder_family = %q, want anthropic; the command printed the family "+
			"but the ROW it wrote must carry it too, or BuilderFamilyReachingSHA skips it", receipts[0].BuilderFamily)
	}
}

// `--lane CHA-#### --cwd ... --provider ...` with no `--task-ref` is the
// standing dispatch shape. The receipt must still be joinable to the card by
// defaulting task_ref to the lane; an explicit --task-ref must still win.
func TestLaunchRecordDefaultsTaskRefFromLaneUnlessExplicit(t *testing.T) {
	binary := buildHerd(t)
	root, lane := launchRecordRepo(t)

	cmd := exec.Command(binary, "launch-record",
		"--lane", "CHA-3211", "--cwd", lane, "--provider", "claude", "--model", "claude-sonnet-5")
	cmd.Dir = lane
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launch-record: %v: %s", err, out)
	}
	receipts := readReceipts(t, root)
	if len(receipts) != 1 || receipts[0].TaskRef != "CHA-3211" {
		t.Fatalf("receipt task_ref = %+v, want a single receipt with task_ref CHA-3211 defaulted from --lane", receipts)
	}

	cmd = exec.Command(binary, "launch-record",
		"--lane", "CHA-3211", "--cwd", lane, "--provider", "claude", "--model", "claude-sonnet-5",
		"--task-ref", "CHA-9999")
	cmd.Dir = lane
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launch-record: %v: %s", err, out)
	}
	receipts = readReceipts(t, root)
	if len(receipts) != 2 || receipts[1].TaskRef != "CHA-9999" {
		t.Fatalf("receipt task_ref = %+v, want the explicit --task-ref to win over --lane", receipts)
	}
}

// The receipt must land at the project root, not wherever a cwd-relative sink
// would resolve from the lane worktree -- or review-ingest, which reads the
// project root, will never see it.
func TestLaunchRecordWritesToProjectRootNotLaneWorktree(t *testing.T) {
	binary := buildHerd(t)
	root, lane := launchRecordRepo(t)

	cmd := exec.Command(binary, "launch-record",
		"--lane", "fac-625-lane", "--cwd", lane, "--provider", "claude", "--model", "claude-sonnet-5")
	cmd.Dir = lane
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launch-record: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(lane, ".herd", "launch-receipts.jsonl")); err == nil {
		t.Fatal("launch-record wrote into the lane worktree; review-ingest reads the project root and would never see this receipt")
	}
	if _, err := os.Stat(filepath.Join(root, ".herd", "launch-receipts.jsonl")); err != nil {
		t.Fatalf("expected the receipt at the project root: %v", err)
	}
}

func TestLaunchRecordRequiresLaneCwdAndProvider(t *testing.T) {
	binary := buildHerd(t)
	_, lane := launchRecordRepo(t)
	cmd := exec.Command(binary, "launch-record", "--cwd", lane, "--provider", "claude")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--lane") {
		t.Fatalf("missing --lane must be refused, got err=%v out=%s", err, out)
	}
}
