package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// standingHookPolicyFixture drives the real public `herd standing` entrypoint
// (runStandingConfigMode -> standing.Run -> AdmitRoute -> launchAdmission ->
// preflightHooks -> harness.DefaultDiscovery), through a real -- not
// stubbed -- provider probe, so a removed AdmitRoute scoping wiring shows up
// as an actual production regression rather than passing on a mocked helper.
func standingHookPolicyFixture(t *testing.T, worktreeRel string) (root, targetDir string, cfg *config.Config, lane *config.LaneDef) {
	t.Helper()
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(key, os.Getenv(key))
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	root = t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		if out, err := gitCmdForTest(root, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "wt/hookpolicy-standing")
	runGit("commit", "-q", "--allow-empty", "-m", "base")

	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	t.Chdir(root)

	installProtocolFakeHerdr(t)
	if out, err := gitCmdForTest(root, "remote", "add", "origin", "https://example.invalid/fixture/standing-hookpolicy.git"); err != nil {
		t.Fatalf("fixture remote: %v: %s", err, out)
	}
	t.Setenv("HERD_WORKSPACE", "wFAKE")
	t.Setenv("HERDR_WORKSPACE_ID", "wFAKE")
	t.Setenv("HERD_MODE", "local")

	dir := t.TempDir()
	// The real (unstubbed) herdr.ProbeProviderModel execs this. grok's probe
	// delivers the prompt via --prompt-file and only checks stdout, so a fake
	// that ignores its args and answers the exact probe token is a faithful,
	// minimal stand-in for a live grok CLI -- no fake AdmitRoute, no stubbed
	// probe function.
	if err := os.WriteFile(filepath.Join(dir, "grok"), []byte("#!/bin/sh\nprintf 'PROBE_OK'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinHealthyQuota(t, dir, "grok")
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("HERD_ERA_PROVIDERS", "grok")

	promptRel := filepath.Join(".herd", "prompts", "worker.md")
	if err := os.MkdirAll(filepath.Dir(promptRel), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptRel, []byte("prompt\n"), 0600); err != nil {
		t.Fatal(err)
	}

	targetDir = filepath.Join(root, worktreeRel)
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		t.Fatal(err)
	}

	lane = &config.LaneDef{
		Name: "chain-indexer", Role: launch.ScoutPlannerRole, AgentKind: "grok", Harness: "grok",
		Provider: "grok", Model: "grok-4.6", Effort: "medium", TaskShape: "architecture",
		Standing: true, Worktree: worktreeRel, Prompt: promptRel,
		Authority: config.AuthorityWrite, Capabilities: []config.Capability{config.CapabilityGitWrite},
	}
	cfg = &config.Config{Lanes: []config.LaneDef{*lane}}
	return root, targetDir, cfg, lane
}

// TestStandingAdmitRouteUsesFreshTargetWorktreePolicyOverStaleCanonical is the
// public-caller regression FAC-624's standing defect needs: it drives
// runStandingConfigMode end to end through the real provider probe and the
// real launchAdmission/preflightHooks/harness.DefaultDiscovery chain. If
// AdmitRoute's worktree scoping (laneHookPolicyScope) is removed, this
// test goes RED on the production admission's real refusal -- not on a
// compile error, and not merely on the extracted helper -- because the
// canonical (coordinator cwd) pin is deliberately malformed while the lane's
// own target worktree pin is genuinely valid.
func TestStandingAdmitRouteUsesFreshTargetWorktreePolicyOverStaleCanonical(t *testing.T) {
	root, targetDir, cfg, lane := standingHookPolicyFixture(t, "target-worktree")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "harness-hooks.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	writeHookPolicyFile(t, filepath.Join(targetDir, ".herd", "harness-hooks.json"), "fresh-target-marker")

	if err := runStandingConfigMode(cfg, true, standing.ModeDryRun, []string{lane.Name}, true, false); err != nil {
		t.Fatalf("standing admission refused a launch against its own freshly pinned target policy (used stale/malformed canonical instead): %v", err)
	}
}

// standingClaudeAttemptFixture reproduces FAC-624's actual live condition
// through the real production discovery chain: a genuine live claude hook
// (parsed from a real settings.json, not a mock) merged with a repo override
// whose Policies come back empty, which is exactly how
// harness.ClaudeDiscovery's hardcoded PolicyRequired: true (independent of
// how many policies actually merged in) produces HookCodePolicySetMissing.
// The same lane/config fails the SAME way on every AdmitRoute call, which is
// what makes it useful for proving attempt-identity behavior: any two calls
// against this fixture are "genuinely distinct attempts at the identical
// admission," not distinguished by anything except when they were minted.
func standingClaudeAttemptFixture(t *testing.T) (root string, cfg *config.Config, lane *config.LaneDef) {
	t.Helper()
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(key, os.Getenv(key))
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	root = t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		if out, err := gitCmdForTest(root, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "wt/hookpolicy-attempt")
	runGit("commit", "-q", "--allow-empty", "-m", "base")

	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	t.Chdir(root)

	installProtocolFakeHerdr(t)
	if out, err := gitCmdForTest(root, "remote", "add", "origin", "https://example.invalid/fixture/standing-attempt.git"); err != nil {
		t.Fatalf("fixture remote: %v: %s", err, out)
	}
	t.Setenv("HERD_WORKSPACE", "wFAKE")
	t.Setenv("HERDR_WORKSPACE_ID", "wFAKE")
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(root, ".herd", "launch-receipts.jsonl"))
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0700); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	// The real (unstubbed) herdr.ProbeProviderModel execs this. claude's
	// probe delivers the prompt via stdin and only checks stdout.
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nprintf 'PROBE_OK'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinHealthyQuota(t, dir, "claude")
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("HERD_ERA_PROVIDERS", "claude")

	// A genuine, parseable settings.json with one real required hook --
	// harness.ClaudeDiscovery actually discovers this, it is not mocked.
	settingsPath := filepath.Join(dir, "settings.json")
	settings := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/bin/true"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CLAUDE_SETTINGS_FILE", settingsPath)

	// The repo override's "claude" entry exists (so hasOverride=true and its
	// Policies -- empty -- get merged into the real discovered claude
	// result) without itself declaring any hooks or policies. This is the
	// exact live condition: PolicyRequired=true (ClaudeDiscovery's hardcoded
	// value), Policies=[] (from the override), hooks discovered for real.
	if err := os.WriteFile(filepath.Join(root, ".herd", "harness-hooks.json"), []byte(`{"providers":{"claude":{"hooks":[],"policies":[]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	promptRel := filepath.Join(".herd", "prompts", "worker.md")
	if err := os.MkdirAll(filepath.Dir(promptRel), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptRel, []byte("prompt\n"), 0600); err != nil {
		t.Fatal(err)
	}

	lane = &config.LaneDef{
		Name: "chain-indexer", Role: launch.WorkerRole, AgentKind: "claude", Harness: "claude",
		Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: launch.Implementation,
		Standing: true, Worktree: ".", Prompt: promptRel,
		Authority: config.AuthorityWrite, Capabilities: []config.Capability{config.CapabilityGitWrite},
	}
	cfg = &config.Config{Lanes: []config.LaneDef{*lane}}
	return root, cfg, lane
}

// TestStandingAdmitRouteAttemptIdentityDistinguishesTwoAttemptsInSameProcess
// is the public-caller regression FAC-624's attempt-identity correction
// needs: two genuinely separate calls to the real public entrypoint
// (runStandingConfigMode), in the SAME test process, for the SAME failing
// lane, must each leave their own refusal receipt -- exactly the shape of
// `herd forge --loop`'s in-process AdmitRoute rearm across many ticks of one
// long-lived coordinator. If standing's per-attempt launch.NewAttemptID
// minting is removed, both calls fall back to the same bare-PID identity and
// the second call's receipt is silently deduplicated against the first --
// reproducing FAC-624's original symptom, just at "process lifetime"
// granularity instead of "minute" granularity.
func TestStandingAdmitRouteAttemptIdentityDistinguishesTwoAttemptsInSameProcess(t *testing.T) {
	root, cfg, lane := standingClaudeAttemptFixture(t)

	if err := runStandingConfigMode(cfg, true, standing.ModeDryRun, []string{lane.Name}, true, false); err == nil {
		t.Fatal("empty policy set must fail closed on the first attempt")
	}
	if err := runStandingConfigMode(cfg, true, standing.ModeDryRun, []string{lane.Name}, true, false); err == nil {
		t.Fatal("empty policy set must fail closed on the second attempt")
	}

	var policySetMissing []launch.Receipt
	for _, r := range readReceipts(t, root) {
		if r.HookCode == "hook.policy_set_missing" {
			policySetMissing = append(policySetMissing, r)
		}
	}
	if len(policySetMissing) != 2 {
		t.Fatalf("two genuinely separate public admissions in the same process did not each get their own receipt: %+v", policySetMissing)
	}
	if policySetMissing[0].ReceiptKey == policySetMissing[1].ReceiptKey {
		t.Fatalf("two distinct standing admission attempts in one process collapsed onto the same receipt key: %q", policySetMissing[0].ReceiptKey)
	}
}

// TestForgeLaunchAdmissionAttemptIdentityDistinguishesTwoAttemptsInSameProcess
// is the same public-caller regression as standing's, for forgeLaunchAdmission
// -- called repeatedly for the same lane over the lifetime of a long-lived
// `herd forge --loop` coordinator process. Reuses standingClaudeAttemptFixture
// unchanged: the exact same live-condition reproduction, a different real
// public entrypoint.
func TestForgeLaunchAdmissionAttemptIdentityDistinguishesTwoAttemptsInSameProcess(t *testing.T) {
	root, cfg, lane := standingClaudeAttemptFixture(t)
	noop := func(*router.LaunchDecision) error { return nil }

	if _, err := forgeLaunchAdmission(cfg, lane, context.Background(), noop); err == nil {
		t.Fatal("empty policy set must fail closed on the first attempt")
	}
	if _, err := forgeLaunchAdmission(cfg, lane, context.Background(), noop); err == nil {
		t.Fatal("empty policy set must fail closed on the second attempt")
	}

	var policySetMissing []launch.Receipt
	for _, r := range readReceipts(t, root) {
		if r.HookCode == "hook.policy_set_missing" {
			policySetMissing = append(policySetMissing, r)
		}
	}
	if len(policySetMissing) != 2 {
		t.Fatalf("two genuinely separate forgeLaunchAdmission attempts in the same process did not each get their own receipt: %+v", policySetMissing)
	}
	if policySetMissing[0].ReceiptKey == policySetMissing[1].ReceiptKey {
		t.Fatalf("two distinct forgeLaunchAdmission attempts in one process collapsed onto the same receipt key: %q", policySetMissing[0].ReceiptKey)
	}
}
