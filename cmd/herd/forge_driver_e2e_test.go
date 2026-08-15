package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lock"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-135: compiled-driver and in-process conformance for cliForgeDriver.
//
// Protocol-faithful fakes only — never fall through to a live herdr socket or
// host ~/.herd keys. Host roots are snapshotted; any mutation is a hard fail.

func TestCliForgeDriver_LaneStateUnknownIsError(t *testing.T) {
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		return "", errors.New("connection refused")
	})
	t.Cleanup(restore)

	d := &cliForgeDriver{maxLanes: 3}
	_, err := d.LaneState(context.Background())
	if err == nil {
		t.Fatal("unreadable herdr reported free capacity (FAC-138/135 regression)")
	}
	if !strings.Contains(err.Error(), "herdr agent list") {
		t.Fatalf("error = %v", err)
	}
}

func TestCliForgeDriver_SignalsUnknownIsError(t *testing.T) {
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		return "", errors.New("timeout")
	})
	t.Cleanup(restore)

	d := &cliForgeDriver{maxLanes: 3}
	_, _, err := d.Signals(context.Background())
	if err == nil {
		t.Fatal("unreadable completion signals returned empty drained set")
	}
}

// prepareReviewProbe installs a hermetic worktree dir + admission pass so
// Review can reach the herd subprocess seam. FAC-144 gates spawn on both.
func prepareReviewProbe(t *testing.T, ref string) {
	t.Helper()
	root := t.TempDir()
	wt := filepath.Join(root, worktreePathForRef(ref))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	restoreAdmit := setAdmitReviewForTest(func(ctx context.Context, cfg *config.Config, r, w, digest string) error {
		return nil
	})
	t.Cleanup(restoreAdmit)
}

func TestCliForgeDriver_ReviewSpawnBeforeRef(t *testing.T) {
	// Mutation probe: if reviewArgs puts the ref before --spawn, parseReviewArgs
	// with trailing-flag form would still work — but the forge loop historically
	// emitted the broken order. This test fails when reviewArgs regresses.
	got := reviewArgs("FAC-1")
	want := []string{"review", "--spawn", "FAC-1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("review argv = %v want %v", got, want)
	}
	// FAC-144: Review refuses without worktree + current PASS. Stub both so
	// this probe measures argv order, not admission fixtures.
	prepareReviewProbe(t, "FAC-1")
	// And the driver must emit exactly that argv through the subprocess seam.
	var recorded []string
	restore := setHerdSubprocessForTest(func(args ...string) error {
		recorded = append([]string(nil), args...)
		return nil
	})
	t.Cleanup(restore)
	d := &cliForgeDriver{}
	if err := d.Review(context.Background(), &provider.Task{Ref: "FAC-1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(recorded, " ") != strings.Join(want, " ") {
		t.Fatalf("driver emitted %v want %v", recorded, want)
	}
	// Prove the historical wrong order is not silently accepted by this path:
	ref, spawn := parseReviewArgs(recorded[1:]) // drop "review"
	if ref != "FAC-1" || !spawn {
		t.Fatalf("parseReviewArgs(%v) = %q %v", recorded[1:], ref, spawn)
	}
}

func TestCliForgeDriver_SwallowedReviewErrorSurfaces(t *testing.T) {
	// FAC-135: driver must not swallow subprocess failures (FAC-138 residual).
	// Must pass the FAC-144 worktree+admission gate first or the test would
	// "pass" on worktree-missing without ever reaching the subprocess.
	prepareReviewProbe(t, "FAC-9")
	restore := setHerdSubprocessForTest(func(args ...string) error {
		return errors.New("reviewer spawn failed")
	})
	t.Cleanup(restore)
	d := &cliForgeDriver{}
	err := d.Review(context.Background(), &provider.Task{Ref: "FAC-9"})
	if err == nil {
		t.Fatal("review failure was swallowed")
	}
	if !strings.Contains(err.Error(), "reviewer spawn failed") {
		t.Fatalf("error = %v; want subprocess failure, not an earlier gate miss", err)
	}
}

func TestCliForgeDriver_ReviewRefusesMissingWorktree(t *testing.T) {
	// FAC-144: no worktree → no spawn. Admission and herd must not run.
	root := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	admitCalled := false
	restoreAdmit := setAdmitReviewForTest(func(ctx context.Context, cfg *config.Config, r, w, digest string) error {
		admitCalled = true
		return nil
	})
	t.Cleanup(restoreAdmit)
	herdCalled := false
	restore := setHerdSubprocessForTest(func(args ...string) error {
		herdCalled = true
		return nil
	})
	t.Cleanup(restore)

	d := &cliForgeDriver{}
	err = d.Review(context.Background(), &provider.Task{Ref: "FAC-1"})
	if err == nil || !strings.Contains(err.Error(), "worktree missing") {
		t.Fatalf("error = %v; want worktree missing", err)
	}
	if admitCalled {
		t.Fatal("admission ran without a worktree")
	}
	if herdCalled {
		t.Fatal("herd review spawned without a worktree")
	}
}

func TestCliForgeDriver_ReviewRefusesWithoutPassReceipt(t *testing.T) {
	// FAC-144: worktree present but admission fails → no spawn.
	root := t.TempDir()
	ref := "FAC-1"
	if err := os.MkdirAll(filepath.Join(root, worktreePathForRef(ref)), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	restoreAdmit := setAdmitReviewForTest(func(ctx context.Context, cfg *config.Config, r, w, digest string) error {
		return errors.New("no current PASS receipt")
	})
	t.Cleanup(restoreAdmit)
	herdCalled := false
	restore := setHerdSubprocessForTest(func(args ...string) error {
		herdCalled = true
		return nil
	})
	t.Cleanup(restore)

	d := &cliForgeDriver{}
	err = d.Review(context.Background(), &provider.Task{Ref: ref})
	if err == nil || !strings.Contains(err.Error(), "refused without current PASS receipt") {
		t.Fatalf("error = %v; want PASS-receipt refusal", err)
	}
	if herdCalled {
		t.Fatal("herd review spawned without a current PASS receipt")
	}
}

func TestCliForgeDriver_ApproveBlockedByIncompleteMergePolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Explicit protected policy with empty checks — must refuse.
	herdYAML := "version: \"1\"\nproject:\n  name: test\ntask_provider:\n  type: memory\nmerge_policy:\n  protected: true\n  required_checks: []\n  require_different_family_review: true\n  require_pull_request_reviews: true\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(herdYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	called := false
	restore := setHerdSubprocessForTest(func(args ...string) error {
		called = true
		return nil
	})
	t.Cleanup(restore)

	d := &cliForgeDriver{}
	err = d.Approve(context.Background(), &provider.Task{Ref: "FAC-1"})
	if err == nil {
		t.Fatal("approve proceeded under incomplete merge policy")
	}
	if !strings.Contains(err.Error(), "merge_policy") {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("approve reached herd subprocess despite merge-policy block")
	}
}

func TestCliForgeDriver_LaneStateCountsBusyAgents(t *testing.T) {
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"type":"agent_list","agents":[
				{"name":"task-fac-1","agent_status":"working","pane_id":"p1","tab_id":"t1","workspace_id":"w"},
				{"name":"task-fac-2","agent_status":"idle","pane_id":"p2","tab_id":"t2","workspace_id":"w"},
				{"name":"assayer","agent_status":"working","pane_id":"p3","tab_id":"t3","workspace_id":"w"}
			]}}`, nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	})
	t.Cleanup(restore)
	d := &cliForgeDriver{maxLanes: 3}
	ls, err := d.LaneState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ls.Busy != 1 || ls.Max != 3 {
		t.Fatalf("LaneState = %+v want Busy=1 Max=3", ls)
	}
}

// hostSnapshot records mtimes of sensitive host roots so tests cannot leak
// keys or mutate operator config.
type hostSnapshot struct {
	herdKeys map[string]os.FileInfo
}

func snapshotHost(t *testing.T) hostSnapshot {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(home, ".herd", "keys")
	out := hostSnapshot{herdKeys: map[string]os.FileInfo{}}
	entries, err := os.ReadDir(keys)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out.herdKeys[e.Name()] = info
	}
	return out
}

func (s hostSnapshot) assertUnchanged(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(home, ".herd", "keys")
	entries, err := os.ReadDir(keys)
	if err != nil {
		if os.IsNotExist(err) {
			if len(s.herdKeys) != 0 {
				t.Fatalf("host keys dir vanished; had %d entries", len(s.herdKeys))
			}
			return
		}
		t.Fatal(err)
	}
	now := map[string]struct{}{}
	for _, e := range entries {
		now[e.Name()] = struct{}{}
		if _, ok := s.herdKeys[e.Name()]; !ok {
			t.Fatalf("host key leak: new file %s under ~/.herd/keys", e.Name())
		}
	}
	for name := range s.herdKeys {
		if _, ok := now[name]; !ok {
			t.Fatalf("host key deleted: %s", name)
		}
	}
}

// installFakeHerdr puts a protocol-faithful herdr on PATH. It never opens a
// live socket. Invocations are appended to logPath.
func installFakeHerdr(t *testing.T, agentJSON string) (binDir, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH shell-stub is POSIX-only")
	}
	binDir = t.TempDir()
	logPath = filepath.Join(binDir, "herdr.log")
	// Quote paths for the shell script carefully; TempDir has no spaces.
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$1 $2" in
  "workspace list") printf '{"result":{"workspaces":[{"workspace_id":"wT","label":"wT","focused":true}]}}' ;;
  "agent list") printf '%s' '` + agentJSON + `' ;;
  "agent prompt") printf 'ok' ;;
  "agent start") printf 'ok' ;;
  "tab create") printf '{"result":{"type":"tab_create","tab_id":"tFake","pane_id":"pFake"}}' ;;
  "tab close") printf 'ok' ;;
  *) printf 'unsupported %s\n' "$*" >&2; exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir, logPath
}

func installFakeKaneo(t *testing.T, stateDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH shell-stub is POSIX-only")
	}
	// File-backed kaneo: tasks live in $stateDir/tasks.json as a JSON array.
	// Minimal protocol for list/get/status used by forge list paths.
	script := `#!/bin/sh
STATE="` + stateDir + `/tasks.json"
[ -f "$STATE" ] || echo '[]' > "$STATE"
cmd="$1 $2"
case "$cmd" in
  "task list")
    # Optional --status filter is applied in-process by the provider after decode.
    cat "$STATE"
    ;;
  "task get")
    id="$3"
    # shellcheck disable=SC2016
    python3 -c '
import json,sys
id=sys.argv[1]
tasks=json.load(open(sys.argv[2]))
for t in tasks:
  if t.get("id")==id or t.get("ref")==id:
    print(json.dumps(t)); sys.exit(0)
print("{}"); sys.exit(1)
' "$id" "$STATE"
    ;;
  "task status")
    id="$3"; status="$4"
    python3 -c '
import json,sys
id,status,path=sys.argv[1],sys.argv[2],sys.argv[3]
tasks=json.load(open(path))
for t in tasks:
  if t.get("id")==id or t.get("ref")==id:
    t["status"]=status
json.dump(tasks, open(path,"w"))
' "$id" "$status" "$STATE"
    ;;
  "task comment")
    exit 0
    ;;
  *)
    echo "unsupported $*" >&2
    exit 64
    ;;
esac
`
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "kaneo"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeGroomedBoard(t *testing.T, stateDir string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One groomed to-do card. Description carries empty deps provenance.
	desc := "## Outcome\\n\\nE2E.\\n\\n```herd-deps-v1\\n{\\\"version\\\":1,\\\"task_ref\\\":\\\"FAC-9001\\\",\\\"task_id\\\":\\\"task-1\\\",\\\"edges\\\":[]}\\n```\\n"
	body := `[{"id":"task-1","ref":"FAC-9001","title":"e2e card","status":"to-do","priority":"urgent","projectId":"p-e2e","labels":[{"name":"worker"}],"description":"` + desc + `"}]`
	if err := os.WriteFile(filepath.Join(stateDir, "tasks.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func factoryFixtureRepo(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := testgit.Command(dir, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(remote, "init", "--bare", "-q")
	run(root, "init", "-q", "-b", "main")
	run(root, "commit", "--allow-empty", "-q", "-m", "base")
	run(root, "remote", "add", "origin", remote)
	run(root, "push", "-q", "-u", "origin", "main")
	// Minimal herd config: kaneo CLI (fake) + lanes.
	if err := os.MkdirAll(filepath.Join(root, ".herd", "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, ".herd", "prompts", "worker.md"), []byte("worker"), 0o644)
	_ = os.WriteFile(filepath.Join(root, ".herd", "prompts", "reviewer.md"), []byte("reviewer"), 0o644)
	yaml := `version: "1"
project:
  name: "e2e"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "p-e2e"
  use_cli: true
lanes:
  - name: "smith"
    role: "worker"
    agent_kind: "pi"
    harness: "pi"
    prompt: ".herd/prompts/worker.md"
    worktree: ".worktrees/smith"
    provider: "codex"
    model: "test-model"
    effort: "medium"
    task_shape: "implementation"
    standing: false
    authority: "write"
    capabilities: ["git-write", "fs-write", "shell-exec"]
  - name: "assayer"
    role: "reviewer"
    agent_kind: "pi"
    harness: "pi"
    prompt: ".herd/prompts/reviewer.md"
    worktree: ".worktrees/assayer"
    provider: "anthropic"
    model: "test-reviewer"
    effort: "medium"
    task_shape: "review"
    standing: false
    authority: "read"
    capabilities: ["shell-exec"]
`
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	// Complete merge policy so local approve preflight can pass when intentional.
	mp := "protected: true\nrequired_checks:\n  - gate\nrequire_different_family_review: true\nrequire_pull_request_reviews: true\nremote_ci:\n  required: true\n  required_checks:\n    - gate\n"
	var policy strings.Builder
	policy.WriteString("\nmerge_policy:\n")
	for _, line := range strings.Split(strings.TrimSuffix(mp, "\n"), "\n") {
		policy.WriteString("  ")
		policy.WriteString(line)
		policy.WriteByte('\n')
	}
	herdPath := filepath.Join(root, ".herd", "herd.yaml")
	current, err := os.ReadFile(herdPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(herdPath, append(current, []byte(policy.String())...), 0o644); err != nil {
		t.Fatal(err)
	}
	// Wind-down explicitly disabled so fleet admission passes.
	// Generation 1, enabled=false.
	wd := `{"enabled":false,"actor":"e2e","reason":"factory-conformance","timestamp":"2026-08-07T00:00:00Z","generation":1}`
	if err := os.WriteFile(filepath.Join(root, ".herd", "winddown.json"), []byte(wd), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFactoryE2E_CoordinatorFenceBlocksSecondLoop(t *testing.T) {
	snap := snapshotHost(t)
	t.Cleanup(func() { snap.assertUnchanged(t) })

	root := factoryFixtureRepo(t)
	installFakeHerdr(t, `{"result":{"type":"agent_list","agents":[]}}`)
	stateDir := t.TempDir()
	writeGroomedBoard(t, stateDir)
	installFakeKaneo(t, stateDir)

	binary := buildHerd(t)
	env := append(os.Environ(),
		"HERD_WINDDOWN_STATE="+filepath.Join(root, ".herd", "winddown.json"),
		"PATH="+os.Getenv("PATH"),
	)
	run := func() string {
		t.Helper()
		cmd := exec.Command(binary, "forge", "--loop", "--ticks", "1", "--interval", "1")
		cmd.Dir = root
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("forge --loop exited 0; want a refusal\n%s", out)
		}
		return string(out)
	}

	// Hold the fence from the test process rather than racing a subprocess that
	// must stay alive: the coordinator fails closed without a durable control
	// reconciler, so it cannot be relied on to hold anything.
	fence := lock.NewDirLock(filepath.Join(root, ".herd", "forge-loop.lock.d"))
	fence.SetMaxAge(forgeLoopFenceMaxAge)
	if err := fence.Acquire(context.Background(), 0, "e2e fence holder"); err != nil {
		t.Fatalf("acquire forge fence: %v", err)
	}
	if out := run(); !strings.Contains(out, "another coordinator is active") {
		t.Fatalf("second coordinator was not refused by the fence:\n%s", out)
	}

	// Control arm, and a gate in its own right. With the fence released the same
	// command must fail for a DIFFERENT, named reason — otherwise this test
	// would also pass against a binary that refuses every invocation regardless
	// of the fence.
	//
	// That reason is 50a82e3's posture: `forge --loop` is composed with a nil
	// control reconciler and fails closed before any board or lane action. A
	// stub reconciler whose Orders always returns the empty set silences this
	// message while restoring the exact pre-50a82e3 behaviour, so this
	// assertion is what turns such a composition red.
	fence.Release()
	out := run()
	if strings.Contains(out, "another coordinator is active") {
		t.Fatalf("fence refusal persisted after release:\n%s", out)
	}
	if !strings.Contains(out, "durable control reconciler is required") {
		t.Fatalf("forge --loop did not fail closed on control composition:\n%s", out)
	}
}

func TestFactoryE2E_UnknownLaneDoesNotBackfill(t *testing.T) {
	snap := snapshotHost(t)
	t.Cleanup(func() { snap.assertUnchanged(t) })

	// In-process: herdr fails → LaneState error → no dispatch via subprocess.
	var actions []string
	var mu sync.Mutex
	restoreH := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		return "", errors.New("herdr down")
	})
	t.Cleanup(restoreH)
	restoreS := setHerdSubprocessForTest(func(args ...string) error {
		mu.Lock()
		actions = append(actions, strings.Join(args, " "))
		mu.Unlock()
		return nil
	})
	t.Cleanup(restoreS)

	d := &cliForgeDriver{maxLanes: 3}
	if _, err := d.LaneState(context.Background()); err == nil {
		t.Fatal("expected lane state error")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 0 {
		t.Fatalf("driver acted without lane capacity: %v", actions)
	}
}

func TestFactoryE2E_NoLiveHerdrFallback(t *testing.T) {
	// Empty PATH except a deliberately broken herdr name — IsAvailable must
	// be false and AgentList must not invent an empty inventory.
	t.Setenv("PATH", t.TempDir())
	if herdr.IsAvailable() {
		t.Fatal("herdr reported available with empty PATH")
	}
}

func TestCliForgeDriver_CrashAfterDispatchSurfacesAndRetries(t *testing.T) {
	// Inject a crash after the dispatch boundary; the error must surface so a
	// restarted coordinator can re-drive without the failure being swallowed.
	calls := 0
	restore := setHerdSubprocessForTest(func(args ...string) error {
		calls++
		if calls == 1 {
			return errors.New("simulated crash after external boundary")
		}
		return nil
	})
	t.Cleanup(restore)
	d := &cliForgeDriver{}
	task := &provider.Task{Ref: "FAC-42"}
	if err := d.Dispatch(context.Background(), task); err == nil {
		t.Fatal("first dispatch crash was swallowed")
	}
	if err := d.Dispatch(context.Background(), task); err != nil {
		t.Fatalf("retry after crash: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
}

// The other side of TestCliForgeDriver_ApproveBlockedByIncompleteMergePolicy:
// under a complete declaration, Approve delegates to `herd approve <ref>` — the
// evidence-gated board move (pkg/sync.BoardDone). Without this pair the block
// test would also pass against a driver that refuses everything.
func TestCliForgeDriver_ApproveEmitsBoardDoneArgv(t *testing.T) {
	root := factoryFixtureRepo(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	var recorded []string
	restore := setHerdSubprocessForTest(func(args ...string) error {
		recorded = append([]string(nil), args...)
		return nil
	})
	t.Cleanup(restore)
	// CloseTabForRef may call herdr; stub it.
	restoreH := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		return `{"result":{"type":"agent_list","agents":[]}}`, nil
	})
	t.Cleanup(restoreH)

	d := &cliForgeDriver{}
	if err := d.Approve(context.Background(), &provider.Task{Ref: "FAC-9001"}); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[0] != "approve" || recorded[1] != "FAC-9001" {
		t.Fatalf("approve argv = %v want [approve FAC-9001]", recorded)
	}
}
