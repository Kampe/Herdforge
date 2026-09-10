package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/harness"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
)

// policyCapturingRuntime records the live harness.DefaultDiscovery result and
// the HERD_HARNESS_HOOKS_FILE environment exactly as it stands the moment
// Open is invoked -- the same moment the real liveUpRuntime would open the
// pane -- without needing a real herdr socket.
type policyCapturingRuntime struct {
	decision    *router.LaunchDecision
	discovery   harness.HookDiscoveryResult
	discoverErr error
	envAtOpen   string
	opens       int
}

func (*policyCapturingRuntime) Available() bool { return true }
func (r *policyCapturingRuntime) Route(*config.LaneDef) (*router.LaunchDecision, error) {
	return r.decision, nil
}
func (r *policyCapturingRuntime) Open(_ *router.LaunchDecision, _ launch.Request, _ *config.LaneDef, _, _, _ string) (*herdr.TabInfo, error) {
	r.opens++
	r.envAtOpen = os.Getenv("HERD_HARNESS_HOOKS_FILE")
	r.discovery, r.discoverErr = harness.DefaultDiscovery{}.Discover("codex")
	if r.discoverErr != nil {
		return nil, r.discoverErr
	}
	return &herdr.TabInfo{ID: "wFAKE:t1", Pane: herdr.PaneInfo{ID: "wFAKE:p1"}}, nil
}
func (*policyCapturingRuntime) Ready(*herdr.TabInfo) error { return nil }
func (*policyCapturingRuntime) Start(_, _, _, _ string, _ launch.Request) error { return nil }
func (*policyCapturingRuntime) Close(string, *herdr.TabInfo) error {
	return errors.New("unexpected compensation")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeHookPolicyFile writes a minimal harness-hooks.json for the "codex"
// provider carrying one distinguishing marker in approved_local_authorities.
// codex has no live hooks of its own, so leaving "hooks" and "policies" empty
// avoids the unrelated hook<->policy handler-matching gate (hook.policy_mismatch)
// while still letting DefaultDiscovery's returned result differ per file.
func writeHookPolicyFile(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"providers":{"codex":{"hooks":[],"policies":[],"approved_local_authorities":[%q]}}}`, marker)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// upHookPolicyFixture mirrors upReceiptFixture but lets the caller place the
// lane's worktree at a named subdirectory instead of "." so canonical (cwd)
// and target (lane.Worktree) hook policy files can genuinely diverge.
func upHookPolicyFixture(t *testing.T, worktreeRel string) (root, targetDir string, runtime *policyCapturingRuntime) {
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
	runGit("init", "-q", "-b", "wt/hookpolicy")
	runGit("commit", "-q", "--allow-empty", "-m", "base")

	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	t.Chdir(root)

	installProtocolFakeHerdr(t)
	if out, err := gitCmdForTest(root, "remote", "add", "origin", "https://example.invalid/fixture/hookpolicy.git"); err != nil {
		t.Fatalf("fixture remote: %v: %s", err, out)
	}
	t.Setenv("HERD_PROJECT_ROOT", root)
	t.Setenv("HERD_CONFIG_PATH", filepath.Join(root, config.DefaultConfigPath))
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(root, ".herd", "launch-receipts.jsonl"))
	t.Setenv("HERD_WORKSPACE", "wFAKE")
	t.Setenv("HERDR_WORKSPACE_ID", "wFAKE")
	t.Setenv("HERD_WINDDOWN_STATE", filepath.Join(root, ".herd", "winddown.json"))
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "1")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0700); err != nil {
		t.Fatal(err)
	}
	targetDir = filepath.Join(root, worktreeRel)
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version: "1"
project:
  name: Herdforge
task_provider:
  type: memory
fleet:
  herdr_workspace: wFAKE
lanes:
  - name: worker
    role: worker
    agent_kind: codex
    harness: codex
    provider: codex
    model: gpt-5.6-luna
    effort: medium
    prompt: .herd/prompts/worker.md
    task_shape: implementation
    standing: true
    worktree: %s
`, worktreeRel)
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := initializeWinddownState(); err != nil {
		t.Fatal(err)
	}
	d, err := testLaunchRouter(t).Decide(launchBoundaryDecideRequest())
	if err != nil {
		t.Fatal(err)
	}
	runtime = &policyCapturingRuntime{decision: d}
	return root, targetDir, runtime
}

// TestUpCommandUsesFreshTargetWorktreePolicyOverStaleCanonical reproduces the
// production regression: `herd up` resolved hook policy relative to its own
// (canonical) process cwd, so a stale canonical .herd/harness-hooks.json
// rejected a launch even though the exact target lane worktree had a freshly
// pinned policy. `herd review` does not have this bug -- it already scopes
// discovery to the candidate worktree via useHarnessHooksFromWorktree.
func TestUpCommandUsesFreshTargetWorktreePolicyOverStaleCanonical(t *testing.T) {
	root, targetDir, runtime := upHookPolicyFixture(t, "target-worktree")
	writeHookPolicyFile(t, filepath.Join(root, ".herd", "harness-hooks.json"), "stale-canonical-marker")
	writeHookPolicyFile(t, filepath.Join(targetDir, ".herd", "harness-hooks.json"), "fresh-target-marker")

	if err := runUpCommand("worker", runtime, os.Stdout); err != nil {
		t.Fatalf("up refused a launch against its own freshly pinned target policy: %v", err)
	}
	if runtime.opens != 1 {
		t.Fatalf("want one open, got %d", runtime.opens)
	}
	want := []string{"fresh-target-marker"}
	if !equalStrings(runtime.discovery.ApprovedAuthorities, want) {
		t.Fatalf("resolved canonical (stale) policy instead of the target worktree's fresh pin: got %v want %v", runtime.discovery.ApprovedAuthorities, want)
	}
}

// TestUpCommandRefusesMalformedTargetWorktreePolicy proves the fix fails
// closed rather than silently falling back to canonical or bypassing
// discovery when the target worktree's own pin file is malformed.
func TestUpCommandRefusesMalformedTargetWorktreePolicy(t *testing.T) {
	root, targetDir, runtime := upHookPolicyFixture(t, "target-worktree")
	writeHookPolicyFile(t, filepath.Join(root, ".herd", "harness-hooks.json"), "canonical-marker")
	if err := os.MkdirAll(filepath.Join(targetDir, ".herd"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, ".herd", "harness-hooks.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}

	err := runUpCommand("worker", runtime, os.Stdout)
	if err == nil {
		t.Fatal("malformed target worktree hook policy did not refuse the launch")
	}
	if runtime.opens != 0 {
		t.Fatalf("malformed target policy still opened a tab: %d", runtime.opens)
	}
	// A rejected launch legitimately records an audit-only launch_rejected
	// receipt (Accepted: false); the fix must never turn that into an
	// Accepted one.
	for _, r := range readReceipts(t, root) {
		if r.Accepted {
			t.Fatalf("malformed target policy produced an accepted receipt: %+v", r)
		}
	}
}

// TestUpCommandExplicitOverrideBoundsWorktreePolicy proves an operator's
// explicit HERD_HARNESS_HOOKS_FILE still wins over both the canonical and the
// target worktree's own pin -- the same boundary useHarnessHooksFromWorktree
// already preserves for `herd review`.
func TestUpCommandExplicitOverrideBoundsWorktreePolicy(t *testing.T) {
	root, targetDir, runtime := upHookPolicyFixture(t, "target-worktree")
	writeHookPolicyFile(t, filepath.Join(root, ".herd", "harness-hooks.json"), "canonical-marker")
	writeHookPolicyFile(t, filepath.Join(targetDir, ".herd", "harness-hooks.json"), "target-marker-ignored")
	operatorFile := filepath.Join(root, "operator-pin.json")
	writeHookPolicyFile(t, operatorFile, "operator-explicit-marker")
	t.Setenv("HERD_HARNESS_HOOKS_FILE", operatorFile)

	if err := runUpCommand("worker", runtime, os.Stdout); err != nil {
		t.Fatalf("up refused a launch under an explicit operator override: %v", err)
	}
	if runtime.envAtOpen != operatorFile {
		t.Fatalf("target worktree scoping clobbered the operator's explicit override: got %q want %q", runtime.envAtOpen, operatorFile)
	}
	want := []string{"operator-explicit-marker"}
	if !equalStrings(runtime.discovery.ApprovedAuthorities, want) {
		t.Fatalf("resolved policy ignored the explicit operator override: got %v want %v", runtime.discovery.ApprovedAuthorities, want)
	}
}

// TestUpCommandExplicitMalformedOverrideIsNotReplacedByTargetPolicy proves the
// worktree-scoping fix never second-guesses an operator's explicit override:
// even when that override file is itself malformed, useHarnessHooksFromWorktree
// must not silently substitute the target worktree's own (valid) pin. The
// launch fails closed on the operator's file, exactly as it would have before
// this correction existed.
func TestUpCommandExplicitMalformedOverrideIsNotReplacedByTargetPolicy(t *testing.T) {
	root, targetDir, runtime := upHookPolicyFixture(t, "target-worktree")
	writeHookPolicyFile(t, filepath.Join(root, ".herd", "harness-hooks.json"), "canonical-marker")
	writeHookPolicyFile(t, filepath.Join(targetDir, ".herd", "harness-hooks.json"), "target-marker-must-not-be-used")
	operatorFile := filepath.Join(root, "operator-pin.json")
	if err := os.WriteFile(operatorFile, []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_HARNESS_HOOKS_FILE", operatorFile)

	err := runUpCommand("worker", runtime, os.Stdout)
	if err == nil {
		t.Fatal("malformed explicit operator override did not refuse the launch")
	}
	if runtime.opens != 0 {
		t.Fatalf("malformed explicit override still opened a tab: %d", runtime.opens)
	}
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != operatorFile {
		t.Fatalf("HERD_HARNESS_HOOKS_FILE was not left exactly as the operator set it: got %q want %q", got, operatorFile)
	}
	for _, r := range readReceipts(t, root) {
		if r.Accepted {
			t.Fatalf("malformed explicit override produced an accepted receipt: %+v", r)
		}
	}
}

// TestUpCommandExplicitOverrideToMissingFileIsNotReplacedByTargetPolicy covers
// an operator override pointed at a file that doesn't exist yet (e.g. not
// pinned this cycle): DefaultDiscovery already treats a missing explicit
// override as a hard failure (harness.go:285), and the worktree-scoping fix
// must preserve that -- not fall back to the target's own valid pin.
func TestUpCommandExplicitOverrideToMissingFileIsNotReplacedByTargetPolicy(t *testing.T) {
	root, targetDir, runtime := upHookPolicyFixture(t, "target-worktree")
	writeHookPolicyFile(t, filepath.Join(root, ".herd", "harness-hooks.json"), "canonical-marker")
	writeHookPolicyFile(t, filepath.Join(targetDir, ".herd", "harness-hooks.json"), "target-marker-must-not-be-used")
	operatorFile := filepath.Join(root, "does-not-exist.json")
	t.Setenv("HERD_HARNESS_HOOKS_FILE", operatorFile)

	err := runUpCommand("worker", runtime, os.Stdout)
	if err == nil {
		t.Fatal("explicit operator override to a missing file did not refuse the launch")
	}
	if runtime.opens != 0 {
		t.Fatalf("missing explicit override still opened a tab: %d", runtime.opens)
	}
	if got := os.Getenv("HERD_HARNESS_HOOKS_FILE"); got != operatorFile {
		t.Fatalf("HERD_HARNESS_HOOKS_FILE was not left exactly as the operator set it: got %q want %q", got, operatorFile)
	}
}

// TestUpCommandRestoresEnvironmentAfterRefusedLaunch proves the deferred
// restore fires on every exit path, not only the success path -- a leaked
// HERD_HARNESS_HOOKS_FILE from one refused lane launch must never leak into
// whatever the coordinator process does next.
func TestUpCommandRestoresEnvironmentAfterRefusedLaunch(t *testing.T) {
	root, targetDir, runtime := upHookPolicyFixture(t, "target-worktree")
	writeHookPolicyFile(t, filepath.Join(root, ".herd", "harness-hooks.json"), "canonical-marker")
	if err := os.MkdirAll(filepath.Join(targetDir, ".herd"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, ".herd", "harness-hooks.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, hadPrevious := os.LookupEnv("HERD_HARNESS_HOOKS_FILE"); hadPrevious {
		t.Fatal("test environment already has HERD_HARNESS_HOOKS_FILE set")
	}

	if err := runUpCommand("worker", runtime, os.Stdout); err == nil {
		t.Fatal("malformed target worktree hook policy did not refuse the launch")
	}
	if got, isSet := os.LookupEnv("HERD_HARNESS_HOOKS_FILE"); isSet {
		t.Fatalf("HERD_HARNESS_HOOKS_FILE leaked past a refused launch: %q", got)
	}
}

// TestUpCommandFallsBackToCanonicalPolicyWhenTargetHasNone covers the no-target
// case: when the lane worktree has no pin file of its own, behavior is
// unchanged from before this correction -- canonical discovery still applies.
func TestUpCommandFallsBackToCanonicalPolicyWhenTargetHasNone(t *testing.T) {
	root, _, runtime := upHookPolicyFixture(t, "target-worktree")
	writeHookPolicyFile(t, filepath.Join(root, ".herd", "harness-hooks.json"), "canonical-only-marker")

	if err := runUpCommand("worker", runtime, os.Stdout); err != nil {
		t.Fatalf("up refused a launch with no target-specific policy: %v", err)
	}
	if runtime.envAtOpen != "" {
		t.Fatalf("set HERD_HARNESS_HOOKS_FILE with no target pin file present: %q", runtime.envAtOpen)
	}
	want := []string{"canonical-only-marker"}
	if !equalStrings(runtime.discovery.ApprovedAuthorities, want) {
		t.Fatalf("did not fall back to canonical policy: got %v want %v", runtime.discovery.ApprovedAuthorities, want)
	}
}
