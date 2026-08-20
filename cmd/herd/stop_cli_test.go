package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/winddown"
)

// enterWinddown is what makes a restarted stop resume instead of conflict or
// regress the fence.
func TestEnterWinddownIsIdempotentResume(t *testing.T) {
	state := filepath.Join(t.TempDir(), ".herd", "winddown.json")
	t.Setenv("HERD_WINDDOWN_STATE", state)
	ctx := context.Background()

	first, err := enterWinddown(ctx, "tester", "herd-stop")
	if err != nil || !first.Enabled || first.Generation != 1 {
		t.Fatalf("first entry = %+v err=%v", first, err)
	}
	// A restart mid-stop must find the same posture, not a conflict.
	resumed, err := enterWinddown(ctx, "other-actor", "herd-stop")
	if err != nil || !resumed.Enabled || resumed.Generation != first.Generation {
		t.Fatalf("resume = %+v err=%v", resumed, err)
	}

	// Re-entering after an explicit release fences forward.
	a, err := newWinddownAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(ctx, false, "tester", "resumed", resumed.Generation+1, nil); err != nil {
		t.Fatal(err)
	}
	reentered, err := enterWinddown(ctx, "tester", "herd-stop")
	if err != nil || !reentered.Enabled || reentered.Generation != resumed.Generation+2 {
		t.Fatalf("re-entry = %+v err=%v", reentered, err)
	}

	// Corrupt state hides the generation to fence: fail closed, never guess.
	if err := os.WriteFile(state, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := enterWinddown(ctx, "tester", "herd-stop"); err == nil {
		t.Fatal("corrupt posture must not be silently overwritten")
	}
}

// fakeHerdr installs a herdr CLI on PATH that serves one agent and logs every
// invocation, so the CLI's real effects on disk can be inspected.
func fakeHerdr(t *testing.T, agentJSON string) (binDir, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(binDir, "herdr.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$1 $2" in
  "workspace list") printf '{"result":{"workspaces":[{"workspace_id":"wT","label":"wT","focused":true}]}}' ;;
  "agent list") printf '%s' '` + agentJSON + `' ;;
  "agent prompt") printf 'ok' ;;
  "tab close") printf 'ok' ;;
  *) printf 'unsupported %s\n' "$*" >&2; exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binDir, logPath
}

// stopRepo builds a repository whose only copy of some work is uncommitted in
// a linked worktree, plus a branch carrying an unmerged commit.
func stopRepo(t *testing.T) (repo, dirtyFile string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	repo = t.TempDir()
	runGitT(t, repo, "init", "-q", "-b", "main")
	runGitT(t, repo, "config", "user.email", "stop@test")
	runGitT(t, repo, "config", "user.name", "stop")
	runGitT(t, repo, "commit", "--allow-empty", "-q", "-m", "base")
	wt := filepath.Join(repo, "wt")
	runGitT(t, repo, "worktree", "add", "-q", "-b", "task/FAC-93", wt)
	dirtyFile = filepath.Join(wt, "unique.txt")
	if err := os.WriteFile(dirtyFile, []byte("only copy of this work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGitT(t, wt, "add", "unique.txt")
	runGitT(t, wt, "commit", "-q", "-m", "unmerged work")
	if err := os.WriteFile(dirtyFile, []byte("only copy of this work\nplus uncommitted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return repo, dirtyFile
}

// The whole point of the command: a forced stop makes "no new work" durable
// and still destroys nothing.
func TestStopCLIMakesWinddownDurableAndDestroysNoWork(t *testing.T) {
	requireHerdrForLiveTest(t)
	repo, dirtyFile := stopRepo(t)
	binary := buildHerd(t)
	binDir, herdrLog := fakeHerdr(t, `{"result":{"type":"agent_list","agents":[{"name":"smith","agent_status":"working","pane_id":"p1","tab_id":"t1","workspace_id":"wT"}]}}`)
	state := filepath.Join(repo, ".herd", "winddown.json")
	env := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HERD_WINDDOWN_STATE="+state,
		"HERD_ACTOR=stop-cli-test",
	)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Dry run: observable, and no posture on disk.
	out, err := run("stop", "--workspace", "wT")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WOULD_PRESERVE name=smith") || !strings.Contains(out, "DRY_RUN") {
		t.Fatalf("dry run output: %s", out)
	}
	if _, statErr := os.Stat(state); !os.IsNotExist(statErr) {
		t.Fatalf("dry run wrote durable posture: %v", statErr)
	}
	if data, readErr := os.ReadFile(herdrLog); readErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line != "" && !strings.HasPrefix(line, "agent list") {
				t.Fatalf("dry run drove the fleet beyond listing: %q\nlog:\n%s", line, data)
			}
		}
	}

	// Force is the explicit kill: it still may not reach any work.
	// FAC-180: unfenced TabClose fails closed, so execute exits non-zero with
	// blocked_close>0 while still making wind-down durable. That honesty is
	// the contract — never claim a clean close when the fence refuses.
	out, err = run("stop", "--execute", "--force-working", "--wait", "0", "--workspace", "wT")
	if err == nil {
		t.Fatalf("stop --execute must exit non-zero when FAC-180 blocks close:\n%s", out)
	}
	if !strings.Contains(out, "WINDDOWN generation=1") || !strings.Contains(out, "EXECUTED") {
		t.Fatalf("stop did not report a durable posture: %s", out)
	}
	if !strings.Contains(out, "BLOCKED_CLOSE") || !strings.Contains(out, "blocked_close=") {
		t.Fatalf("stop must report blocked close honestly: %s", out)
	}
	if !strings.Contains(out, "closed=0") {
		t.Fatalf("blocked execute must not claim released capacity: %s", out)
	}

	var durable winddown.State
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("posture not durable: %v", err)
	}
	if err := json.Unmarshal(raw, &durable); err != nil || !durable.Enabled || durable.Actor != "stop-cli-test" {
		t.Fatalf("durable posture = %s (err %v)", raw, err)
	}

	// AC: no new work starts. Every claiming command routes through the same
	// admission gate; pulse is the canonical one.
	out, err = run("pulse", "--role", "worker")
	if err == nil || !strings.Contains(out, "fleet admission rejected") {
		t.Fatalf("wind-down did not stop new work: %v\n%s", err, out)
	}

	// AC: shared/root and dirty worktrees are never deleted.
	content, err := os.ReadFile(dirtyFile)
	if err != nil || !strings.Contains(string(content), "plus uncommitted") {
		t.Fatalf("uncommitted work lost: %q err=%v", content, err)
	}
	worktrees := runGitT(t, repo, "worktree", "list")
	if !strings.Contains(worktrees, filepath.Join(repo, "wt")) || !strings.Contains(worktrees, repo) {
		t.Fatalf("a worktree was removed: %s", worktrees)
	}
	if branches := runGitT(t, repo, "branch", "--list", "task/FAC-93"); !strings.Contains(branches, "task/FAC-93") {
		t.Fatalf("branch carrying unmerged work was deleted: %q", branches)
	}

	// The only fleet mutations attempted are a prompt and a tab close.
	log, err := os.ReadFile(herdrLog)
	if err != nil {
		t.Fatalf("herdr log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		switch {
		case line == "", strings.HasPrefix(line, "agent list"),
			strings.HasPrefix(line, "workspace list"),
			strings.HasPrefix(line, "agent prompt"),
			strings.HasPrefix(line, "agent send-keys"),
			strings.HasPrefix(line, "tab close"):
		default:
			t.Fatalf("stop invoked an unexpected herdr command: %q\nlog:\n%s", line, log)
		}
	}
	if !strings.Contains(string(log), "agent prompt p1") {
		t.Fatalf("no stop request was delivered: %s", log)
	}
}
