package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/standing"
)

func TestStandingShippedRaiseDoesNotHangAfterAdmit(t *testing.T) {
	prevAdmit := standingAdmitDeadline
	prevGoal := standingGoalGuardDeadline
	standingAdmitDeadline = 5 * time.Second
	standingGoalGuardDeadline = time.Second
	t.Cleanup(func() {
		standingAdmitDeadline = prevAdmit
		standingGoalGuardDeadline = prevGoal
	})

	root, origin, laneDir, _ := standingLaneRepo(t)
	fresh := standingCommit(t, origin, "origin-ahead")
	standingGit(t, root, "init", "-q", "-b", "main")
	standingGit(t, root, "commit", "-q", "--allow-empty", "-m", "identity")
	standingGit(t, root, "remote", "add", "origin", "https://github.com/Kampe/Herdforge.git")
	instead := "url." + origin + "/.insteadOf"
	standingGit(t, root, "config", instead, "https://github.com/Kampe/Herdforge.git")
	standingGit(t, laneDir, "remote", "set-url", "origin", "https://github.com/Kampe/Herdforge.git")
	standingGit(t, laneDir, "config", instead, "https://github.com/Kampe/Herdforge.git")

	installProtocolFakeHerdr(t)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	t.Setenv("HERD_WORKSPACE", "wFAKE")
	t.Setenv("HERDR_WORKSPACE_ID", "wFAKE")
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("HERD_ERA_PROVIDERS", "grok")
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "grok"), []byte("#!/bin/sh\nprintf 'PROBE_OK'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinHealthyQuota(t, binDir, "grok")
	promptRel := filepath.Join(".herd", "prompts", "worker.md")
	if err := os.MkdirAll(filepath.Dir(promptRel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptRel, []byte("prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lane := &config.LaneDef{
		Name: "scout", Role: launch.ScoutPlannerRole, AgentKind: "grok", Harness: "grok",
		Provider: "grok", Model: "grok-4.6", Effort: "medium", TaskShape: "architecture",
		Standing: true, Worktree: "wt/scout", Prompt: promptRel,
		Authority: config.AuthorityWrite, Capabilities: []config.Capability{config.CapabilityGitWrite},
	}
	cfg := &config.Config{Lanes: []config.LaneDef{*lane}}

	start := time.Now()
	raiseErr := runStandingConfigMode(cfg, true, standing.ModeRaise, []string{lane.Name}, true, false)
	elapsed := time.Since(start)
	if elapsed > 12*time.Second {
		t.Fatalf("shipped ModeRaise hung %s", elapsed)
	}
	got := standingGit(t, laneDir, "rev-parse", "HEAD")
	if got != fresh {
		t.Fatalf("PrepareWorktree did not refresh: HEAD %s want %s (raise err=%v after %s)", got, fresh, raiseErr, elapsed)
	}
}

func TestStandingShippedRaiseRefreshesBehindWorktree(t *testing.T) {
	root, origin, laneDir, _ := standingLaneRepo(t)
	fresh := standingCommit(t, origin, "origin-ahead")
	standingGit(t, root, "init", "-q", "-b", "main")
	standingGit(t, root, "commit", "-q", "--allow-empty", "-m", "identity")
	standingGit(t, root, "remote", "add", "origin", "https://github.com/Kampe/Herdforge.git")
	instead := "url." + origin + "/.insteadOf"
	standingGit(t, root, "config", instead, "https://github.com/Kampe/Herdforge.git")
	standingGit(t, laneDir, "remote", "set-url", "origin", "https://github.com/Kampe/Herdforge.git")
	standingGit(t, laneDir, "config", instead, "https://github.com/Kampe/Herdforge.git")

	installProtocolFakeHerdr(t)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	t.Setenv("HERD_WORKSPACE", "wFAKE")
	t.Setenv("HERDR_WORKSPACE_ID", "wFAKE")
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("HERD_ERA_PROVIDERS", "grok")

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "grok"), []byte("#!/bin/sh\nprintf 'PROBE_OK'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinHealthyQuota(t, binDir, "grok")

	promptRel := filepath.Join(".herd", "prompts", "worker.md")
	if err := os.MkdirAll(filepath.Dir(promptRel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptRel, []byte("prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lane := &config.LaneDef{
		Name: "scout", Role: launch.ScoutPlannerRole, AgentKind: "grok", Harness: "grok",
		Provider: "grok", Model: "grok-4.6", Effort: "medium", TaskShape: "architecture",
		Standing: true, Worktree: "wt/scout", Prompt: promptRel,
		Authority: config.AuthorityWrite, Capabilities: []config.Capability{config.CapabilityGitWrite},
	}
	cfg := &config.Config{Lanes: []config.LaneDef{*lane}}

	err := runStandingConfigMode(cfg, true, standing.ModeRaise, []string{lane.Name}, true, false)
	got := standingGit(t, laneDir, "rev-parse", "HEAD")
	if got != fresh {
		t.Fatalf("shipped raise did not refresh standing worktree: HEAD %s want %s (raise err=%v)", got, fresh, err)
	}
	raw, readErr := os.ReadFile(filepath.Join(root, ".herd", "standing-admitted", "scout.json"))
	if readErr != nil {
		t.Fatalf("admitted base missing after shipped raise: %v (raise err=%v)", readErr, err)
	}
	var rec standingAdmittedBase
	if json.Unmarshal(raw, &rec) != nil || rec.BaseSHA != fresh {
		t.Fatalf("admitted record %+v want %s", rec, fresh)
	}
}
