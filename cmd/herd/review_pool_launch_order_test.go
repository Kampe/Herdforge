package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// FAC-708: cold native Codex/OpenCode sessions are assigned only after the
// first accepted model turn. The helper must wait for that authoritative value
// without accepting a pane/terminal/timestamp fallback.
func TestAwaitNativeReviewerSessionCapturesColdSession(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	state := filepath.Join(dir, "calls")
	script := `#!/bin/sh
n=0
if [ -f "$FAC708_SESSION_CALLS" ]; then n=$(cat "$FAC708_SESSION_CALLS"); fi
n=$((n + 1)); printf '%s\n' "$n" > "$FAC708_SESSION_CALLS"
if [ "$n" -lt 2 ]; then
  printf '%s\n' '{"result":{"agents":[{"name":"reviewer","agent":"codex","agent_status":"working","workspace_id":"w1","tab_id":"t1","pane_id":"p1","terminal_id":"term1","agent_session":{}}]}}'
else
  printf '%s\n' '{"result":{"agents":[{"name":"reviewer","agent":"codex","agent_status":"working","workspace_id":"w1","tab_id":"t1","pane_id":"p1","terminal_id":"term1","agent_session":{"value":"ses_cold_authoritative"}}]}}'
fi
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(herdr.BinaryEnv, bin)
	t.Setenv(herdr.NoLiveEnv, "1")
	t.Setenv("FAC708_SESSION_CALLS", state)
	tab := herdr.TabInfo{ID: "t1", Pane: herdr.PaneInfo{ID: "p1", TerminalID: "term1"}}
	agent, err := awaitNativeReviewerSession("reviewer", "w1", tab, time.Second)
	if err != nil {
		t.Fatalf("cold session capture: %v", err)
	}
	if agent.Session.Value != "ses_cold_authoritative" {
		t.Fatalf("captured session = %q, want authoritative post-prompt session", agent.Session.Value)
	}
	if got, err := os.ReadFile(state); err != nil || strings.TrimSpace(string(got)) != "2" {
		t.Fatalf("expected retry after nullable cold session, calls=%q err=%v", got, err)
	}
}

func TestReviewPoolLaunchOrdersManifestAfterColdSession(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	positions := []string{
		"appendReviewRetirementPending(root, pending)",
		"verifyReviewLaunchFence(ws, surfaceAbs, *tab, lease, sha)",
		"herdr.StartReviewAgent(tab.ID",
		"herdr.Send(agentName,",
		"awaitNativeReviewerSession(agentName, ws, *tab",
		"recordReviewRetirementManifest(root, cfg",
	}
	last := -1
	for _, marker := range positions {
		at := strings.Index(body, marker)
		if at < 0 {
			t.Fatalf("production launch path missing %q", marker)
		}
		if at <= last {
			t.Fatalf("production launch ordering regressed at %q", marker)
		}
		last = at
	}
	if strings.Contains(body, "launched reviewer lacks authenticated session identity") {
		t.Fatal("cold startup must not reject a nullable pre-prompt model session")
	}
}

func TestVerifyReviewLaunchFenceRejectsMissingAuthenticatedLease(t *testing.T) {
	err := verifyReviewLaunchFence("w1", t.TempDir(), herdr.TabInfo{ID: "t1", Pane: herdr.PaneInfo{ID: "p1", TerminalID: "term1"}}, &worktree.PoolSlot{}, strings.Repeat("a", 40))
	if err == nil || !strings.Contains(err.Error(), "authenticated candidate or lease") {
		t.Fatalf("missing lease must fail closed, got %v", err)
	}
}
