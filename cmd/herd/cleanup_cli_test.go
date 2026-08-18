package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// cleanupFakeHerdr writes a minimal herdr fake that only answers `agent list`
// with the supplied JSON payload. compare-close is intentionally NOT handled
// — the default cleanup seam fails closed at ExpandCloseRequest before the
// transport is reached, so the fake never sees that subcommand.
func cleanupFakeHerdr(t *testing.T, agentListJSON string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	script := `#!/bin/sh
case "$1 $2" in
  "agent list") printf '%s\n' '` + agentListJSON + `' ;;
  "workspace list") printf '{"result":{"workspaces":[{"workspace_id":"w","label":"other"}]}}\n' ;;
  *) printf '{"result":{}}\n' ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// cleanupEnv returns the env for exec'ing the herd binary with the fake herdr.
func cleanupEnv(fakeBin string) []string {
	return []string{
		herdr.BinaryEnv + "=" + fakeBin,
		herdr.NoLiveEnv + "=1",
		"HERD_WORKSPACE=w",
		"PATH=/usr/bin:/bin",
	}
}

func TestCleanupCLI_DryRunJSON(t *testing.T) {
	binary := buildHerd(t)
	fake := cleanupFakeHerdr(t,
		`{"result":{"agents":[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":3}],"type":"agents"}}`)
	cmd := exec.Command(binary, "cleanup", "--dry-run", "--json")
	cmd.Env = cleanupEnv(fake)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run --json exit: %v\n%s", err, out)
	}
	var pkt map[string]interface{}
	if err := json.Unmarshal(out, &pkt); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if pkt["dry_run"] != true {
		t.Fatalf("dry_run must be true: %s", out)
	}
	cands, ok := pkt["candidates"].([]interface{})
	if !ok || len(cands) != 1 {
		t.Fatalf("candidates=%v want 1: %s", pkt["candidates"], out)
	}
	if pkt["closed"].(float64) != 0 || pkt["blocked"].(float64) != 0 {
		t.Fatalf("dry-run counts must be zero: %s", out)
	}
	attempts, _ := pkt["attempts"].([]interface{})
	if len(attempts) != 0 {
		t.Fatalf("dry-run must not produce attempts: %s", out)
	}
}

func TestCleanupCLI_DryRunText(t *testing.T) {
	binary := buildHerd(t)
	fake := cleanupFakeHerdr(t,
		`{"result":{"agents":[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":3}],"type":"agents"}}`)
	cmd := exec.Command(binary, "cleanup", "--dry-run")
	cmd.Env = cleanupEnv(fake)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run text exit: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "would close") {
		t.Fatalf("expected 'would close' in output: %s", s)
	}
	if strings.Contains(s, "BLOCKED") {
		t.Fatalf("dry-run must not show BLOCKED: %s", s)
	}
}

func TestCleanupCLI_MutationFailsClosedJSON(t *testing.T) {
	binary := buildHerd(t)
	fake := cleanupFakeHerdr(t,
		`{"result":{"agents":[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":3}],"type":"agents"}}`)
	cmd := exec.Command(binary, "cleanup", "--json")
	cmd.Env = cleanupEnv(fake)
	out, _ := cmd.CombinedOutput()
	var pkt map[string]interface{}
	if err := json.Unmarshal(out, &pkt); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if pkt["closed"].(float64) != 0 {
		t.Fatalf("mutation must close zero tabs: %s", out)
	}
	if pkt["blocked"].(float64) != 1 {
		t.Fatalf("mutation must block 1 candidate: %s", out)
	}
	attempts, _ := pkt["attempts"].([]interface{})
	if len(attempts) != 1 {
		t.Fatalf("attempts=%d want 1: %s", len(attempts), out)
	}
	att := attempts[0].(map[string]interface{})
	if att["outcome"] != "blocked" {
		t.Fatalf("outcome=%v want blocked: %s", att["outcome"], out)
	}
}

func TestCleanupCLI_MutationFailsClosedText(t *testing.T) {
	binary := buildHerd(t)
	fake := cleanupFakeHerdr(t,
		`{"result":{"agents":[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":3}],"type":"agents"}}`)
	cmd := exec.Command(binary, "cleanup")
	cmd.Env = cleanupEnv(fake)
	out, _ := cmd.CombinedOutput()
	s := string(out)
	if !strings.Contains(s, "BLOCKED") {
		t.Fatalf("mutation must show BLOCKED: %s", s)
	}
	if !strings.Contains(s, "closed=0") {
		t.Fatalf("mutation must show closed=0: %s", s)
	}
	if strings.Contains(s, "closed=1") {
		t.Fatalf("mutation must never close without generation fence: %s", s)
	}
}

func TestCleanupCLI_NoCandidatesText(t *testing.T) {
	binary := buildHerd(t)
	fake := cleanupFakeHerdr(t,
		`{"result":{"agents":[],"type":"agents"}}`)
	cmd := exec.Command(binary, "cleanup")
	cmd.Env = cleanupEnv(fake)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("no candidates must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "nothing to close") {
		t.Fatalf("expected 'nothing to close': %s", out)
	}
}

func TestCleanupCLI_HerdrNotFoundExitsNonZero(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "cleanup")
	cmd.Env = []string{
		herdr.BinaryEnv + "=/nonexistent/herdr",
		herdr.NoLiveEnv + "=1",
		"PATH=/usr/bin:/bin",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("missing herdr must exit non-zero: %s", out)
	}
	if !strings.Contains(string(out), "herdr CLI not found") {
		t.Fatalf("expected 'herdr CLI not found': %s", out)
	}
}
