package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/laneenv"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/coordinator"
	"github.com/Kampe/Herdforge/pkg/herdr"
)

func TestParseMaxLanes(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
		auto bool
	}{{"3", 3, false}, {" auto ", 0, true}, {"0", 0, false}} {
		got, auto, err := parseMaxLanes(tc.raw)
		if err != nil || got != tc.want || auto != tc.auto {
			t.Errorf("parseMaxLanes(%q) = (%d, %v, %v)", tc.raw, got, auto, err)
		}
	}
	for _, raw := range []string{"", "-1", "three"} {
		if _, _, err := parseMaxLanes(raw); err == nil {
			t.Errorf("parseMaxLanes(%q) accepted invalid input", raw)
		}
	}
}

func TestCoordinatorControlBindingSurvivesCompletedCoordinatorTab(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Register(root, "coordinator", "wB"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.BindTab(root, "wB", "wB:t353", "wB:p353", "term-coordinator"); err != nil {
		t.Fatal(err)
	}

	binding, err := coordinatorControlBinding(root, "wB")
	if err != nil {
		t.Fatal(err)
	}
	if !binding.ControlSeat || binding.TabID != "wB:t353" || binding.PaneID != "wB:p353" || binding.TerminalID != "term-coordinator" || binding.Generation != "" {
		t.Fatalf("binding=%+v, want control-plane provenance without task generation", binding)
	}
}

// The CLI tests exec the built binary ~70 times. Linking it once keeps the
// package inside the suite's timeout; every caller only ever runs it, so a
// shared artifact is equivalent to a per-test one.
var (
	herdBinaryOnce sync.Once
	herdBinary     string
	herdBinaryErr  error
	herdBinaryOut  []byte
)

func TestMain(m *testing.M) {
	// Reviewer panes carry HERD_ROLE=agent as launch metadata. The CLI suite
	// creates coordinator-signed fixtures in-process and launches child CLIs;
	// keep that pane marker out of both test contexts while leaving the
	// production signer-boundary check unchanged.
	// FAC-610: HERD_ROLE was the first of these to bite, but it is one member
	// of a class. A lane also inherits HERD_ROOT, HERD_PROJECT_ROOT,
	// HERD_WORKSPACE and HERDR_* pane vars, which made five cmd/herd tests fail
	// in a lane's shell and pass in the coordinator's on the same commit.
	laneenv.Strip()

	root, rootGitState, guardErr := snapshotRootGitState()
	if guardErr != nil {
		// Tests may be run from a non-repository source checkout. In that case
		// there is no live root checkout whose configuration can be corrupted.
		root = ""
	}
	code := m.Run()
	if root != "" {
		if after, err := readRootGitState(root); err != nil {
			fmt.Fprintf(os.Stderr, "root git configuration guard failed: %v\n", err)
			code = 1
		} else if after != rootGitState {
			fmt.Fprintln(os.Stderr, "root git configuration guard failed: tests changed the root checkout's git config or remotes")
			code = 1
		}
	}
	if dir := filepath.Dir(herdBinary); herdBinary != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

func snapshotRootGitState() (string, string, error) {
	root, err := filepath.Abs(".")
	if err != nil {
		return "", "", err
	}
	if _, err := readRootGitState(root); err != nil {
		return "", "", err
	}
	state, err := readRootGitState(root)
	return root, state, err
}

func readRootGitState(root string) (string, error) {
	config := exec.Command("git", "-C", root, "config", "--local", "--null", "--list")
	configOut, err := config.Output()
	if err != nil {
		return "", err
	}
	remotes := exec.Command("git", "-C", root, "remote", "-v")
	remoteOut, err := remotes.Output()
	if err != nil {
		return "", err
	}
	return string(configOut) + "\x00---remotes---\x00" + string(remoteOut), nil
}

func TestForgeDriverBlocksCapacityWhenReconciliationUnavailable(t *testing.T) {
	d := &cliForgeDriver{maxLanes: 3}
	if err := d.ObserveReconciliation(context.Background()); err == nil {
		t.Fatal("missing observer must block")
	}
	state, err := d.LaneState(context.Background())
	if err != nil {
		t.Fatalf("LaneState under blocked reconciliation: %v", err)
	}
	if state.Busy != 3 || state.Max != 3 {
		t.Fatalf("blocked reconciliation exposed capacity: %+v", state)
	}
}

func TestNewProductionForgeObserverBindsWorkspaceAndDurableRecorder(t *testing.T) {
	observer, err := newProductionForgeObserver(&config.Config{Fleet: config.FleetConfig{HerdrWorkspace: "wK"}})
	if err != nil {
		t.Fatal(err)
	}
	if observer.Workspace != "wK" {
		t.Fatalf("workspace = %q, want wK", observer.Workspace)
	}
	if _, ok := observer.Reader.(herdr.SocketAuthorityReader); !ok {
		t.Fatalf("reader = %T, want SocketAuthorityReader", observer.Reader)
	}
	if observer.Record == nil {
		t.Fatal("production observer has no JSONL recorder")
	}
}

func TestNewProductionForgeObserverUsesAuthoritativeWorkspaceFallback(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "wK")
	observer, err := newProductionForgeObserver(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if observer.Workspace != "wK" {
		t.Fatalf("workspace = %q, want wK", observer.Workspace)
	}
}

func TestDeriveCoordinatorControlBindingFromLiveWKTabShape(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	binding, err := deriveCoordinatorControlBinding(root, "wK", []herdr.AgentEntry{
		{Kind: "codex", TabID: "wK:t2", PaneID: "wK:p2", TerminalID: "term_65903f1bf062c1f", Workspace: "wK", Cwd: root, ForegroundCwd: root, Status: "working"},
		{Kind: "codex", Name: "task-fac-304", TabID: "wK:t60", PaneID: "wK:p60", TerminalID: "term_task", Workspace: "wK", Cwd: root + "/.herd/worktrees/fac-304", ForegroundCwd: root + "/.herd/worktrees/fac-304", Status: "working"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.TabID != "wK:t2" || binding.PaneID != "wK:p2" || binding.TerminalID == "" || !binding.ControlSeat || binding.Role != "coordinator" {
		t.Fatalf("binding=%+v, want exact coordinator control incarnation", binding)
	}
}

func TestVerifiedTaskHeadDerivesEmptyCandidateSHAFromCleanTempGit(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "test"}, {"config", "commit.gpgSign", "false"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "TASK-CONTEXT.json"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "TASK-CONTEXT.json").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "commit", "-qm", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifiedTaskHead(root)
	if err != nil || got != strings.TrimSpace(string(head)) {
		t.Fatalf("verifiedTaskHead=%q err=%v, want %q", got, err, strings.TrimSpace(string(head)))
	}
	if err := os.WriteFile(filepath.Join(root, "dirty"), []byte("no fallback"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedTaskHead(root); err == nil {
		t.Fatal("dirty task worktree must fail closed")
	}
}

func TestVerifiedTaskCandidateChecksSignedHEADWithoutRejectingUntrackedFiles(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "test"}, {"config", "commit.gpgSign", "false"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "commit", "-qm", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}
	head, err := taskWorktreeHead(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("untracked"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := verifiedTaskCandidate(root, head); err != nil || got != head {
		t.Fatalf("verifiedTaskCandidate=%q err=%v, want signed HEAD %q despite untracked files", got, err, head)
	}
	if _, err := verifiedTaskCandidate(root, "0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("signed candidate mismatch must fail closed")
	}
}

func buildHerd(t *testing.T) string {
	t.Helper()
	herdBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "herd-cli-bin")
		if err != nil {
			herdBinaryErr = err
			return
		}
		binary := filepath.Join(dir, "herd")
		revision, err := exec.Command("git", "rev-parse", "HEAD").Output()
		if err != nil {
			herdBinaryErr = err
			return
		}
		herdBinaryOut, herdBinaryErr = exec.Command("go", "build", "-buildvcs=false", "-ldflags", "-X github.com/Kampe/Herdforge/pkg/provenance.BinaryRevision="+strings.TrimSpace(string(revision)), "-o", binary, ".").CombinedOutput()
		if herdBinaryErr == nil {
			herdBinary = binary
		}
	})
	if herdBinaryErr != nil {
		t.Fatalf("build failed: %v, output: %s", herdBinaryErr, herdBinaryOut)
	}
	return herdBinary
}

func TestVersionFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("herd --version failed: %v", err)
	}
	revision, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if !strings.Contains(string(out), "herd version") || !strings.Contains(string(out), "revision "+strings.TrimSpace(string(revision))) {
		t.Errorf("expected version output, got %s", string(out))
	}
}

func TestHelpFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("herd --help failed: %v", err)
	}
	if !strings.Contains(string(out), "Herdforge") {
		t.Errorf("expected help output, got %s", string(out))
	}
}

func TestVFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "-v").CombinedOutput()
	if err != nil {
		t.Fatalf("herd -v failed: %v", err)
	}
	if !strings.Contains(string(out), "herd version") {
		t.Errorf("expected version output, got %s", string(out))
	}
}

func TestHFlag(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "-h").CombinedOutput()
	if err != nil {
		t.Fatalf("herd -h failed: %v", err)
	}
	if !strings.Contains(string(out), "Herdforge") {
		t.Errorf("expected help output, got %s", string(out))
	}
}

func TestUnknownCommand(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "nonexistent")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit error for unknown command")
	}
	if !strings.Contains(string(out), "unknown subcommand") {
		t.Errorf("expected unknown subcommand message, got %s", string(out))
	}
}

func TestInit(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	cmd := exec.Command(binary, "init")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd init failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "Scaffolded") {
		t.Errorf("expected scaffold message, got %s", string(out))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".herd", "herd.yaml")); os.IsNotExist(err) {
		t.Error(".herd/herd.yaml should exist")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".herd", "winddown.json")); os.IsNotExist(err) {
		t.Error(".herd/winddown.json should exist")
	}
}

func TestInitTwice(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	// First init
	initCmd := exec.Command(binary, "init")
	initCmd.Dir = tmpDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("first init failed: %v, output: %s", err, out)
	}
	// Second init in same dir
	cmd := exec.Command(binary, "init")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("second init failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "already exists") {
		t.Errorf("expected 'already exists' message, got %s", string(out))
	}
}

func TestStatusUninitialized(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	cmd := exec.Command(binary, "status")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status in uninitialized dir failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "Uninitialized") {
		t.Errorf("expected Uninitialized, got %s", string(out))
	}
}

func TestStatusInitialized(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	initCmd := exec.Command(binary, "init")
	initCmd.Dir = tmpDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init failed: %v, output: %s", err, out)
	}
	cmd := exec.Command(binary, "status")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "Active") {
		t.Errorf("expected Active, got %s", string(out))
	}
}

func TestFreshClonePreflightAndRuntimeMigrationLeaveTrackedStateClean(t *testing.T) {
	binary := buildHerd(t)
	repoRootCmd := exec.Command("git", "rev-parse", "--show-toplevel")
	repoRootCmd.Dir = "."
	repoRootOut, err := repoRootCmd.Output()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	repoRoot := strings.TrimSpace(string(repoRootOut))
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(filepath.Join(seed, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".gitignore", ".herd/herd.yaml"} {
		contents, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(seed, rel), contents, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := testgit.Command(dir, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git(seed, "init", "--quiet")
	git(seed, "config", "user.email", "runtime-hygiene@test.invalid")
	git(seed, "config", "user.name", "runtime hygiene")
	git(seed, "add", ".")
	git(seed, "commit", "--quiet", "-m", "test fixture")

	clone := filepath.Join(t.TempDir(), "clone")
	git(".", "clone", "--quiet", "--no-local", seed, clone)
	clean := func(stage string) {
		t.Helper()
		cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
		cmd.Dir = clone
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s status: %v: %s", stage, err, output)
		}
		if string(output) != "" {
			t.Fatalf("%s dirtied tracked runtime state: %s", stage, output)
		}
	}
	preflightStatic := exec.Command(binary, "preflight-static")
	preflightStatic.Dir = clone
	if output, err := preflightStatic.CombinedOutput(); err != nil {
		t.Fatalf("fresh clone preflight-static: %v: %s", err, output)
	}
	clean("preflight-static")

	status := exec.Command(binary, "status")
	status.Dir = clone
	if output, err := status.CombinedOutput(); err != nil {
		t.Fatalf("runtime migration: %v: %s", err, output)
	}
	clean("runtime migration")
}

func TestPreflightStaticAndOperationalReadinessGates(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	// 1. Static preflight succeeds without any fleet attestation or environment variables.
	staticCmd := exec.Command(binary, "preflight-static")
	staticCmd.Dir = tmpDir
	staticCmd.Env = append(os.Environ(), "HERD_CONTROL_SECRET=", "HERD_LIVE_HARNESS_PROOF=", "HERD_REFRESH_READINESS=")
	staticOut, err := staticCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preflight-static must succeed without attestation; got error: %v: %s", err, staticOut)
	}
	if !strings.Contains(string(staticOut), "Preflight boundary check passed.") {
		t.Fatalf("expected preflight boundary check pass output, got: %s", staticOut)
	}

	for _, tc := range []struct {
		name       string
		mode       string
		secret     string
		wantErr    bool
		wantOutput string
	}{
		{name: "local skips hosted readiness", mode: "local", wantOutput: "Preflight merge-policy check passed."},
		{name: "production requires readiness", mode: "production", wantErr: true, wantOutput: "FAC-133 readiness: BLOCKED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readinessRoot := filepath.Join(tmpDir, tc.name, ".herd")
			cmd := exec.Command(binary, "preflight")
			cmd.Dir = tmpDir
			cmd.Env = append(os.Environ(),
				"HERD_MODE="+tc.mode,
				"HERD_CONTROL_SECRET="+tc.secret,
				"HERD_READINESS_ROOT="+readinessRoot,
				"HERD_LIVE_HARNESS_PROOF=",
				"HERD_REFRESH_READINESS=",
			)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("preflight error=%v, wantErr=%v: %s", err, tc.wantErr, out)
			}
			if !strings.Contains(string(out), tc.wantOutput) {
				t.Fatalf("expected output %q, got: %s", tc.wantOutput, out)
			}
			if tc.mode != "production" && strings.Contains(string(out), "FAC-133 readiness:") {
				t.Fatalf("local preflight must not run hosted readiness: %s", out)
			}
			if _, statErr := os.Stat(readinessRoot); !os.IsNotExist(statErr) {
				t.Fatalf("local preflight must not create readiness state, stat error=%v", statErr)
			}
		})
	}
}

func TestStatusEvidenceFailureExitsNonZero(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	initCmd := exec.Command(binary, "init")
	initCmd.Dir = tmpDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init failed: %v, output: %s", err, out)
	}
	if err := os.Mkdir(filepath.Join(tmpDir, ".herd", "herdforge.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "status")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Dependency evidence") {
		t.Fatalf("status evidence failure must exit nonzero: err=%v output=%s", err, out)
	}
}

func TestNoArgs(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary).CombinedOutput()
	if err != nil {
		t.Fatalf("herd with no args failed: %v", err)
	}
	if !strings.Contains(string(out), "Herdforge") {
		t.Errorf("expected help output on no args, got %s", string(out))
	}
}

func TestInitFull(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	cmd := exec.Command(binary, "init", "--full")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd init --full failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "3-lane forge") {
		t.Errorf("expected 3-lane forge message, got %s", string(out))
	}
	// Verify all three prompts were created
	for _, prompt := range []string{".herd/prompts/smith.md", ".herd/prompts/worker.md", ".herd/prompts/reviewer.md"} {
		if _, err := os.Stat(filepath.Join(tmpDir, prompt)); os.IsNotExist(err) {
			t.Errorf("%s should exist", prompt)
		}
	}
	// Verify config has all three lanes
	cfgData, err := os.ReadFile(filepath.Join(tmpDir, ".herd", "herd.yaml"))
	if err != nil {
		t.Fatal("herd.yaml should exist after init --full")
	}
	cfgStr := string(cfgData)
	for _, lane := range []string{"forge-smith", "worker", "reviewer"} {
		if !strings.Contains(cfgStr, lane) {
			t.Errorf("config should contain lane %s", lane)
		}
	}
}

func TestInitFullPromptsUseScopedTestCadence(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	cmd := exec.Command(binary, "init", "--full")
	cmd.Dir = tmpDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init --full failed: %v, output: %s", err, out)
	}

	want := []string{
		"go test ./<changed-package>/... -run <TestName>",
		"herd verify",
		"TestFactoryE2E_CoordinatorFenceBlocksSecondLoop",
		"TestApproveCLI_ReleasedNewerGenerationStillFences",
		"TestBroker_SessionAuthorityDiesWithPaneIncarnation",
		"TestLaneLaunchDecisionReportsConfiguredProbeFailure",
		"TestNewDrainAdaptersFailsClosedOnMissingAuthority/no_reviewer_lane",
	}
	for _, prompt := range []string{".herd/prompts/worker.md", ".herd/prompts/smith.md"} {
		t.Run(prompt, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(tmpDir, prompt))
			if err != nil {
				t.Fatal(err)
			}
			content := string(data)
			for _, marker := range want {
				if !strings.Contains(content, marker) {
					t.Errorf("fresh %s is missing scoped-test marker %q", prompt, marker)
				}
			}
			if strings.Contains(content, "Run 'go test ./...' before signalling completion") {
				t.Errorf("fresh %s still mandates full tests during iteration", prompt)
			}
		})
	}
}

func TestInitFullTwice(t *testing.T) {
	binary := buildHerd(t)
	tmpDir := t.TempDir()
	// First init --full
	initCmd := exec.Command(binary, "init", "--full")
	initCmd.Dir = tmpDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("first init --full failed: %v, output: %s", err, out)
	}
	// Modify one prompt to test non-overwrite
	os.WriteFile(filepath.Join(tmpDir, ".herd", "prompts", "worker.md"), []byte("custom content"), 0644)
	// Second init --full
	cmd := exec.Command(binary, "init", "--full")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("second init --full failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "3-lane forge") {
		t.Errorf("expected 3-lane forge message on second run, got %s", string(out))
	}
	// Verify existing prompt was NOT overwritten
	data, err := os.ReadFile(filepath.Join(tmpDir, ".herd", "prompts", "worker.md"))
	if err != nil {
		t.Fatal("worker.md should exist")
	}
	if string(data) != "custom content" {
		t.Errorf("existing prompt should not have been overwritten, got %s", string(data))
	}
}

func TestCloneUsage(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "clone")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected error for clone without args")
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Errorf("expected usage message, got %s", string(out))
	}
}

func TestCloneHelpInUsage(t *testing.T) {
	binary := buildHerd(t)
	// Verify clone appears in help output
	out, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("herd --help failed: %v", err)
	}
	if !strings.Contains(string(out), "clone") {
		t.Errorf("help should list clone command, got %s", string(out))
	}
}
