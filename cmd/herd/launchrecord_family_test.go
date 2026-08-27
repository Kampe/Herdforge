package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-625: `herd launch-record` computed the vendor family, guarded on it,
// PRINTED it -- and never wrote it. Every manually recorded receipt went out
// with an empty builder_family, which BuilderFamilyReachingSHA skips, so the
// command whose entire purpose is recording provenance recorded everything
// except provenance. Its success line printed `family=anthropic` while the row
// on disk proved nothing, which is why it went unnoticed for so long.
//
// Live rows with provider/model/lane and no family: CHA-3211 21:24:34,
// CHA-3455 21:16:50, CHA-3466 21:20:25.
//
// TaskRef had the paired defect: it came only from optional --task-ref, while
// the standing workflow directs `--lane CHA-####` without it, so task_ref was
// empty and the receipt could not be joined to a card.
//
// These drive the SHIPPED command and assert on the row it WRITES, never on its
// stdout -- the stdout is precisely what lied.

func recordAndRead(t *testing.T, binary, root, dir string, args ...string) map[string]any {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"launch-record"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HERD_ROOT="+dir, "HERD_REPO_ROOT="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch-record failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".herd", "launch-receipts.jsonl"))
	if err != nil {
		t.Fatalf("no receipt written at the project root: %v\ncommand output:\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func launchRecordRepo(t *testing.T) (root, lane string) {
	t.Helper()
	root = t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(root, "init", "-q", "-b", "main")
	git(root, "commit", "-q", "--allow-empty", "-m", "base")
	lane = filepath.Join(root, "lane-wt")
	git(root, "worktree", "add", "-q", "-b", "fix/cha-3211-r3", lane, "HEAD")
	return root, lane
}

// THE defect. A recorded receipt must carry the derived vendor family.
func TestLaunchRecordWritesTheDerivedBuilderFamily(t *testing.T) {
	binary := buildHerd(t)
	root, lane := launchRecordRepo(t)

	row := recordAndRead(t, binary, root, root,
		"--lane", "CHA-3211", "--role", "worker", "--task-shape", "implementation",
		"--provider", "claude", "--model", "claude-sonnet-5", "--cwd", lane)

	if got, _ := row["builder_family"].(string); got != "anthropic" {
		t.Fatalf("builder_family = %q, want anthropic.\n"+
			"The family is computed, guarded and printed but not written, so the receipt "+
			"cannot establish provenance and BuilderFamilyReachingSHA skips it -- the "+
			"command that exists to record provenance records everything except provenance.\nrow: %v", got, row)
	}
}

// The paired defect: `--lane CHA-####` without `--task-ref` must still produce a
// joinable task identity.
func TestLaunchRecordDefaultsTaskRefToTheLane(t *testing.T) {
	binary := buildHerd(t)
	root, lane := launchRecordRepo(t)

	row := recordAndRead(t, binary, root, root,
		"--lane", "CHA-3211", "--role", "worker", "--task-shape", "implementation",
		"--provider", "claude", "--model", "claude-sonnet-5", "--cwd", lane)

	if got, _ := row["task_ref"].(string); got != "CHA-3211" {
		t.Fatalf("task_ref = %q, want CHA-3211 defaulted from --lane. The standing workflow "+
			"directs --lane CHA-#### with no --task-ref, so the receipt could not be joined to a card.", got)
	}
}

// An explicit --task-ref must still win over the lane default.
func TestAnExplicitTaskRefWins(t *testing.T) {
	binary := buildHerd(t)
	root, lane := launchRecordRepo(t)

	row := recordAndRead(t, binary, root, root,
		"--lane", "defi-crusader", "--task-ref", "CHA-3466", "--role", "worker",
		"--task-shape", "implementation", "--provider", "claude", "--model", "claude-sonnet-5", "--cwd", lane)

	if got, _ := row["task_ref"].(string); got != "CHA-3466" {
		t.Fatalf("task_ref = %q, want the explicit CHA-3466", got)
	}
}

// FAC-625, third defect and the same cwd-relative class as FAC-643/646:
// recording from a LANE WORKTREE must still write the PROJECT's receipt log,
// because that is the only log ingest reads.
func TestLaunchRecordWritesToTheProjectRootFromALaneWorktree(t *testing.T) {
	binary := buildHerd(t)
	root, lane := launchRecordRepo(t)

	// cwd is the lane worktree, not the project root.
	row := recordAndRead(t, binary, root, lane,
		"--lane", "CHA-3211", "--role", "worker", "--task-shape", "implementation",
		"--provider", "claude", "--model", "claude-sonnet-5", "--cwd", lane)

	if got, _ := row["builder_family"].(string); got != "anthropic" {
		t.Fatalf("row at the project root is wrong or absent: %v", row)
	}
	if _, err := os.Stat(filepath.Join(lane, ".herd", "launch-receipts.jsonl")); err == nil {
		t.Fatal("a receipt was written into the LANE worktree's log, where ingest never looks; " +
			"that is how the CHA-3211 receipt became invisible to review-ingest")
	}
}
