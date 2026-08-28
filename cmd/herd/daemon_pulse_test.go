package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This is deliberately a compiled-CLI test of `herd daemon --once`, not a
// test of a helper. The defect was a fully working pulse path with no external
// caller: only the shipped daemon command proves that an OS process runs the
// resume verb when the coordinator itself is paused.
func TestDaemonOnceResumesPausedCoordinatorOutsideTheCoordinator(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main")
	runGitT(t, root, "config", "user.email", "fac-633@test")
	runGitT(t, root, "config", "user.name", "FAC-633")
	runGitT(t, root, "commit", "--allow-empty", "-qm", "base")

	if err := os.MkdirAll(filepath.Join(root, ".herd", "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "prompts", "orchestrator.md"), []byte("orchestrator"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := `version: "1"
project:
  name: "fac-633"
  default_branch: "main"
task_provider:
  type: "memory"
  enabled: ["memory"]
  project_id: "fac-633"
fleet:
  herdr_workspace: "wFAC633"
verification:
  test_command: "echo ok"
lanes:
  - name: "orchestrator"
    role: "orchestrator"
    agent_kind: "codex"
    harness: "codex"
    prompt: ".herd/prompts/orchestrator.md"
    worktree: ".worktrees/orchestrator"
    provider: "codex"
    model: "gpt-5.6-sol"
    effort: "medium"
    task_shape: "coordinator"
    standing: true
    authority: "read"
    capabilities: ["board-write"]
`
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	winddown := filepath.Join(root, ".herd", "winddown.json")
	if err := os.WriteFile(winddown, []byte(`{"enabled":false,"actor":"test","reason":"fac-633","timestamp":"2026-08-28T18:00:00Z","generation":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeDir := t.TempDir()
	log := filepath.Join(fakeDir, "herdr.log")
	fake := filepath.Join(fakeDir, "herdr")
	const fakeHerdr = `#!/bin/sh
printf '%s\n' "$*" >> "$HERD_FAKE_LOG"
case "$1 $2" in
  "agent list")
    printf '%s\n' '{"result":{"agents":[{"name":"forge-orchestrator","agent_status":"working","pane_id":"paused-pane","tab_id":"paused-tab","workspace_id":"wFAC633","cwd":"'"$PWD"'","foreground_cwd":"'"$PWD"'","state_change_seq":7,"revision":1}],"type":"agents"}}'
    ;;
  "pane read") printf '%s\n' '{"result":{"text":"Goal paused (/goal resume)"}}' ;;
  "pane process-info") printf '%s\n' '{"result":{"process_info":{"foreground_processes":[{"pid":1,"name":"codex"}]}}}' ;;
  "agent explain") printf '%s\n' '{"result":{"state":"working","visible_working":true}}' ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(fakeHerdr), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "daemon", "--once")
	cmd.Dir = root
	cmd.Env = append(reviewTestEnv(),
		"HERD_MODE=local",
		"HERD_WINDDOWN_STATE="+winddown,
		"HERD_HERDR_BIN="+fake,
		"HERD_NO_LIVE_HERDR=1",
		"HERD_FAKE_LOG="+log,
		"HERD_WORKSPACE=wFAC633",
		"HERDR_WORKSPACE_ID=wFAC633",
		"HERD_RESUME_STATE="+filepath.Join(root, ".herd", "run", "resume-state.json"),
	)
	prependToPath(cmd, fakeDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd daemon --once failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CONTROL-PLANE STALL: paused coordinator(s): forge-orchestrator") {
		t.Fatalf("daemon did not surface the paused coordinator alarm:\n%s", out)
	}
	logBody, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBody), "pane send-text paused-pane /goal resume") || !strings.Contains(string(logBody), "pane send-keys paused-pane enter") {
		t.Fatalf("daemon never drove the paused-goal resume verb through the shipped path:\n%s", logBody)
	}
}
