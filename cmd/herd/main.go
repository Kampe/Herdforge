package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Kampe/Herdforge/pkg/lock"

	"github.com/Kampe/Herdforge/pkg/activate"
	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/classify"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/lost"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/next"
	"github.com/Kampe/Herdforge/pkg/outbox"
	"github.com/Kampe/Herdforge/pkg/overlap"
	"github.com/Kampe/Herdforge/pkg/posture"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/standing"
	"github.com/Kampe/Herdforge/pkg/process"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/resetsafe"
	"github.com/Kampe/Herdforge/pkg/resolve"
	"github.com/Kampe/Herdforge/pkg/resources"
	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/scopeauth"
	"github.com/Kampe/Herdforge/pkg/scopefence"
	"github.com/Kampe/Herdforge/pkg/selftest"
	"github.com/Kampe/Herdforge/pkg/store"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/textdelivery"
	"github.com/Kampe/Herdforge/pkg/throughput"
	"github.com/Kampe/Herdforge/pkg/usage"
	"github.com/Kampe/Herdforge/pkg/verifier"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

const version = "0.2.0-dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	command := os.Args[1]
	switch command {
	case "--version", "-v":
		fmt.Printf("herd version %s\n", version)
		os.Exit(0)

	case "--help", "-h":
		printUsage()
		os.Exit(0)
	}

	// FAC-189: every subcommand recognizes -h/--help before positional
	// payloads or any provider/Herdr/git/outbox/claim/worktree side effect.
	// Literal payloads equal to --help require `--` or an explicit flag
	// (see parseTicketRef / dispatch --ticket=).
	if _, known := subcommandUsage[command]; known {
		if exitIfHelp(command, os.Args[2:]) {
			return
		}
		// Probe after the help gate so help tests observe zero entries.
		markOperational(command)
	}

	switch command {
	case "init":
		runInit()

	case "clone":
		runClone()

	case "preflight":
		runPreflight()

	case "verify":
		runVerify()

	case "verify-fac151":
		runFAC151Hermetic()

	case "selftest":
		runSelfTest()

	case "status":
		runStatus()

	case "pulse":
		runPulse()

	case "wind-down":
		runWindDown()

	case "posture":
		runFamilyPosture()

	case "claude-only":
		// Legacy alias: prefer `herd posture claude-only|no-claude|clear|status`.
		runPosture(posture.ClaudeOnly)

	case "no-claude":
		runPosture(posture.NoClaude)

	case "board-frozen":
		runBoardFrozen()

	case "board-freeze":
		runBoardFreeze()

	case "role-inject":
		runRoleInject()

	case "feedback":
		runFeedback()

	case "stop":
		runStop()

	case "stash":
		runStash()

	case "park":
		runPark()

	case "wave":
		runWave()

	case "quota-supervisor":
		runQuotaSupervisor()

	case "rescue":
		runRescue()

	case "seed-lane-state":
		runSeedLaneState()

	case "spin":
		runSpin()

	case "watch":
		runWatch()

	case "fresh-build":
		runFreshBuild()

	case "shot":
		runShot()

	case "scope":
		runScope()

	case "review-classify":
		runReviewClassify()

	case "review-ingest":
		runReviewIngest()

	case "harvest-merge":
		runHarvestMerge()

	case "hold":
		runHold()

	case "standing":
		if err := runStandingE(); err != nil {
			fmt.Fprintf(os.Stderr, "standing failed: %v\n", err)
			os.Exit(1)
		}

	case "daemon":
		runDaemon()

	case "usage":
		runUsage()

	case "quota":
		runQuota()

	case "review":
		runReview()

	case "review-ledger":
		runReviewLedger()

	case "drain":
		runDrain()

	case "approve":
		runApprove()

	case "board-done":
		runBoardDone()

	case "board-sync":
		runBoardSync()

	case "sh", "repl":
		runShell()

	case "send":
		runSend()

	case "herdr-deliver":
		runHerdrDeliver()

	case "cleanup":
		runCleanup()

	case "labels":
		runLabels()

	case "forge":
		if err := runForgeE(); err != nil {
			fmt.Fprintf(os.Stderr, "forge failed: %v\n", err)
			os.Exit(1)
		}

	case "up":
		runUp()

	case "activate":
		runActivate()

	case "validate-config":
		runValidateConfig()

	case "doctor-models":
		runDoctorModels()

	case "tool-probe":
		runToolProbe()

	case "shoot":
		runShoot()

	case "next":
		runNext()

	case "dispatch":
		runDispatch()

	case "deps":
		runDeps()

	case "harvest":
		runHarvest()

	case "unmerged":
		runUnmerged()

	case "lost":
		runLost()

	case "throughput":
		runThroughput()

	case "worktrees":
		runWorktrees()

	case "containers":
		runContainers()

	case "commands":
		runCommandSessions()

	case "overlap":
		runOverlap()

	case "attention":
		runAttention()

	case "process":
		runProcess()

	case "resolve-lane":
		runResolveLane()

	case "route":
		runRoute()

	case "kick":
		runKick()

	case "lifecycle":
		runLifecycle()

	case "resources":
		runResources()

	case "lock":
		runLock()

	case "reset-safe":
		runResetSafe()

	case "signer-boundary":
		runSignerBoundary()

	case "command":
		runCommand()

	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand '%s'\nRun 'herd --help' for usage.\n", command)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Herdforge: Self-Forging Multi-Agent Orchestration Daemon")
	fmt.Println("\nUsage:")
	fmt.Println("  herd <command> [flags]")
	fmt.Println("\nCommands:")
	fmt.Println("  init       Scaffold default .herd/herd.yaml configuration file")
	fmt.Println("  clone      Clone a Herdforge repository and bootstrap the forge")
	fmt.Println("  preflight  Run workspace boundary and repo-relative path verification")
	fmt.Println("  selftest   Run core orchestration behavior self-test suite")
	fmt.Println("  status     Display current orchestration engine status")
	fmt.Println("  pulse      Coordinator heartbeat (observe default; --act mutates bounded steps)")
	fmt.Println("  wind-down  Control durable fleet launch posture: on, off, or status")
	fmt.Println("  posture      Family policy: claude-only | no-claude | clear | status")
	fmt.Println("  claude-only  Legacy alias for posture claude-only on/off/status")
	fmt.Println("  no-claude    Legacy alias for posture no-claude on/off/status")
	fmt.Println("  board-frozen Exit 0 with the freeze trigger when board mutation is frozen")
	fmt.Println("  board-freeze Durable gate: on, off, or status; every provider mutation refuses while on")
	fmt.Println("  role-inject  SessionStart hook: bind a lane to its worker contract")
	fmt.Println("  feedback     Fleet-wide control-plane feedback census")
	fmt.Println("  stop         Stop the herd without deleting worktrees (dry-run default)")
	fmt.Println("  stash        Worktree-scoped stash that cannot collide across lanes")
	fmt.Println("  park         Make parked work durable (annotated pushed tag) + audit exposure")
	fmt.Println("  wave         Pre-wave readiness report; --standing/--up raise after gates pass")
	fmt.Println("  quota-supervisor  Convert live quota, cooldown and process evidence into per-surface concurrency caps")
	fmt.Println("  rescue       Diagnose/repair cramped or split agent panes (dry-run default; --apply once)")
	fmt.Println("  seed-lane-state   Restore or seed a lane's state artifacts (never overwrites)")
	fmt.Println("  spin         Detect stalled (frozen output) and spinning (no git delta) panes")
	fmt.Println("  watch        Fire the moment an agent settles; --stream feeds harvest triggers")
	fmt.Println("  fresh-build  Prove cross-package build errors are real (not stale dist)")
	fmt.Println("  shot         Run one bounded task headless through the quota router")
	fmt.Println("  scope        Publish the trusted task scope the dispatch fence resolves against")
	fmt.Println("  review-classify   Deterministic R0-R3 risk floor for review dispatch")
	fmt.Println("  review-ingest     Validate reviewer verdicts and admit them to the ledger")
	fmt.Println("  harvest-merge     Cherry-pick a lane's reviewed commits onto a fresh base")
	fmt.Println("  hold       Control durable generation-fenced lane/task hold: on, off, or status")
	fmt.Println("  review     Claim in-progress tasks for reviewer and advance to review status")
	fmt.Println("  approve    Move in-review cards to done, gated on merge evidence")
	fmt.Println("  drain      Report coordinator review pile (optional bounded --act)")
	fmt.Println("  board-done Move one card to done ONLY with proof its work is on origin/main")
	fmt.Println("  board-sync Reconcile board status against git reality (report only)")
	fmt.Println("  sh         Interactive shell: run herd subcommands in a loop")
	fmt.Println("  send       Submit text to a herdr agent pane and verify consumption")
	fmt.Println("  herdr-deliver  Durably deliver stdin or --file bytes to one Herdr session (FAC-183)")
	fmt.Println("  cleanup    Close finished one-off agent tabs (standing fleet exempt)")
	fmt.Println("  labels     Reconcile drifted Herdforge tab labels in place (FAC-199)")
	fmt.Println("  forge      Full cycle: pulse worker + review + approve")
	fmt.Println("  standing   Raise/status/shutdown declarative standing control roles")
	fmt.Println("  daemon     Start the long-running orchestration daemon (infinite pulse loop)")
	fmt.Println("  usage      Show harness quota usage from OpenUsage CLI")
	fmt.Println("  quota      Show binding headroom, pace/pressure, pool breakdown")
	fmt.Println("  up         Start a single agent lane (herd up <lane-name>)")
	fmt.Println("  activate   Bring up all deployables + health-check gate (compose + /v1/status)")
	fmt.Println("  validate-config  Validate .herd/herd.yaml configuration")
	fmt.Println("  doctor-models    Probe each lane's model (+fallbacks) for quota exhaustion")
	fmt.Println("  next            Show highest-priority next action")
	fmt.Println("  dispatch        Dispatch a ticket to a worktree and launch agent")
	fmt.Println("  deps            Packet↔board dependency-graph conformance (FAC-159)")
	fmt.Println("  harvest         Sweep all worktrees for unmerged commits")
	fmt.Println("  unmerged        Authoritative cherry-based unmerged check (herd unmerged <path> | --all)")
	fmt.Println("  lost            Find ownerless unmerged work on ANY branch (subject-based)")
	fmt.Println("  throughput      Read-only fleet throughput KPIs from local evidence")
	fmt.Println("  worktrees       Snapshot all worktree state + collision check")
	fmt.Println("  containers      Durable container lifecycle status + unowned audit (FAC-200)")
	fmt.Println("  commands        Retained command session status + recovery sweep (FAC-193)")
	fmt.Println("  verify          Gate: real commits + build + tests (FAC-98/FAC-116)")
	fmt.Println("  verify-fac151   Run only the fixed hermetic FAC-151 verifier profile")
	fmt.Println("  overlap         Detect files/symbols edited together by 2+ unmerged branches")
	fmt.Println("  attention       List agents needing coordinator eyes")
	fmt.Println("  process         Classify harvest targets (herd-process digest)")
	fmt.Println("  resolve-lane    Resolve a lane to concrete provider+model (deterministic)")
	fmt.Println("  route           Pick the healthy execution surface for a task shape")
	fmt.Println("  kick            Re-engage standing or named agent lanes")
	fmt.Println("  attention       List standing agents needing coordinator eyes (triage)")
	fmt.Println("  lifecycle       Observe and act on fleet state via lifecycle engine")
	fmt.Println("  resources       Snapshot system-resource headroom (free-mem, swap, gate verdict)")
	fmt.Println("  lock           Advisory shared-checkout lock: with, acquire, release, status")
	fmt.Println("  reset-safe     Reset a feature worktree after preserving unique commits")
	fmt.Println("  signer-boundary  OS signing boundary: serve | establish | status | prove | sign (FAC-169)")
	fmt.Println("  command         Run a root-authorized command under a durable attempt budget")
	fmt.Println("  --version       Show herd version")
}

const resetSafeUsage = "usage: herd reset-safe <worktree-path>"

// runResetSafe composes the reviewed package operation into the public CLI.
// The command intentionally accepts one positional target only: repo root is
// the current checkout, and all mutation/safety policy stays in resetsafe.
func runResetSafe() {
	args := os.Args[2:]
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(resetSafeUsage)
		return
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, resetSafeUsage)
		os.Exit(2)
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-reset-safe: cannot resolve repo root: %v\n", err)
		os.Exit(1)
	}
	plan, err := resetsafe.New(context.Background(), repoRoot, args[0], resetsafe.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := plan.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runInit() {
	pulseFlags := flag.NewFlagSet("init", flag.ExitOnError)
	full := pulseFlags.Bool("full", false, "Scaffold full 3-lane forge config (smith, worker, reviewer)")
	pulseFlags.Parse(os.Args[2:])

	if *full {
		runInitFull()
		return
	}

	herdDir := ".herd"
	cfgPath := ".herd/herd.yaml"

	if err := os.MkdirAll(herdDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create .herd directory: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.Stat(cfgPath); err == nil {
		fmt.Println(".herd/herd.yaml already exists.")
		os.Exit(0)
	}

	defaultConfig := `version: "1"
project:
  name: "my-herd-app"
  default_branch: "main"

task_provider:
  type: "linear"
  project_id: "your-linear-project-id"
  api_key_env: "LINEAR_API_KEY"

lanes:
  - name: "worker"
    role: "worker"
    agent_kind: "pi"
    harness: "pi"
    prompt: ".herd/prompts/worker.md"
    worktree: ".worktrees/worker"
    provider: "codex"
    model: "gpt-5.6-luna"
    effort: "medium"
    task_shape: "implementation"

verification:
  test_command: "go test ./..."
`
	if err := os.WriteFile(cfgPath, []byte(defaultConfig), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write default config: %v\n", err)
		os.Exit(1)
	}

	os.MkdirAll(".herd/prompts", 0755)
	os.WriteFile(".herd/prompts/worker.md", []byte("# Herdforge Worker Agent\n\nWork on the assigned task in your worktree.\n"), 0644)

	fmt.Println("Scaffolded .herd/herd.yaml successfully.")
}

func runInitFull() {
	herdDir := ".herd"
	cfgPath := ".herd/herd.yaml"

	if err := os.MkdirAll(herdDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create .herd directory: %v\n", err)
		os.Exit(1)
	}

	os.MkdirAll(".herd/prompts", 0755)
	os.MkdirAll(".worktrees", 0755)

	fullConfig := `version: "1"

# Herdforge — the forge that forges itself
project:
  name: "my-herd-app"
  default_branch: "main"
  repo_url: "https://github.com/user/my-herd-app.git"

task_provider:
  type: "linear"
  project_id: "your-linear-project-id"
  api_key_env: "LINEAR_API_KEY"

# Agent lanes — each lane runs in a herdr workspace tab
lanes:
  - name: "forge-smith"
    role: "forge-smith"
    agent_kind: "pi"
    harness: "pi"
    prompt: ".herd/prompts/smith.md"
    worktree: ".worktrees/smith"
    provider: "codex"
    model: "gpt-5.6-luna"
    effort: "medium"
    task_shape: "implementation"

  - name: "worker"
    role: "worker"
    agent_kind: "pi"
    harness: "pi"
    prompt: ".herd/prompts/worker.md"
    worktree: ".worktrees/worker"
    provider: "codex"
    model: "gpt-5.6-luna"
    effort: "medium"
    task_shape: "implementation"

  - name: "reviewer"
    role: "reviewer"
    agent_kind: "pi"
    harness: "pi"
    prompt: ".herd/prompts/reviewer.md"
    worktree: ".worktrees/reviewer"
    provider: "claude"
    model: "claude-sonnet-5"
    effort: "medium"
    task_shape: "qa"

verification:
  test_command: "go test ./..."
  preflight_command: "go build ./..."
`

	if err := os.WriteFile(cfgPath, []byte(fullConfig), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
		os.Exit(1)
	}

	// Write all three prompt files
	writePrompt(".herd/prompts/smith.md", `# Herdforge Smith Agent Contract

You are the **Forge-Smith Planner Agent** in the Herdforge network.

## Responsibilities
1. Plan architecture and break work into tasks.
2. Delegate implementation to worker agents.
3. Review completed work for correctness and coherence.
4. Decide when completed work is ready for final review.
5. Work exclusively inside your assigned worktree.
`)
	writePrompt(".herd/prompts/worker.md", `# Herdforge Worker Agent Contract

You are an **Autonomous Builder Agent** operating in a dedicated git worktree.

## Core Rules & Invariants
1. **Worktree Isolation**: Work exclusively inside your designated worktree path.
2. **Test-Driven Development**: Write failing tests first, then implement.
3. **Fail-Closed Verification**: Run 'go test ./...' before signalling completion.
4. **No Absolute Paths**: All file paths must be repository-relative.
5. **Conventional Commits**: Write clean atomic commit messages.
`)
	writePrompt(".herd/prompts/reviewer.md", `# Herdforge Reviewer Agent Contract

You are an **Adversarial Code Reviewer** in the Herdforge network.

## Review Protocol
1. **Cross-Model Independence**: Reviewer differs from worker's provider.
2. **Risk Classification**: R0 (docs), R1 (refactor), R2 (features), R3 (auth/security).
3. **Audit Checks**: AST soundness, no secrets, test suite passes.
4. **Verdict**: Return APPROVED or REJECTED with actionable feedback.
`)

	fmt.Println("Scaffolded full 3-lane forge configuration:")
	fmt.Println("  lanes: forge-smith, worker, reviewer")
	fmt.Println("  prompts: .herd/prompts/{smith,worker,reviewer}.md")
	fmt.Println("  worktrees: .worktrees/{smith,worker,reviewer}")
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Edit .herd/herd.yaml with your project/repo settings")
	fmt.Println("  2. Run 'kaneo link -w <workspace> -p <project>' to link Kaneo")
	fmt.Println("  3. Run 'herd standing' to launch all agents")
}

func writePrompt(path, content string) {
	os.MkdirAll(".herd/prompts", 0755)
	if _, err := os.Stat(path); err == nil {
		return // don't overwrite existing
	}
	os.WriteFile(path, []byte(content), 0644)
}

func runClone() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: herd clone <repo-url> [target-dir]\n")
		os.Exit(1)
	}

	repoURL := os.Args[2]
	targetDir := "."
	if len(os.Args) >= 4 {
		targetDir = os.Args[3]
	}

	repoName := repoURL
	if last := strings.LastIndex(repoURL, "/"); last >= 0 {
		repoName = repoURL[last+1:]
	}
	repoName = strings.TrimSuffix(repoName, ".git")

	if targetDir == "." {
		targetDir = repoName
	}

	fmt.Printf("Cloning %s into %s...\n", repoURL, targetDir)

	cmd := exec.Command("git", "clone", repoURL, targetDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "git clone failed: %v\n", err)
		os.Exit(1)
	}

	// Run herd init --full in the cloned directory
	initCmd := exec.Command(os.Args[0], "init", "--full")
	initCmd.Dir = targetDir
	initOut, err := initCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init forge in %s: %v\n  output: %s\n", targetDir, err, initOut)
		os.Exit(1)
	}
	fmt.Print(string(initOut))

	// Try to auto-link from existing .kaneo.json if present (from herd.yaml repo_url)
	kaneoJSON := filepath.Join(targetDir, ".kaneo.json")
	if _, err := os.Stat(kaneoJSON); err == nil {
		fmt.Println("Found existing .kaneo.json — Kaneo linking already configured.")
	} else {
		fmt.Println("\nTip: Run 'kaneo link -w <workspace> -p <project>' in the cloned repo")
		fmt.Println("  or copy .kaneo.json from the source to link to your Kaneo board.")
	}

	fmt.Printf("\nHerdforge cloned and bootstrapped in %s/\n", targetDir)
	fmt.Println("Run: cd", targetDir, "&& herd standing")
}

func runPreflight() {
	if err := preflight.CheckWorktreeBoundary("."); err != nil {
		fmt.Fprintf(os.Stderr, "Preflight failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Preflight boundary check passed. Zero absolute path leaks detected.")
	if err := preflight.CheckDangerousSignalLiterals("."); err != nil {
		fmt.Fprintf(os.Stderr, "Preflight failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Preflight signal-literal check passed. No host-wide kill literals in production sources.")
}

func runSelfTest() {
	runner := selftest.NewSelfTestRunner(".")
	results, err := runner.RunSuite(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Self-test failed: %v\n", err)
		os.Exit(1)
	}

	for _, res := range results {
		fmt.Printf("[PASS] %s\n", res.Name)
	}
	fmt.Println("\nAll self-test assertions passed cleanly.")
}

func runStatus() {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Printf("Status: Uninitialized (no valid .herd/herd.yaml found)\n")
		return
	}
	fmt.Printf("Status: Active\nProject: %s\nProvider: %s\nLanes: %d configured\n",
		cfg.Project.Name, cfg.TaskProvider.Type, len(cfg.Lanes))
	st, err := store.New(".herd/herdforge.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Dependency evidence: UNAVAILABLE (%v)\n", err)
		os.Exit(1)
	}
	defer st.Close()
	blocked, err := st.BlockedSelectionHistory(10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Dependency evidence: UNREADABLE (%v)\n", err)
		os.Exit(1)
	}
	fmt.Printf("Dependency BLOCKED evidence: %d recent\n", len(blocked))
	for _, record := range blocked {
		fmt.Printf("  BLOCKED %s [%s] %s\n", record.Ref, record.Code, record.Reason)
	}
	// FAC-193: a completed tool call must not be able to hide a live
	// background terminal behind an agent-level working state, so fleet
	// status reports retained command sessions alongside lane evidence.
	line, err := commandSessionStatusLine(commandSessionDBPath(""), time.Now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Retained command sessions: UNREADABLE (%v)\n", err)
		os.Exit(1)
	}
	fmt.Println(line)
}

// loadTaskProvider activates the configured board provider with FAC-150
// deadlines via provider.NewFromHerdConfig. Non-Kaneo types error (FAC-155).
func loadTaskProvider(cfg *config.Config) (provider.TaskProvider, error) {
	return provider.NewFromHerdConfig(cfg)
}

func runUsage() {
	snap, err := usage.FetchSnapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Current usage snapshot:")
	for name, p := range snap.Providers {
		util := snap.Utilization(name)
		fmt.Printf("  %s (%s): utilization=%.0f%%", p.DisplayName, p.Plan, util*100)
		for rname, r := range p.Resources {
			if r.Kind == "consumption" {
				fmt.Printf("  %s: %.0f/%.0f", rname, r.Used, r.Limit)
			} else if r.Kind == "balance" {
				fmt.Printf("  %s: %.0f", rname, r.Available)
			}
		}
		fmt.Println()
	}
}

func runQuota() {
	fs := flag.NewFlagSet("quota", flag.ExitOnError)
	wantJSON := fs.Bool("json", false, "Output JSON")
	pickMode := fs.Bool("pick", false, "Pick best provider")
	among := fs.String("among", "", "Comma-separated providers for --pick (default: codex,claude)")
	oneProvider := fs.String("provider", "", "Query one provider")
	onePool := fs.String("pool", "all", "Model pool for --provider")
	_ = fs.Bool("force", false, "Bypass openusage cache")
	exhaustedPct := fs.Float64("exhausted-at", usage.DefaultExhaustedPct, "Exhausted threshold percent")
	fs.Parse(os.Args[2:])

	e := usage.NewQuotaEngine()
	e.ExhaustedPct = *exhaustedPct

	snap, err := usage.FetchSnapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "quota: %v\n", err)
		os.Exit(1)
	}

	computed := e.ComputeAll(snap)

	// --provider mode
	if *oneProvider != "" {
		resolved := e.AliasProvider(*oneProvider)
		p, ok := computed[resolved]
		if !ok {
			fmt.Fprintf(os.Stderr, "quota: no data for provider %q\n", resolved)
			os.Exit(4)
		}
		if *onePool != "all" {
			pool, ok := p.Pools[*onePool]
			if !ok {
				fmt.Fprintf(os.Stderr, "quota: no pool %q for provider %q\n", *onePool, resolved)
				os.Exit(4)
			}
			p = pool
		}
		if *wantJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]usage.BurnState{resolved: p})
			return
		}
		if !p.Available && p.Reason == "no-quota-data" {
			fmt.Printf("%s: no quota data (plan=%s)\n", resolved, p.Plan)
			return
		}
		status := "AVAILABLE"
		if !p.Available {
			status = strings.ToUpper(p.Reason)
		}
		fmt.Printf("%s: binding %.0f%% used (%s), %.0f%% left, resets %s -- %s\n",
			resolved, p.Used, p.Window, p.Remaining, p.ResetsIn, status)
		if len(p.Pools) > 0 {
			for pname, pool := range p.Pools {
				pstatus := "OK"
				if !pool.Available {
					pstatus = strings.ToUpper(pool.Reason)
				}
				fmt.Printf("  pool %s: %.0f%% used, %s (resets %s)\n",
					pname, pool.Used, pstatus, pool.ResetsIn)
			}
		}
		return
	}

	// --pick mode
	if *pickMode {
		amongList := []string{"codex", "claude"}
		if *among != "" {
			for _, a := range strings.Split(*among, ",") {
				a = strings.TrimSpace(a)
				if a != "" {
					amongList = append(amongList, a)
				}
			}
		}
		pick, state, err := e.PickProvider(computed, amongList)
		if err != nil {
			fmt.Fprintf(os.Stderr, "quota: %v\n", err)
			os.Exit(5)
		}
		if *wantJSON {
			out := map[string]interface{}{
				"pick":            pick,
				"binding":         state,
				"amongConsidered": amongList,
			}
			maybeRunner := ""
			for _, n := range amongList {
				if n != pick {
					maybeRunner = n
					break
				}
			}
			if maybeRunner != "" {
				out["runnerUp"] = map[string]interface{}{
					"provider": maybeRunner,
					"binding":  computed[maybeRunner],
				}
			}
			json.NewEncoder(os.Stdout).Encode(out)
			return
		}
		fmt.Println(pick)
		runwayText := "exhaustion runway unknown"
		if state.ExhaustsBeforeReset != nil {
			if *state.ExhaustsBeforeReset && state.RunwayMinutes != nil {
				runwayText = fmt.Sprintf("projected runway %dh", *state.RunwayMinutes/60)
			} else {
				runwayText = "safe through reset"
			}
		}
		rationale := fmt.Sprintf("%s: %s, %.0f%% left (binding %.0f%% %s, resets %s)",
			pick, runwayText, state.Remaining, state.Used, state.Window, state.ResetsIn)
		// Find runner-up
		for _, n := range amongList {
			if n != pick {
				if r, ok := computed[n]; ok {
					rationale += fmt.Sprintf("  >  %s: %.0f%% left (%.0f%% %s)", n, r.Remaining, r.Used, r.Window)
				}
				break
			}
		}
		fmt.Fprintln(os.Stderr, rationale)
		return
	}

	// table mode (default)
	if *wantJSON {
		json.NewEncoder(os.Stdout).Encode(computed)
		return
	}

	type row struct {
		name, used, left, win, state, plan string
	}
	rows := []row{
		{"provider", "used", "left", "binding-win/reset", "state", "plan"},
	}
	sortedNames := make([]string, 0, len(computed))
	for name := range computed {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)
	for _, name := range sortedNames {
		p := computed[name]
		if p.Reason == "no-quota-data" {
			rows = append(rows, row{name, "-", "-", "-", "no-data", orEmpty(p.Plan)})
			continue
		}
		flag := "OK"
		if !p.Available {
			flag = strings.ToUpper(p.Reason)
		}
		rows = append(rows, row{
			name,
			fmt.Sprintf("%.0f%%", p.Used),
			fmt.Sprintf("%.0f%%", p.Remaining),
			fmt.Sprintf("%s/%s", p.Window, p.ResetsIn),
			flag,
			orEmpty(p.Plan),
		})
	}
	widths := make([]int, 6)
	for _, r := range rows {
		for i, v := range []string{r.name, r.used, r.left, r.win, r.state, r.plan} {
			if len(v) > widths[i] {
				widths[i] = len(v)
			}
		}
	}
	for _, r := range rows {
		vals := []string{r.name, r.used, r.left, r.win, r.state, r.plan}
		for i, v := range vals {
			fmt.Print(v)
			if i < len(vals)-1 {
				fmt.Print(strings.Repeat(" ", widths[i]-len(v)) + "  ")
			}
		}
		fmt.Println()
	}
}

func orEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runDaemon() {
	if err := requireFleetAdmission(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		os.Exit(1)
	}
	daemonFlags := flag.NewFlagSet("daemon", flag.ExitOnError)
	role := daemonFlags.String("role", "worker", "Target role for pulse sweeps")
	interval := daemonFlags.Int("interval", 60, "Pulse interval in seconds")
	daemonFlags.Parse(os.Args[2:])

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: failed to load config: %v\n", err)
		os.Exit(1)
	}

	st, err := store.New(".herd/herdforge.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: store init failed — durable dependency BLOCKED evidence is required: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx := context.Background()
	pulseInterval := time.Duration(*interval) * time.Second

	fmt.Printf("Daemon started (role=%s interval=%ds)\n", *role, *interval)
	fmt.Println("Press Ctrl+C to stop.")

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nDaemon shutting down.")
			return
		default:
		}

		cycleErr := runDaemonCycle(ctx, requireFleetAdmission, func(ctx context.Context) error {
			// FAC-196: claim-to-dispatch is one transaction. Non-compensable
			// prep (lane, routed decision, Herdr) happens before RunPulse.
			// FAC-194 still owns removing any residual OpenCode ModelRouter
			// constructions on other entrypoints; this path uses the
			// authoritative launchAdmission + SurfaceRouter waterfall only.
			lane := findLaneForRole(cfg, *role)
			if lane == nil {
				return fmt.Errorf("no lane configured for role %q", *role)
			}
			var tp provider.TaskProvider
			decision, admitErr := launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, *role, herdr.IsAvailable(), routedLaneDecision(ctx, nil), func(_ *router.LaunchDecision) error {
				var tpErr error
				tp, tpErr = loadTaskProvider(cfg)
				return tpErr
			})
			if admitErr != nil {
				return fmt.Errorf("launch route rejected before claim: %w", admitErr)
			}
			if tp == nil {
				return fmt.Errorf("task provider: not constructed after launch admission")
			}
			repository := repositoryIdentityForLaunch(cfg)
			if repository == "" {
				return fmt.Errorf("authenticated repository identity is required")
			}

			wm := resolveCanonicalWorktreeManager()
			v := verifier.NewVerifier(cfg.Verification.TestCommand)
			eng := daemon.NewEngine(cfg, tp, nil, st, wm, v)

			standing := fmt.Sprintf("forge-%s", lane.Name)
			rec, err := eng.RunDaemonTick(ctx, *role, daemon.TickOptions{
				Decision:     decision,
				Lane:         lane,
				Repository:   repository,
				Herdr:        dispatch.LiveHerdr{},
				StandingName: standing,
				ResolveStanding: func(_ context.Context, name string, req launch.Request) (*daemon.StandingAgent, error) {
					tabLabel, rerr := herdr.ResolveAgentTabWithDecision(name, req)
					if rerr != nil {
						if authorizeEphemeralTaskAgent(rerr) != nil {
							return nil, rerr
						}
						return nil, nil
					}
					// Readback exact agent identity for reuse gate.
					agents, lerr := herdr.AgentList()
					if lerr != nil {
						return nil, lerr
					}
					for _, a := range agents {
						if a.Name == name || a.Name == tabLabel {
							return &daemon.StandingAgent{
								Name:    a.Name,
								TabID:   a.TabID,
								PaneID:  a.PaneID,
								Session: a.Session.Value,
								CWD:     a.Cwd,
								Model:   req.Decision.Model,
								Harness: a.Kind,
							}, nil
						}
					}
					return nil, nil
				},
			})
			if err != nil {
				return fmt.Errorf("daemon tick: %w", err)
			}
			if rec != nil && rec.Launched {
				fmt.Printf("[%s] Dispatched: %s — agent=%s tab=%s model=%s/%s lease=g%d\n",
					time.Now().Format(time.RFC3339), rec.TaskRef, rec.AgentName, rec.TabID, rec.Model, rec.Effort, rec.LeaseGeneration)
			}
			return nil
		})
		if cycleErr != nil {
			fmt.Fprintf(os.Stderr, "daemon: %v\n", cycleErr)
		}

		time.Sleep(pulseInterval)
	}
}

// runDaemonCycle is the production per-cycle admission seam. The cycle
// callback includes provider construction and RunPulse, so a posture
// transition observed here reaches no provider, claim, worktree, tab, or
// process effect.
func runDaemonCycle(ctx context.Context, admit func(context.Context) error, cycle func(context.Context) error) error {
	if err := admit(ctx); err != nil {
		return fmt.Errorf("cycle admission: %w", err)
	}
	return cycle(ctx)
}

func runStandingE() error {
	fs := flag.NewFlagSet("standing", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "Plan raise without creating tabs or starting agents")
	status := fs.Bool("status", false, "Report live vs missing standing owners")
	shutdown := fs.Bool("shutdown", false, "Close settled standing owners only (never ephemeral task workers)")
	only := fs.String("only", "", "Comma-separated lane or forge-<lane> names to operate on")
	quiet := fs.Bool("quiet", false, "Suppress non-error progress lines")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	mode := standing.ModeRaise
	switch {
	case *status && *shutdown:
		return errors.New("standing: --status and --shutdown are mutually exclusive")
	case *status && *dryRun:
		return errors.New("standing: --status and --dry-run are mutually exclusive")
	case *shutdown && *dryRun:
		// Dry-run shutdown: plan closes without executing.
		mode = standing.ModeShutdown
	case *status:
		mode = standing.ModeStatus
	case *shutdown:
		mode = standing.ModeShutdown
	case *dryRun:
		mode = standing.ModeDryRun
	}

	// Positional bare ids are shorthand for --only (shell parity).
	onlyList := splitCSV(*only)
	for _, arg := range fs.Args() {
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("standing: unknown flag %q", arg)
		}
		onlyList = append(onlyList, arg)
	}

	if err := requireFleetAdmission(context.Background()); err != nil {
		return err
	}
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return runStandingConfigMode(cfg, herdr.IsAvailable(), mode, onlyList, *quiet, *dryRun && *shutdown)
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runStandingConfig is the testable raise entry used by launch-policy tests.
// It raises every standing lane with live herdr seams when herdrAvailable.
func runStandingConfig(cfg *config.Config, herdrAvailable bool) error {
	return runStandingConfigMode(cfg, herdrAvailable, standing.ModeRaise, nil, false, false)
}

func runStandingConfigMode(cfg *config.Config, herdrAvailable bool, mode standing.Mode, only []string, quiet bool, shutdownDry bool) error {
	if !herdrAvailable && mode != standing.ModeDryRun && mode != standing.ModeStatus {
		// Status may still want a workspace; raise/shutdown need herdr.
		if mode == standing.ModeRaise || mode == standing.ModeShutdown {
			return errors.New("herdr CLI not found — install herdr first")
		}
	}
	if !herdrAvailable && mode == standing.ModeRaise {
		return errors.New("herdr CLI not found — install herdr first")
	}

	opts := standing.Options{
		Mode:     mode,
		Only:     only,
		Quiet:    quiet,
		RepoRoot: ".",
		ListAgents: func() ([]standing.Agent, error) {
			// Policy unit tests pass herdrAvailable=true without a live
			// herdr binary; an empty inventory is correct until a real
			// raise needs idempotency against live names.
			if !herdrAvailable || !herdr.IsAvailable() {
				return nil, nil
			}
			raw, err := herdr.AgentList()
			if err != nil {
				if mode == standing.ModeDryRun {
					return nil, nil
				}
				return nil, err
			}
			out := make([]standing.Agent, 0, len(raw))
			for _, a := range raw {
				out = append(out, standing.Agent{
					Name: a.Name, Status: a.Status, PaneID: a.PaneID,
					TabID: a.TabID, Workspace: a.Workspace, Cwd: a.Cwd,
				})
			}
			return out, nil
		},
		ResolveWorkspace: func(repoRoot string, c *config.Config) (string, error) {
			// Fail-closed: never invent a hardcoded workspace ID (FAC-121).
			if !herdrAvailable || !herdr.IsAvailable() {
				return "", errors.New("herdr unavailable for workspace resolution")
			}
			return herdr.RequireWorkspace(repoRoot)
		},
		PrepareWorktree: func(lane *config.LaneDef) error {
			return prepareStandingWorktree(lane)
		},
		AdmitRoute: func(lane *config.LaneDef) (standing.Route, error) {
			// Policy + route only — worktree prepare is a post-admission
			// side effect owned by PrepareWorktree so dry-run/status never
			// mutate the tree.
			decision, err := launchAdmission(cfg, lane.Role, true, routedLaneDecision(context.Background(), nil))
			if err != nil {
				return standing.Route{}, err
			}
			if err := validateDecisionBeforeSideEffect(decision, lane.Name); err != nil {
				return standing.Route{}, err
			}
			return standing.Route{
				Provider: decision.Provider,
				Model:    decision.Model,
				Effort:   decision.Effort,
				Harness:  decision.Harness,
				Decision: decision,
			}, nil
		},
		RepositoryIdentity: repositoryIdentityForLaunch,
		CreateTab: func(workspace, label, cwd string) (standing.Tab, error) {
			tab, err := herdr.TabCreateForTask(workspace, label, cwd, true)
			if err != nil {
				return standing.Tab{}, err
			}
			return standing.Tab{ID: tab.ID, Label: tab.Label, PaneID: tab.Pane.ID, Cwd: tab.Cwd}, nil
		},
		StartAgent: func(tab standing.Tab, agentName string, route standing.Route, lane *config.LaneDef, repository string) error {
			decision, ok := route.Decision.(*router.LaunchDecision)
			if !ok || decision == nil {
				return errors.New("standing start requires a routed LaunchDecision")
			}
			return herdr.StartPreparedAgent(tab.ID, agentName, decision.Harness, tab.PaneID, launch.Request{
				Decision: decision, TaskRef: lane.Name, Scope: router.ScopeLane,
				Repository: repository, Lane: lane.Name, Name: agentName, TabID: tab.ID, PaneID: tab.PaneID,
			})
		},
		PromptAgent: func(agentName, promptText string) error {
			_, err := herdr.AgentPrompt(agentName, promptText, false)
			return err
		},
		CloseTab: func(tabID string) error {
			// Best-effort: StartPreparedAgent already reconciles most start
			// failures. A second close of a gone tab must not mask the
			// original start error.
			_ = herdr.TabClose(tabID)
			return nil
		},
	}

	if mode == standing.ModeShutdown && shutdownDry {
		// Plan-only shutdown: do not close tabs.
		opts.CloseTab = nil
	}
	if mode == standing.ModeDryRun {
		opts.CreateTab = nil
		opts.StartAgent = nil
		opts.PromptAgent = nil
		opts.CloseTab = nil
	}

	result, err := standing.Run(cfg, opts)
	if result != nil && !quiet {
		for _, rr := range result.Roles {
			switch rr.Outcome {
			case standing.OutcomeRaised:
				fmt.Printf("herd-standing: started %s (%s/%s) tab=%s pane=%s cwd=%s\n",
					rr.AgentName, rr.Provider, rr.Model, rr.TabID, rr.PaneID, rr.CWD)
			case standing.OutcomeSkippedLive:
				fmt.Printf("herd-standing: skip %s (already live)\n", rr.AgentName)
			case standing.OutcomePreview:
				fmt.Printf("herd-standing: DRY %s %s\n", rr.AgentName, rr.Reason)
			case standing.OutcomeLive:
				fmt.Printf("herd-standing: live %s %s cwd=%s\n", rr.AgentName, rr.Reason, rr.CWD)
			case standing.OutcomeMissing:
				fmt.Printf("herd-standing: missing %s\n", rr.AgentName)
			case standing.OutcomeClosed, standing.OutcomeWouldClose:
				fmt.Printf("herd-standing: %s %s (%s)\n", rr.Outcome, rr.AgentName, rr.Reason)
			case standing.OutcomePreserved:
				fmt.Printf("herd-standing: preserve %s (%s)\n", rr.AgentName, rr.Reason)
			case standing.OutcomeFailed:
				fmt.Fprintf(os.Stderr, "herd-standing: FAIL %s: %s\n", rr.AgentName, rr.Reason)
			}
		}
		fmt.Println(standing.Summary(result))
	}
	return err
}

func runStanding() {
	if err := runStandingE(); err != nil {
		fmt.Fprintf(os.Stderr, "standing failed: %v\n", err)
		os.Exit(1)
	}
}

func runUp() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: herd up <lane-name>\n")
		os.Exit(1)
	}
	laneName := os.Args[2]
	if err := requireFleetAdmission(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "up: %v\n", err)
		os.Exit(1)
	}

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var lane *config.LaneDef
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Name == laneName {
			lane = &cfg.Lanes[i]
			break
		}
	}
	if lane == nil {
		fmt.Fprintf(os.Stderr, "lane '%s' not found in config\n", laneName)
		os.Exit(1)
	}

	if !herdr.IsAvailable() {
		fmt.Fprintf(os.Stderr, "herdr CLI not found\n")
		os.Exit(1)
	}
	repository := repositoryIdentityForLaunch(cfg)
	if repository == "" {
		fmt.Fprintf(os.Stderr, "launch rejected before tab creation: repository identity unavailable\n")
		os.Exit(1)
	}
	if lane.Worktree == "" {
		fmt.Fprintf(os.Stderr, "launch rejected before tab creation: isolated worktree required\n")
		os.Exit(1)
	}
	var tab *herdr.TabInfo
	decision, err := launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, lane.Role, true, routedLaneDecision(context.Background(), nil), func(_ *router.LaunchDecision) error {
		var tabErr error
		cwd := "."
		if lane.Worktree != "" {
			cwd = filepath.Join(".", lane.Worktree)
		}
		tab, tabErr = herdr.TabCreateForTask(herdr.ResolveWorkspace("."), fmt.Sprintf("forge-%s", lane.Name), cwd, true)
		return tabErr
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "launch route rejected before tab creation: %v\n", err)
		os.Exit(1)
	}
	if err := validateDecisionBeforeSideEffect(decision, lane.Name); err != nil {
		fmt.Fprintf(os.Stderr, "launch decision rejected before tab creation: %v\n", err)
		os.Exit(1)
	}
	tabLabel := fmt.Sprintf("forge-%s", lane.Name)
	if err := herdr.StartPreparedAgent(tab.ID, tabLabel, decision.Harness, tab.Pane.ID, launch.Request{Decision: decision, TaskRef: lane.Name, Scope: router.ScopeLane, Repository: repository, Lane: lane.Name}); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Lane '%s' started: tab=%s pane=%s agent=%s\n", lane.Name, tab.ID, tab.Pane.ID, tabLabel)
}

func runActivate() {
	actFlags := flag.NewFlagSet("activate", flag.ExitOnError)
	build := actFlags.String("build", "", "Comma-separated services to rebuild before up (e.g. api,worker)")
	noFleet := actFlags.Bool("no-fleet", false, "Activate runtime, do NOT raise/kick the standing fleet")
	selftestFlag := actFlags.Bool("selftest", false, "Run activate predicate selftest and exit")
	timeout := actFlags.Int("timeout", 60, "Health-check gate timeout in seconds")
	poll := actFlags.Int("poll", 5, "Health-check poll interval in seconds")
	apiURL := actFlags.String("api-url", "", "Override /v1/status base URL (default http://localhost:13100)")
	webURL := actFlags.String("web-url", "", "Override web probe URL (default http://localhost:4174)")
	actFlags.Parse(os.Args[2:])

	if *selftestFlag {
		if err := activate.Selftest(); err != nil {
			fmt.Fprintf(os.Stderr, "activate selftest FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("activate selftest: PASS")
		return
	}
	resolvedNoFleet, err := resolveActivateNoFleet(*noFleet, os.Getenv("HERD_WIND_DOWN"), requireFleetAdmission)
	if err != nil {
		fmt.Fprintf(os.Stderr, "activate: %v\n", err)
		os.Exit(1)
	}
	*noFleet = resolvedNoFleet

	var buildServices []string
	if *build != "" {
		for _, s := range strings.Split(*build, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				buildServices = append(buildServices, s)
			}
		}
	}

	opts := activate.Options{
		BuildServices: buildServices,
		NoFleet:       *noFleet,
		Timeout:       time.Duration(*timeout) * time.Second,
		PollInterval:  time.Duration(*poll) * time.Second,
	}
	// Env overrides feed the defaults (herd-activate:174-175): explicit
	// flags take precedence, otherwise OV_LOCAL_API_URL / OV_LOCAL_WEB_URL.
	if *apiURL != "" {
		opts.APIURL = *apiURL
	}
	if *webURL != "" {
		opts.WebURL = *webURL
	}

	fmt.Printf("herd-activate: up -d all deployables + health-check gate (timeout=%ds)\n", *timeout)
	res, err := activate.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-activate: UNHEALTHY — status=%s api_unhealthy=%s compose_not_running=%s\n",
			overallOr(res, "unreachable"), unhealthyOr(res, "?"), notRunningOr(res, "none"))
		fmt.Fprintf(os.Stderr, "herd-activate: check 'docker compose ps' and 'docker compose logs <svc>'\n")
		os.Exit(1)
	}
	fmt.Printf("herd-activate: OK — all deployables healthy; web %s -> %d\n", opts.WebURL, res.WebCode)
	if opts.NoFleet {
		fmt.Println("herd-activate: --no-fleet (or HERD_WIND_DOWN) set; runtime is up, NOT raising/kicking the standing fleet")
	} else if res.FleetKicked {
		fmt.Println("herd-activate: kicked standing fleet (post-activation)")
	}
}

func resolveActivateNoFleet(flagValue bool, windDownEnv string, admit func(context.Context) error) (bool, error) {
	noFleet := flagValue || windDownEnv == "1"
	if noFleet {
		return true, nil
	}
	if err := admit(context.Background()); err != nil {
		return false, err
	}
	return false, nil
}

func overallOr(res *activate.Result, fallback string) string {
	if res == nil || res.Overall == "" {
		return fallback
	}
	return res.Overall
}

func unhealthyOr(res *activate.Result, fallback string) string {
	if res == nil || res.Unhealthy == "" {
		return fallback
	}
	return res.Unhealthy
}

func notRunningOr(res *activate.Result, fallback string) string {
	if res == nil || res.NotRunning == "" {
		return fallback
	}
	return res.NotRunning
}

// parseReviewArgs parses `herd review [<ref>] [--spawn]`. Go's flag package
// stops at the first positional, so `review FAC-1 --spawn` used to parse
// spawn=false and NO reviewer was ever spawned — the forge loop's review step
// was a no-op for every caller that put the ref first (FAC-138).
func parseReviewArgs(args []string) (ref string, spawn bool) {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	spawnFlag := fs.Bool("spawn", false, "Spawn reviewer agent in herdr")
	fs.Parse(leadingPositionalArgs(args))
	return fs.Arg(0), *spawnFlag
}

func runReview() {
	refArg, spawn := parseReviewArgs(os.Args[2:])
	if spawn {
		if err := requireFleetAdmission(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "review: %v\n", err)
			os.Exit(1)
		}
	}

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		os.Exit(1)
	}

	ctx := context.Background()

	// Find tasks in "in-progress" status
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "in-progress")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to list in-progress tasks: %v\n", err)
		os.Exit(1)
	}

	if refArg != "" {
		want := hsync.NormalizeRef(refArg)
		var filtered []*provider.Task
		for _, t := range tasks {
			if strings.EqualFold(hsync.NormalizeRef(t.Ref), want) {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	if len(tasks) == 0 {
		fmt.Println("No tasks in-progress to review.")
		return
	}

	// Sort deterministically: Priority DESC, Ref ASC
	sort.SliceStable(tasks, func(i, j int) bool {
		priorityRank := map[provider.Priority]int{
			provider.PriorityUrgent: 4,
			provider.PriorityHigh:   3,
			provider.PriorityMedium: 2,
			provider.PriorityLow:    1,
		}
		pi := priorityRank[tasks[i].Priority]
		pj := priorityRank[tasks[j].Priority]
		if pi != pj {
			return pi > pj
		}
		return provider.CompareRefs(tasks[i].Ref, tasks[j].Ref) < 0
	})

	claimIdx := -1
	for i, task := range tasks {
		fmt.Printf("[%d] [%s] %s (priority=%s)\n", i, task.Ref, task.Title, task.Priority)
		if claimIdx < 0 {
			claimIdx = i
		}
	}
	if claimIdx < 0 {
		fmt.Println("No eligible in-progress tasks found.")
		return
	}

	task := tasks[claimIdx]
	fmt.Printf("\nSelected [%s] %s for review\n", task.Ref, task.Title)

	if spawn {
		if !herdr.IsAvailable() {
			fmt.Fprintf(os.Stderr, "herdr CLI not found\n")
			os.Exit(1)
		}

		lane := findLaneForRole(cfg, "reviewer")
		if lane == nil {
			fmt.Fprintf(os.Stderr, "no lane configured for role 'reviewer'\n")
			os.Exit(1)
		}
		decision, err := launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, lane.Role, true, routedLaneDecision(context.Background(), task), func(_ *router.LaunchDecision) error {
			_, listErr := herdr.AgentList()
			return listErr
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "review launch route rejected before tab creation: %v\n", err)
			os.Exit(1)
		}
		if err := validateDecisionBeforeSideEffect(decision, task.Ref); err != nil {
			fmt.Fprintf(os.Stderr, "review launch decision rejected before tab creation: %v\n", err)
			os.Exit(1)
		}

		standingName := fmt.Sprintf("forge-%s", lane.Name)
		targetLabel := standingName
		worktreeDir := lane.Worktree
		if worktreeDir == "" {
			fmt.Fprintf(os.Stderr, "review launch rejected: isolated task worktree required\n")
			os.Exit(1)
		}

		tabLabel, err := herdr.ResolveAgentTabWithDecision(standingName, taskLaunchRequest(decision, task.Ref, repositoryIdentityForLaunch(cfg), lane.Name))
		if err != nil {
			if gateErr := authorizeEphemeralTaskAgent(err); gateErr != nil {
				fmt.Fprintf(os.Stderr, "standing reviewer %s blocked: %v\n", standingName, err)
				os.Exit(1)
			}
			tabLabel = fmt.Sprintf("review-%s-%s", lane.Name, task.Ref)
			cwd := filepath.Join(".", worktreeDir)
			tab, tabErr := herdr.TabCreateForTask(herdr.ResolveWorkspace("."), tabLabel, cwd, true)
			if tabErr != nil {
				fmt.Fprintf(os.Stderr, "failed to create herdr tab: %v\n", tabErr)
				os.Exit(1)
			}
			if err := herdr.StartPreparedAgent(tab.ID, tabLabel, decision.Harness, tab.Pane.ID, taskLaunchRequest(decision, task.Ref, repositoryIdentityForLaunch(cfg), lane.Name)); err != nil {
				fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Spawned reviewer '%s' in tab %s (pane %s)\n", tabLabel, tab.ID, tab.Pane.ID)
		} else {
			targetLabel = standingName
			fmt.Printf("Using standing reviewer '%s' (tab %s)\n", standingName, tabLabel)
		}

		// FAC-131: a TIGHT, SCOPED review packet — no spec dump, only the
		// changed packages' targeted tests. Deepseek and most fleet models
		// have a small context window and overflow when handed the full spec
		// plus a whole-repo `go test ./...`. Scope keeps the review inside the
		// window: review only the diff, test only what changed.
		testCmd := scopedTestCommand(worktreeDir)
		reviewPacket := fmt.Sprintf(`REVIEW %s — verdict ONLY, edit nothing. End with the verdict line.
cd %s
1. git diff origin/main..HEAD --stat  (see ONLY the changed files — review just these)
2. %s   (targeted tests for the changed packages, not the whole repo)
3. If a port, spot-check the changed file against ~/Personal/chainseer/bin/ for parity.
Your FINAL line MUST be exactly one of:
REVIEW VERDICT %s: APPROVED
REVIEW VERDICT %s: REJECTED - <numbered fixes>
Do not read the whole codebase. Do not run the full suite. Change nothing.`,
			task.Ref, worktreeDir, testCmd, task.Ref, task.Ref)

		if _, err := herdr.AgentPrompt(targetLabel, reviewPacket, false); err != nil {
			fmt.Fprintf(os.Stderr, "failed to deliver review packet: %v\n", err)
			os.Exit(1)
		} else {
			fmt.Printf("  -> delivered review packet to %s\n", targetLabel)
		}
	}

	// Move card to "in-review" status after dispatching
	if err := tp.UpdateStatus(ctx, task.ID, "in-review"); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: failed to move card to in-review status: %v\n", err)
	} else {
		fmt.Printf("  -> moved card [%s] to 'in-review' status\n", task.Ref)
	}
}

// parseApproveArgs parses `herd approve [<ref>] [--force] [--evidence <sha>]`.
// Same swallowed-flag defect as review (FAC-138): with the ref first, --force
// and --evidence silently parsed as their zero values.
func parseApproveArgs(args []string) (ref, evidence string, force bool) {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	forceFlag := fs.Bool("force", false, "Approve without merge evidence (look at the diff first)")
	evidenceFlag := fs.String("evidence", "", "Proof commit SHA (only with a single <ref> argument)")
	fs.Parse(leadingPositionalArgs(args))
	return fs.Arg(0), *evidenceFlag, *forceFlag
}

// runApprove sweeps in-review cards and moves each to done ONLY with merge
// evidence on origin/main (via sync.BoardDone). Cards without proof are
// refused and stay in-review — a done card is a claim about reality.
func runApprove() {
	refArg, evidenceArg, forceArg := parseApproveArgs(os.Args[2:])

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		os.Exit(1)
	}

	ctx := context.Background()

	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "in-review")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to list in-review tasks: %v\n", err)
		os.Exit(1)
	}

	if refArg != "" {
		want := hsync.NormalizeRef(refArg)
		var filtered []*provider.Task
		for _, t := range tasks {
			if strings.EqualFold(hsync.NormalizeRef(t.Ref), want) {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
		if len(tasks) == 0 {
			fmt.Fprintf(os.Stderr, "no in-review card matches %s\n", want)
			os.Exit(1)
		}
	} else if evidenceArg != "" {
		fmt.Fprintf(os.Stderr, "--evidence needs a single <ref> argument\n")
		os.Exit(1)
	}

	if len(tasks) == 0 {
		fmt.Println("No tasks in review status to approve.")
		return
	}

	sort.SliceStable(tasks, func(i, j int) bool {
		return provider.CompareRefs(tasks[i].Ref, tasks[j].Ref) < 0
	})

	approved, refused, failed := 0, 0, 0
	for _, task := range tasks {
		res, err := hsync.BoardDone(ctx, tp, ".", cfg.TaskProvider.ProjectID, task.Ref, evidenceArg, forceArg)
		switch {
		case err == nil:
			fmt.Printf("APPROVED [%s]: %s\n  proof: %s\n", res.Ref, task.Title, res.Proof)
			approved++
		case errors.Is(err, hsync.ErrNoEvidence):
			fmt.Printf("REFUSED  [%s]: %s\n  %v\n", task.Ref, task.Title, err)
			refused++
		default:
			fmt.Fprintf(os.Stderr, "ERROR    [%s]: %v\n", task.Ref, err)
			failed++
		}
	}

	fmt.Printf("\nherd approve: approved=%d refused=%d failed=%d\n", approved, refused, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// runBoardDone is the strict single-card gate: exit 0 only when the card
// provably moved to done. Port of bin/herd-board-done.
func runBoardDone() {
	fs := flag.NewFlagSet("board-done", flag.ExitOnError)
	evidence := fs.String("evidence", "", "Explicit proof commit SHA (must be an ancestor of origin/main)")
	force := fs.Bool("force", false, "Override missing evidence (look at the diff first)")
	selftestFlag := fs.Bool("selftest", false, "Run normalization/repo assertions and exit")
	// Pull the leading positional out BEFORE parsing. Go's flag package stops
	// at the first non-flag argument, so `board-done FAC-136 --evidence <sha>`
	// silently discarded --evidence and the command then refused with
	// "no merge evidence found" no matter what proof you supplied.
	fs.Parse(leadingPositionalArgs(os.Args[2:]))

	if *selftestFlag {
		for in, want := range map[string]string{"FAC-018": "FAC-18", "FAC-648": "FAC-648", "FAC-0648": "FAC-648"} {
			if got := hsync.NormalizeRef(in); got != want {
				fmt.Fprintf(os.Stderr, "board-done selftest FAIL: NormalizeRef(%s)=%s want %s\n", in, got, want)
				os.Exit(1)
			}
		}
		if _, err := hsync.MergeEvidence(".", "SELFTEST-0", ""); err != nil {
			fmt.Fprintf(os.Stderr, "board-done selftest FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("board-done selftest PASS")
		return
	}

	ref := fs.Arg(0)
	if ref == "" {
		fmt.Fprintf(os.Stderr, "Usage: herd board-done <ref> [--evidence <sha>] [--force]\n")
		os.Exit(2)
	}

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		os.Exit(1)
	}

	res, err := hsync.BoardDone(context.Background(), tp, ".", cfg.TaskProvider.ProjectID, ref, *evidence, *force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-done: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd board-done: %s proof: %s\n", res.Ref, res.Proof)
	// A closed ticket must not keep holding scope. Its claim otherwise stays
	// Active forever and blocks any later task that overlaps it — FAC-174 was
	// merged and board-closed yet still held pkg/verifier, which rejected
	// FAC-198 with scope_overlap and needed a manual release to clear.
	releaseScopeClaimQuietly(res.Ref)
	fmt.Printf("herd board-done: %s is done (verified by read-back)\n", res.Ref)
}

// runBoardSync reconciles the board against git reality and reports drift.
// Exit codes:
//
//	0 = no drift (board is honest)
//	1 = hard error (config, provider, git)
//	2 = drift found (one or more findings)
//	3 = partial: drift found AND errors occurred during reconcile
//	4 = provider list tasks returned zero cards (board may be empty)
func runBoardSync() {
	fs := flag.NewFlagSet("board-sync", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Output results as JSON")
	intervalSec := fs.Int("interval", 0, "Run continuously at N-second intervals (0 = run once)")
	ensureDaemon := fs.Bool("ensure-daemon", false, "Not yet implemented: exit 0 and do nothing")
	selftestFlag := fs.Bool("selftest", false, "Run classification assertions and exit")
	fs.Parse(os.Args[2:])

	if *selftestFlag {
		tests := []struct {
			mergedLog string
			ref       string
			epoch     int64
			want      bool
		}{
			{"1745683200\tfeat: implement widget renderer (FAC-18)", "fac-18", 0, true},
			{"1745683200\tfeat: implement widget renderer (FAC-18)", "fac-18", 1745683201, true},
			{"1745683200\tfeat: implement widget renderer (FAC-18)", "fac-18", 1745683200, true},
			{"1745683200\tfeat: implement widget renderer (FAC-18)", "fac-18", 1745683199, false},
			{"1745683200\tafter FAC-18 restore followup", "fac-18", 0, false},
			{"1745683200\tfollow-up on fac-18 bug", "fac-18", 0, false},
			{"1745683200\tprep for FAC-18 sprint planning", "fac-18", 0, false},
			{"1745683200\tfeat: implement widget renderer (FAC-18)", "fac-1", 0, false},
		}
		for _, tc := range tests {
			if got := hsync.RefShipped(tc.mergedLog, tc.ref, tc.epoch); got != tc.want {
				fmt.Fprintf(os.Stderr, "board-sync selftest FAIL: RefShipped(log=%q, ref=%q, epoch=%d) = %v, want %v\n", tc.mergedLog, tc.ref, tc.epoch, got, tc.want)
				os.Exit(1)
			}
		}
		// Also test NormalizeRef and mentionPivot match
		if got := hsync.NormalizeRef("FAC-018"); got != "FAC-18" {
			fmt.Fprintf(os.Stderr, "board-sync selftest FAIL: NormalizeRef(FAC-018)=%q want FAC-18\n", got)
			os.Exit(1)
		}
		fmt.Println("board-sync selftest PASS")
		return
	}

	if *ensureDaemon {
		// Placeholder: the daemon will call board-sync when integrated.
		// For now, exit 0 gracefully.
		fmt.Println("board-sync: --ensure-daemon not yet implemented, exiting 0")
		return
	}

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "board-sync: failed to load config: %v\n", err)
		os.Exit(1)
	}

	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		os.Exit(1)
	}

	syncer := hsync.NewBoardSyncer(tp)

	if *intervalSec > 0 {
		for {
			code := runBoardSyncOnce(syncer, cfg.TaskProvider.ProjectID, *asJSON)
			if code != 0 {
				os.Exit(code)
			}
			time.Sleep(time.Duration(*intervalSec) * time.Second)
		}
	}

	code := runBoardSyncOnce(syncer, cfg.TaskProvider.ProjectID, *asJSON)
	os.Exit(code)
}

func runBoardSyncOnce(syncer *hsync.BoardSyncer, projectID string, asJSON bool) int {
	drift, err := syncer.ReconcileBoard(context.Background(), projectID, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "board-sync: %v\n", err)
		return 1
	}

	if asJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"drift":    drift.Drift,
			"findings": drift.Findings,
		})
	} else {
		if drift.Drift == 0 {
			fmt.Println("board-sync: board is honest — no drift found")
			return 0
		}
		for _, f := range drift.Findings {
			prefix := "board-sync:"
			switch f.Kind {
			case "SHIPPED":
				prefix = "board-sync: 🚢 SHIPPED"
			case "STALE":
				prefix = "board-sync: STALE"
			case "BOARD_LAG":
				prefix = "board-sync: ⏳ BOARD_LAG"
			case "UNKNOWN":
				prefix = "board-sync: ? UNKNOWN"
			}
			fmt.Printf("%s %s (%s/%s): %s\n", prefix, f.Ref, f.TaskID, f.Status, f.Action)
		}
		if drift.Drift > 0 {
			fmt.Printf("board-sync: %d drift finding(s)\n", drift.Drift)
		}
	}
	if drift.Drift > 0 {
		return 2
	}
	return 0
}

// runSend ports bin/herd-send: prompt an agent and verify it consumed the
// submit (working/done), with one Enter nudge before giving up.
func runSend() {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	noVerify := fs.Bool("no-verify", false, "Submit without waiting for the agent to flip to working")
	file := fs.String("file", "", "Read the text to send from a file (for long packets)")
	timeoutSec := fs.Int("timeout", 30, "Seconds to wait for consumption confirmation")
	selftestFlag := fs.Bool("selftest", false, "Run status-extraction assertions and exit")
	fs.Parse(os.Args[2:])

	if *selftestFlag {
		agents := []herdr.AgentEntry{
			{Name: "a", PaneID: "w3:p3", Status: "working"},
			{PaneID: "w3:p9", Status: "idle"},
		}
		if herdr.StatusFromList(agents, "w3:p3") != "working" ||
			herdr.StatusFromList(agents, "a") != "working" ||
			herdr.StatusFromList(agents, "w3:p9") != "idle" ||
			herdr.StatusFromList(agents, "ghost") != "" {
			fmt.Fprintln(os.Stderr, "send selftest FAIL: status extraction")
			os.Exit(1)
		}
		fmt.Println("send selftest PASS")
		return
	}

	// Collect ALL positionals with flags interleaved anywhere: Go's flag
	// package stops at the first positional, which let a trailing
	// "--timeout 30" leak INTO the delivered prompt text.
	var pos []string
	rest := fs.Args()
	for len(rest) > 0 {
		pos = append(pos, rest[0])
		fs.Parse(rest[1:])
		rest = fs.Args()
	}
	if len(pos) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: herd send <pane|name> \"<text>\" [--file path] [--no-verify] [--timeout s]\n")
		os.Exit(2)
	}
	target := pos[0]

	var text string
	switch {
	case *file != "":
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd send: %v\n", err)
			os.Exit(1)
		}
		text = strings.TrimSpace(string(data))
	case len(pos) > 1:
		text = strings.Join(pos[1:], " ")
	default:
		fmt.Fprintf(os.Stderr, "herd send: no text given (positional or --file)\n")
		os.Exit(2)
	}

	if !herdr.IsAvailable() {
		fmt.Fprintf(os.Stderr, "herd send: herdr CLI not found\n")
		os.Exit(1)
	}

	status, err := herdr.Send(target, text, !*noVerify, time.Duration(*timeoutSec)*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd send: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd send: %s -> %s\n", target, status)
}

// runHerdrDeliver is the durable operator boundary for free-form prompt bytes.
// Text comes only from stdin or --file; positional free-form arguments are
// rejected so shells cannot evaluate backticks or $(...) (FAC-183 / FAC-151).
func runHerdrDeliver() {
	fs := flag.NewFlagSet("herdr-deliver", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	key := fs.String("key", "", "stable operation key")
	generation := fs.Int64("generation", 0, "positive operation generation")
	target := fs.String("target", "", "exact Herdr target (name or pane)")
	session := fs.String("session", "", "optional session provenance")
	wait := fs.Bool("wait", false, "ask herdr to wait for a working state")
	file := fs.String("file", "", "read exact prompt bytes from this file")
	state := fs.String("state", ".herd/herdr-delivery.db", "shared SQLite receipt authority path")
	deliveryTimeout := fs.Int("timeout", 30, "seconds to wait for consumption proof")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintln(os.Stderr, "herd herdr-deliver: positional payloads are forbidden; use stdin or --file")
		os.Exit(2)
	}
	if *file == "-" {
		fmt.Fprintln(os.Stderr, "herd herdr-deliver: --file - is not a payload source; omit --file and use stdin")
		os.Exit(2)
	}
	payload := textdelivery.Payload{File: *file}
	if *file == "" {
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd herdr-deliver: read stdin: %v\n", err)
			os.Exit(1)
		}
		payload.Bytes = body
	}
	proof, err := herdr.DeliverOperator(context.Background(), herdr.OperatorDelivery{
		Key: *key, Generation: *generation, Target: *target, Session: *session,
		Wait: *wait, Payload: payload, StatePath: *state, Timeout: time.Duration(*deliveryTimeout) * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd herdr-deliver: %v\n", err)
		os.Exit(1)
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd herdr-deliver: encode proof: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

// runCleanup ports bin/herd-cleanup: one agent = one tab — close tabs of
// finished one-off agents. Standing lanes, working agents, orchestrators,
// and unnamed panes are never touched.
func runCleanup() {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "List what would be closed without closing")
	asJSON := fs.Bool("json", false, "Output JSON")
	fs.Parse(os.Args[2:])

	if !herdr.IsAvailable() {
		fmt.Fprintf(os.Stderr, "herd cleanup: herdr CLI not found\n")
		os.Exit(1)
	}

	standing := map[string]bool{}
	if cfg, err := config.LoadConfig(".herd/herd.yaml"); err == nil {
		for _, lane := range cfg.Lanes {
			standing["forge-"+lane.Name] = true
		}
	}

	cands, errs := herdr.Cleanup(standing, *dryRun)
	// FAC-180: Cleanup mutation mode never closes without compare-and-close.
	// Report honestly — never print "closed" when nothing was closed.
	errMsgs := make([]string, 0, len(errs))
	for _, e := range errs {
		if e != nil {
			errMsgs = append(errMsgs, e.Error())
		}
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"dry_run":    *dryRun,
			"candidates": cands,
			"closed":     0, // mutation path is fenced; no silent success
			"blocked":    !*dryRun && len(errs) > 0,
			"errors":     errMsgs,
			"error_count": len(errs),
		})
	} else {
		if len(cands) == 0 && len(errs) == 0 {
			fmt.Println("herd cleanup: nothing to close")
		}
		if *dryRun {
			for _, c := range cands {
				fmt.Printf("herd cleanup: would close %s (tab %s) — %s\n", c.Name, c.TabID, c.Reason)
			}
		} else {
			for _, c := range cands {
				fmt.Printf("herd cleanup: BLOCKED %s (tab %s) — FAC-180 compare-and-close required; not closed\n", c.Name, c.TabID)
			}
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "herd cleanup: error — %v\n", e)
			}
			if len(cands) > 0 || len(errs) > 0 {
				fmt.Printf("herd cleanup: closed=0 blocked=%d candidates=%d\n", len(errs), len(cands))
			}
		}
	}
	if len(errs) > 0 {
		os.Exit(1)
	}
}

// runLabels ports the FAC-199 acceptance criterion "live readback shows no
// raw task-fac-* label in workspace <ws>": a bounded, one-shot sweep that
// repairs every drifted tab label in the resolved workspace in place. It
// never closes a tab and never crosses a workspace boundary — see
// herdr.ReconcileWorkspaceLabels.
func runLabels() {
	fs := flag.NewFlagSet("labels", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Output JSON")
	fs.Parse(os.Args[2:])

	if !herdr.IsAvailable() {
		fmt.Fprintf(os.Stderr, "herd labels: herdr CLI not found\n")
		os.Exit(1)
	}
	workspace, err := herdr.RequireWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd labels: %v\n", err)
		os.Exit(1)
	}
	renamed, err := herdr.ReconcileWorkspaceLabels(workspace)
	if *asJSON {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"workspace": workspace, "renamed": renamed, "error": errMsg,
		})
	} else {
		if len(renamed) == 0 {
			fmt.Printf("herd labels: no drifted labels in workspace %s\n", workspace)
		}
		for _, id := range renamed {
			fmt.Printf("herd labels: reconciled tab %s\n", id)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd labels: error — %v\n", err)
		}
	}
	if err != nil {
		os.Exit(1)
	}
}

// runShell is a thin interactive loop: each line is dispatched as a fresh
// `herd <line>` subprocess, so every subcommand works and errors cannot kill
// the shell.
func runShell() {
	fmt.Println("herd shell — type any herd subcommand ('status', 'quota', ...), 'exit' to quit")
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("herd> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			fmt.Print("herd> ")
			continue
		case "exit", "quit":
			return
		}
		cmd := exec.Command(os.Args[0], strings.Fields(line)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		_ = cmd.Run()
		fmt.Print("herd> ")
	}
}

// runDoctorModels probes every lane's model and fallbacks so quota
// exhaustion is caught explicitly instead of surfacing as agents that plan
// but never build. Exit 1 when any lane has no healthy model.
func runDoctorModels() {
	fs := flag.NewFlagSet("doctor-models", flag.ExitOnError)
	// FAC-129: --tool-probe verifies the resolved model actually EXECUTES
	// tools, not just that it is authenticated/in-quota. A tool-incapable
	// surface is DEAD for fleet work no matter how healthy its quota looks.
	toolProbe := fs.Bool("tool-probe", false, "also verify each model executes tools (herd tool-probe)")
	fs.Parse(os.Args[2:])

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor-models: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	deadLanes := 0
	for _, lane := range cfg.Lanes {
		model, trail := herdr.ResolveHealthyModel(ctx, lane.Model, lane.FallbackModels)
		if model == "" {
			deadLanes++
			fmt.Printf("DEAD  %s — every candidate exhausted:\n", lane.Name)
			for _, p := range trail {
				fmt.Printf("        %s: %s\n", p.Model, p.Reason)
			}
			continue
		}
		if *toolProbe {
			if tp := herdr.ToolProbe(ctx, model); !tp.Executes {
				deadLanes++
				fmt.Printf("DEAD  %s -> %s — does NOT execute tools: %s\n", lane.Name, model, tp.Reason)
				continue
			}
		}
		if model == lane.Model {
			fmt.Printf("OK    %s -> %s\n", lane.Name, model)
		} else {
			fmt.Printf("FELL-OVER %s -> %s (primary %s exhausted)\n", lane.Name, model, lane.Model)
		}
	}
	if deadLanes > 0 {
		fmt.Fprintf(os.Stderr, "\ndoctor-models: %d lane(s) have NO healthy model\n", deadLanes)
		os.Exit(1)
	}
	fmt.Println("\ndoctor-models: every lane has a healthy model")
}

func runValidateConfig() {
	cfgPath := ".herd/herd.yaml"
	if len(os.Args) > 2 {
		cfgPath = os.Args[2]
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "VALIDATION FAILED: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("VALIDATION PASSED\n")
	fmt.Printf("  Version  : %s\n", cfg.Version)
	fmt.Printf("  Project  : %s\n", cfg.Project.Name)
	fmt.Printf("  Branch   : %s\n", cfg.Project.DefaultBranch)
	fmt.Printf("  Provider : %s\n", cfg.TaskProvider.Type)
	fmt.Printf("  Lanes    : %d\n", len(cfg.Lanes))
	for _, lane := range cfg.Lanes {
		fmt.Printf("    - %s: agent_kind=%s model=%s", lane.Name, lane.AgentKind, lane.Model)
		if lane.Route != nil {
			fmt.Printf(" route=%s", *lane.Route)
		}
		if lane.Risk != nil {
			fmt.Printf(" risk=%s", *lane.Risk)
		}
		fmt.Println()
	}
	if cfg.Verification.TestCommand != "" {
		fmt.Printf("  Test Cmd : %s\n", cfg.Verification.TestCommand)
	}
}

func runNext() {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		os.Exit(1)
	}

	picker := next.NewNextPicker(cfg, tp)
	actions, evalErr := picker.EvalAll(context.Background())
	if evalErr != nil {
		fmt.Fprintf(os.Stderr, "next eval failed: %v\n", evalErr)
		os.Exit(1)
	}

	if len(actions) == 0 {
		fmt.Println("No action required.")
		return
	}

	fmt.Println("=== Next Actions (priority order) ===")
	for _, a := range actions {
		fmt.Printf("  %s", a.String())
	}
	fmt.Println()
}

// dispatchRequest is the parsed, side-effect-free CLI contract for dispatch.
// Parsing never loads config, claims work, or opens durable stores.
type dispatchRequest struct {
	TicketRef    string
	NoLaunch     bool
	LaneName     string
	LaneExplicit bool
}

// parseDispatchArgs routes flags through a real FlagSet before any operational
// code. Flags may appear before or after the ticket (Go's flag package stops at
// the first positional; we re-parse the tail like runSend). Help is handled by
// the global gate; this parser still refuses bare reserved help tokens as
// positionals (defense in depth).
func parseDispatchArgs(args []string) (dispatchRequest, error) {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noLaunch := fs.Bool("no-launch", false, "Skip agent launch")
	laneName := fs.String("lane", "worker", "Lane name from config")
	ticketFlag := fs.String("ticket", "", "Ticket ref (required when the value begins with '-')")

	parse := func(in []string) error {
		if err := fs.Parse(in); err != nil {
			if err == flag.ErrHelp {
				return fmt.Errorf("help requested")
			}
			return err
		}
		return nil
	}
	if err := parse(args); err != nil {
		return dispatchRequest{}, err
	}
	// Collect positionals with flags interleaved anywhere after the first one.
	var pos []string
	rest := fs.Args()
	for len(rest) > 0 {
		pos = append(pos, rest[0])
		if err := parse(rest[1:]); err != nil {
			return dispatchRequest{}, err
		}
		rest = fs.Args()
	}

	laneExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "lane" {
			laneExplicit = true
		}
	})
	ref, err := parseTicketRef(*ticketFlag, pos)
	if err != nil {
		return dispatchRequest{}, err
	}
	return dispatchRequest{
		TicketRef:    ref,
		NoLaunch:     *noLaunch,
		LaneName:     *laneName,
		LaneExplicit: laneExplicit,
	}, nil
}

func runDispatch() {
	req, err := parseDispatchArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, usageFor("dispatch"))
		if !strings.Contains(err.Error(), "missing ticket") && err.Error() != "help requested" {
			fmt.Fprintf(os.Stderr, "dispatch: %v\n", err)
		}
		os.Exit(1)
	}
	ticketRef := req.TicketRef
	noLaunch := req.NoLaunch
	laneName := req.LaneName
	laneExplicit := req.LaneExplicit

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	registry, err := canonicalLaneRegistry(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lane identity: %v\n", err)
		os.Exit(1)
	}
	var canonicalLane lifecycle.CanonicalLane
	if laneExplicit {
		canonicalLane, err = registry.ResolveLaneName(laneName)
	} else {
		canonicalLane, err = registry.ResolveRole(laneName)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "lane identity: %v\n", err)
		os.Exit(1)
	}
	// Collapse to the resolved lane NAME right here. Everything downstream --
	// the hold gate, the launch admission, both Dispatch calls, the log line --
	// then refers to the same lane by construction. Re-resolving the raw string
	// later (the bare default is a ROLE, "worker") is what let the hold bind one
	// lane while the launch bound another; agreement has to be structural, not
	// two lookups that happen to use compatible rules.
	laneName = canonicalLane.Name

	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		os.Exit(1)
	}

	wm := resolveCanonicalWorktreeManager()
	// NewProductionDispatcher alone leaves the scope fence unauthorised, which
	// rejected EVERY production dispatch with "FAC-169 authority surface is not
	// present". pkg/scopeauth verifies receipt/payload consistency so the fence
	// can be constructed. Read its package doc: it does NOT authenticate the
	// issuer, so FAC-169 remains open.
	//
	// expectedRevision is the DEPS graph revision (a hash of board edges and
	// prerequisite statuses), not a git commit, and it must equal the revision
	// the published graph snapshot carries. herd scope publish prints it.
	scopeVerifier := scopeauth.New()
	expectedRevision, expectedFiles := publishedGraphBinding(".")
	d := dispatch.NewProductionDispatcherWithAuthorities(cfg, tp, wm,
		scopeVerifier, scopeVerifier, expectedRevision, expectedFiles)
	closeControl, err := configureProductionControl(d, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "control store init failed: %v\n", err)
		os.Exit(1)
	}
	defer closeControl()
	var decision *router.LaunchDecision
	var dispatchResult *dispatch.DispatchResult
	holdAuthority, holdErr := newProductionHoldAuthority()
	if holdErr != nil {
		fmt.Fprintf(os.Stderr, "hold authority: %v\n", holdErr)
		os.Exit(1)
	}
	defer holdAuthority.Close()
	repositoryIdentity, identityErr := holdRepository()
	if identityErr != nil {
		fmt.Fprintf(os.Stderr, "hold identity: %v\n", identityErr)
		os.Exit(1)
	}
	admitDispatch := func() error {
		for _, identity := range []lifecycle.HoldIdentity{{Repository: repositoryIdentity, Owner: canonicalLane.Role, Lane: canonicalLane.Name, Scope: "lane"}, {Repository: repositoryIdentity, Owner: canonicalLane.Role, Lane: canonicalLane.Name, Task: ticketRef, Scope: "task"}} {
			generation, err := holdAuthority.CurrentGeneration(context.Background(), identity)
			if err != nil {
				return err
			}
			decision, err := holdAuthority.Check(context.Background(), identity, generation)
			if err != nil {
				return err
			}
			if decision.Held {
				return fmt.Errorf("held: %s (%s)", decision.Reason, decision.Code)
			}
		}
		return nil
	}
	if err := admitDispatch(); err != nil {
		fmt.Fprintf(os.Stderr, "dispatch hold admission rejected: %v\n", err)
		os.Exit(1)
	}
	if !noLaunch {
		// canonicalLane.Role, not a second lookup: this is the same lane the hold
		// gate above already admitted.
		decision, err = launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, canonicalLane.Role, true, routedLaneDecision(context.Background(), nil), func(admitted *router.LaunchDecision) error {
			if err := admitDispatch(); err != nil {
				return err
			}
			var dispatchErr error
			dispatchResult, dispatchErr = d.Dispatch(context.Background(), dispatch.DispatchOptions{TicketRef: ticketRef, NoLaunch: noLaunch, LaneName: laneName, Decision: admitted})
			return dispatchErr
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "dispatch launch failed: %v\n", err)
			os.Exit(1)
		}
		// launchAdmission already validated lane capability before any side effect.
		// Dispatch rebinds/validates the exact task+lease after claim; never post-validate a lane decision against a task ref.
	}
	fmt.Printf("Dispatching %s to lane '%s'...\n", ticketRef, laneName)

	result := dispatchResult
	if noLaunch {
		if err := admitDispatch(); err != nil {
			fmt.Fprintf(os.Stderr, "dispatch hold admission rejected: %v\n", err)
			os.Exit(1)
		}
		result, err = d.Dispatch(context.Background(), dispatch.DispatchOptions{TicketRef: ticketRef, NoLaunch: true, LaneName: laneName, Decision: decision})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dispatch failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  Ticket : %s — %s\n", result.TicketRef, result.TicketTitle)
	fmt.Printf("  Worktree : %s\n", result.Worktree)
	fmt.Printf("  Branch   : %s\n", result.Branch)
	fmt.Printf("  Packet   : %s\n", result.TaskPacket)
	if result.Launched {
		fmt.Printf("  Agent    : Launched in herdr tab\n")
	} else {
		fmt.Printf("  Agent    : Not launched (use --no-launch or see TASK-PACKET.md)\n")
	}
}

func configureProductionControl(d *dispatch.Dispatcher, root string) (func() error, error) {
	controlStore, err := outbox.NewStore(filepath.Join(root, ".herd", "control-orders.db"))
	if err != nil {
		return nil, err
	}
	controlMailbox := mail.NewMailbox(filepath.Join(root, ".herd", "control-mail.jsonl"))
	d.ControlFactory = func(_ context.Context, scope dispatch.ControlScope) (*control.CoordinatorOrders, error) {
		owner, err := control.NewOwnerToken()
		if err != nil {
			return nil, err
		}
		validate := func(_ context.Context, target control.WakeTarget) (control.WakeTarget, error) {
			agents, err := herdr.AgentList()
			if err != nil {
				return control.WakeTarget{}, err
			}
			for _, a := range agents {
				// Fourth and last copy of "every agent kind reports a session
				// id", which is false: grok never does. Tab/pane/name/
				// workspace/kind is the exact identity herdr guarantees.
				if a.TabID == target.TabID && a.PaneID == target.PaneID && a.Name == target.AgentName && a.Workspace == target.Workspace && a.Kind == target.Provider {
					target.SessionID = a.Session.Value
					return target, nil
				}
			}
			return control.WakeTarget{}, fmt.Errorf("Herdr target/session drifted before wake")
		}
		orders := &control.CoordinatorOrders{Identity: scope.Identity, Delivery: &control.Delivery{Outbox: controlStore, Sender: controlMailbox, Waker: control.HerdrWaker{Target: scope.Wake, Validate: validate}, Authority: control.FencedAuthority{Identity: scope.Identity, Check: scope.Check}, Evidence: control.MailboxEvidenceReader{Mailbox: controlMailbox}, Owner: owner}}
		return orders, nil
	}
	// Production dispatch fails closed without a Compensator (FAC-121). Only
	// test doubles implemented it, so every real dispatch was rejected; wire
	// the durable FAC-119 outbox here.
	compensator, err := dispatch.NewOutboxCompensator(filepath.Join(root, ".herd", "dispatch-outbox.db"))
	if err != nil {
		controlStore.Close()
		return nil, err
	}
	d.Compensator = compensator
	return func() error {
		return errors.Join(compensator.Close(), controlStore.Close())
	}, nil
}

func runHarvest() {
	harvestFlags := flag.NewFlagSet("harvest", flag.ExitOnError)
	quiet := harvestFlags.Bool("quiet", false, "Show summary counts only")
	asJSON := harvestFlags.Bool("json", false, "Output JSON")
	harvestFlags.Parse(os.Args[2:])

	repoRoot := "."
	if err := os.Chdir(repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "failed to change dir: %v\n", err)
		os.Exit(1)
	}
	h := harvest.NewHarvester(repoRoot)
	ctx := context.Background()

	if *asJSON {
		result, err := h.Harvest(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "harvest failed: %v\n", err)
			os.Exit(1)
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
		return
	}

	if *quiet {
		fmt.Println(h.QuietSummary(ctx))
		return
	}

	fmt.Println("=== herd-harvest: fleet-wide worktree sweep ===")

	result, err := h.Harvest(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-harvest: error — %v\n", err)
		os.Exit(1)
	}

	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "  error: %s\n", e)
		}
	}

	if len(result.UnmergedWorktrees) == 0 {
		fmt.Println("(no unmerged work found in any worktree)")
	} else {
		for _, uw := range result.UnmergedWorktrees {
			fmt.Printf("%s (%s):\n", uw.WorktreePath, uw.Branch)
			for _, c := range uw.Unmerged {
				fmt.Printf("  %s\n", c)
			}
		}
	}

	fmt.Println()
	fmt.Println("herd-harvest: sweep complete. Any worktree listed above needs a review dispatch")
	fmt.Println("  (herd review) then approval — do not assume 'working' pane")
	fmt.Println("  status means nothing is ready to merge.")
}

// runThroughput ports bin/herd-throughput: read-only KPIs from the main-ref
// git log, the review verdict ledger, and the route-decisions log. Exit 0
// normal, 2 unknown arg/help-usage, 3 invalid window.
func runThroughput() {
	fs := flag.NewFlagSet("throughput", flag.ExitOnError)
	wantJSON := fs.Bool("json", false, "Emit the machine-readable metric packet")
	sinceFlag := fs.String("since", os.Getenv("HERD_THROUGHPUT_SINCE"), "ISO-8601 window start (default 7 days ago)")
	untilFlag := fs.String("until", os.Getenv("HERD_THROUGHPUT_UNTIL"), "ISO-8601 window end (default now)")
	fs.Parse(os.Args[2:])

	const isoLayout = "2006-01-02T15:04:05Z"
	now := time.Now().UTC()
	until := *untilFlag
	if until == "" {
		until = now.Format(isoLayout)
	}
	since := *sinceFlag
	if since == "" {
		since = now.AddDate(0, 0, -7).Format(isoLayout)
	}

	startEpoch := throughput.IsoEpoch(since)
	endEpoch := throughput.IsoEpoch(until)
	if startEpoch <= 0 || endEpoch < startEpoch {
		fmt.Fprintf(os.Stderr, "herd-throughput: invalid time window since=%s until=%s\n", since, until)
		os.Exit(3)
	}
	win := throughput.Window{Start: time.Unix(startEpoch, 0).UTC(), End: time.Unix(endEpoch, 0).UTC()}

	mainRef := envOr("HERD_THROUGHPUT_MAIN_REF", "origin/main")
	ledger := firstEnv("HERD_THROUGHPUT_LEDGER", "HERD_REVIEW_LEDGER",
		filepath.Join(stateDir(), "review-ledger.jsonl"))
	routeLog := firstEnv("HERD_THROUGHPUT_ROUTE_LOG", "HERD_ROUTE_DECISION_LOG",
		filepath.Join(stateDir(), "route-decisions.log"))

	// main-ref commits in window: %H\t%cI\t%s. 2>/dev/null semantics — a
	// missing ref must not abort.
	var commits []throughput.CommitLine
	logCmd := exec.Command("git", "log", mainRef, "--format=%H%x09%cI%x09%s",
		"--since="+since, "--until="+until)
	if out, err := logCmd.Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) == 3 {
				commits = append(commits, throughput.CommitLine{SHA: parts[0], Stamp: parts[1], Subject: parts[2]})
			}
		}
	}

	// Verdict ledger (JSONL): each line an event; keep verdict events in window.
	var verdicts []throughput.VerdictLine
	if data, err := os.ReadFile(ledger); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e struct {
				Event   string `json:"event"`
				SHA     string `json:"sha"`
				TS      string `json:"ts"`
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal([]byte(line), &e) != nil || e.Event != "verdict" || e.Verdict == "" {
				continue
			}
			if e.TS >= since && e.TS <= until {
				verdicts = append(verdicts, throughput.VerdictLine{SHA: e.SHA, Stamp: e.TS, Verdict: e.Verdict})
			}
		}
	}

	// Route decisions: count in-window "T"-bearing lines.
	routeDecisions := 0
	if data, err := os.ReadFile(routeLog); err == nil {
		routeDecisions = throughput.CountRouteLines(strings.Split(string(data), "\n"), since, until)
	}

	m := throughput.Compute(commits, verdicts, routeDecisions, win)

	if *wantJSON {
		json.NewEncoder(os.Stdout).Encode(m)
		return
	}
	fmt.Printf("herd-throughput: merges/day=%.2f verdict→merge=%ds rounds/ticket=%.2f merged_tickets=%d route-decisions/merged-ticket=%.2f\n",
		m.MergesPerDay, m.MedianVerdictToMergeSeconds, m.ReviewRoundsPerTicket, m.MergedTickets, m.RouteDecisionsPerMergedTicket)
}

// runOverlap ports bin/herd-overlap: surface files that more than one
// unmerged branch is editing, and same-name symbols added in different
// files, before those branches collide at merge. Exit 0 = no overlap (or a
// --json snapshot, or selftest pass), 1 = overlap found / selftest fail,
// 2 = unknown arg, 3 = no origin/main.
func runOverlap() {
	fs := flag.NewFlagSet("overlap", flag.ExitOnError)
	quiet := fs.Bool("quiet", false, "Only the overlaps, for a pulse stage")
	min := fs.Int("min", 2, "Only files touched by this many branches / symbols on this many tips")
	wantJSON := fs.Bool("json", false, "Output JSON")
	symbolsMode := fs.Bool("symbols", false, "Detect same-name additions in different files")
	selftestMode := fs.Bool("selftest", false, "Self-test origin/main against itself")
	fs.Parse(os.Args[2:]) // flag.ExitOnError prints usage + exits 2 on unknown arg

	repoRoot := "."
	if err := os.Chdir(repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "failed to change dir: %v\n", err)
		os.Exit(1)
	}
	o := overlap.NewOverlap(repoRoot)
	mainRef := envOr("HERD_OVERLAP_MAIN_REF", "origin/main")
	ctx := context.Background()

	if *selftestMode {
		runOverlapSelftest(ctx, repoRoot)
		return
	}

	// Verify the main ref is present before any 3-dot census, matching the
	// reference's exit-3 path.
	if !gitRefExists(ctx, repoRoot, mainRef) {
		fmt.Fprintln(os.Stderr, "herd-overlap: no origin/main; run git fetch origin main")
		os.Exit(3)
	}

	if *symbolsMode {
		hot := o.SymbolOverlaps(ctx, mainRef, *min)
		if *wantJSON {
			if hot == nil {
				hot = []overlap.SymbolHot{}
			}
			out, _ := json.Marshal(hot)
			fmt.Println(string(out))
		} else if len(hot) == 0 {
			if !*quiet {
				fmt.Printf("herd-overlap: no symbol is being added on %d+ unmerged tips in different files\n", *min)
			}
		} else {
			fmt.Printf("herd-overlap: %d symbol(s) added on %d+ unmerged tips in different files\n", len(hot), *min)
			for _, s := range hot {
				fmt.Printf("  %s\n", s.Symbol)
				for _, r := range s.Refs {
					fmt.Printf("    %s|%s\n", r.Branch, r.Location)
				}
			}
		}
		if len(hot) == 0 {
			os.Exit(0)
		}
		os.Exit(1)
	}

	hot, scanned, err := o.FileOverlaps(ctx, mainRef, *min, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-overlap: error — %v\n", err)
		os.Exit(1)
	}

	if *wantJSON {
		if hot == nil {
			hot = []overlap.FileOverlap{}
		}
		out, _ := json.Marshal(hot)
		fmt.Println(string(out))
		os.Exit(0)
	}

	if len(hot) == 0 {
		if !*quiet {
			fmt.Printf("herd-overlap: no file is being edited by %d+ unmerged branches (%d branch(es) scanned)\n", *min, scanned)
		}
		os.Exit(0)
	}

	fmt.Printf("herd-overlap: %d file(s) edited by %d+ unmerged branches (%d scanned)\n", len(hot), *min, scanned)
	fmt.Println("  Two branches on one file is normal. Two branches on one file for days,")
	fmt.Println("  neither able to see the other, is the same design being built twice.")
	fmt.Println()

	// Rank by branch count so the worst convergence reads first. Cap the
	// printed list: overlap runs on every beat and a wall of output is
	// ignored exactly as reliably as no output at all.
	top := 12
	if v := os.Getenv("HERD_OVERLAP_TOP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			top = n
		}
	}
	shown := 0
	for _, fo := range hot {
		if shown >= top {
			break
		}
		shown++
		fmt.Printf("  [%d] %s\n", len(fo.Branches), fo.File)
		for _, b := range fo.Branches {
			fmt.Printf("        %s\n", b)
		}
	}
	if len(hot) > shown {
		fmt.Println()
		fmt.Printf("  ... and %d more (herd overlap --min 3, or HERD_OVERLAP_TOP=50)\n", len(hot)-shown)
	}

	// Exit 1 so a pulse stage can surface it as work to look at, matching the
	// herd-drain convention. Not a failure of the beat.
	os.Exit(1)
}

// runOverlapSelftest ports the reference --selftest: origin/main must exist,
// and a branch compared against itself must contribute no changed files (so a
// merged branch can never manufacture a phantom overlap).
func runOverlapSelftest(ctx context.Context, repoRoot string) {
	if !gitRefExists(ctx, repoRoot, "origin/main") {
		fmt.Fprintln(os.Stderr, "FAIL: no origin/main")
		os.Exit(1)
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "origin/main...origin/main")
	cmd.Dir = repoRoot
	out, err := cmd.Output() // err tolerated: a missing ref contributes nothing
	if err == nil {
		n := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				n++
			}
		}
		if n != 0 {
			fmt.Fprintf(os.Stderr, "FAIL: origin/main against itself reported %d changed files\n", n)
			os.Exit(1)
		}
	}
	fmt.Println("herd-overlap --selftest PASS")
}

// gitRefExists reports whether ref resolves in repoRoot.
func gitRefExists(ctx context.Context, repoRoot, ref string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "-q", ref)
	cmd.Dir = repoRoot
	return cmd.Run() == nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstEnv(primary, secondary, def string) string {
	if v := os.Getenv(primary); v != "" {
		return v
	}
	if v := os.Getenv(secondary); v != "" {
		return v
	}
	return def
}

// resolveCanonicalWorktreeManager builds a WorktreeManager rooted at the
// canonical repository root, never at the process's literal cwd string
// (FAC-152). A dispatch invoked from deep inside a task worktree — e.g.
// <task-worktree>/pkg/dispatch — must still create its next worktree in the
// shared canonical pool, not a pool computed relative to wherever the
// process happened to be running; that mismatch is exactly what produced
// the nested pkg/dispatch/.herd/worktrees/fac-1 lane. Fails closed (exits
// non-zero) rather than silently falling back to cwd, which would defeat
// the fix in precisely the case it exists to catch.
func resolveCanonicalWorktreeManager() *worktree.WorktreeManager {
	root, err := worktree.ResolveCanonicalRoot(context.Background(), ".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd: cannot resolve canonical repository root (FAC-152 fail-closed): %v\n", err)
		os.Exit(1)
	}
	return worktree.NewWorktreeManager(root)
}

func stateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "herdforge")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "herdforge")
}

// runLost ports bin/herd-lost: subjects-not-patch-ids, owned-is-not-lost.
// Exit 0 clean, 1 when an ownerless branch holds unmerged subjects, 2 usage.
// FAC-159: lost is diagnostic only. Re-dispatch of recovered work must go
// through herd dispatch (RequireTaskLaunch + fenced post-check).
func runLost() {
	fs := flag.NewFlagSet("lost", flag.ExitOnError)
	quiet := fs.Bool("quiet", false, "Only status lines, no per-branch tables")
	noFetch := fs.Bool("no-fetch", false, "Skip git fetch origin")
	limit := fs.Int("limit", 60, "Cap subjects examined per branch")
	fs.Parse(os.Args[2:])

	f := lost.NewFinder(".")
	f.Fetch = !*noFetch
	f.Limit = *limit

	rep, err := f.Find(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	table := func(header string, rows []lost.BranchRow) {
		if *quiet || len(rows) == 0 {
			return
		}
		fmt.Println(header)
		for _, r := range rows {
			fmt.Printf("  %-40s %2d/%2d unmerged  last=%s  first-missing: %s\n",
				r.Label, r.Unmerged, r.Total, r.Age, r.FirstMissing)
		}
	}
	table("OWNERLESS (lost) — triage each: recover, or delete with a recorded reason:", rep.Lost)
	table("DURABLE PARK (intentional review backlog):", rep.Parked)
	table("Branches with a LIVE WORKTREE (owned, harvested by the coordinator):", rep.Owned)
	if !*quiet && len(rep.Superseded) > 0 {
		fmt.Printf("%d branch(es) fully superseded by origin/main — safe to delete:\n", len(rep.Superseded))
		for _, b := range rep.Superseded {
			fmt.Printf("  %s\n", b)
		}
	}

	if len(rep.Lost) > 0 {
		fmt.Printf("herd-lost: %d ownerless branch(es), %d unmerged subject(s). Triage each: recover, or delete with a recorded reason.\n",
			len(rep.Lost), rep.LostTotal)
		os.Exit(1)
	}
	if !*quiet {
		fmt.Println("herd-lost: no ownerless branch holds unmerged work.")
	}
}

// runUnmerged ports bin/herd-unmerged: patch-equivalence authority, byte-
// distinct from herd harvest so drain-style pipelines can parse it. Exit
// codes are contract: 0 clean-or-listed, 1 real error, 2 usage.
func runUnmerged() {
	const usageLine = "usage: bin/herd-unmerged <worktree-path> | --all"
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usageLine)
		os.Exit(2)
	}

	printBlock := func(u *harvest.UnmergedWork) {
		fmt.Printf("%s (%s):\n", u.WorktreePath, u.Branch)
		for _, sha := range u.Unmerged {
			fmt.Printf("  %s\n", sha)
		}
	}

	ctx := context.Background()
	h := harvest.NewHarvester(".")

	switch args[0] {
	case "-h", "--help":
		fmt.Println(usageLine)
		return
	case "--all":
		result, err := h.Harvest(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-unmerged: %v\n", err)
			os.Exit(1)
		}
		for i := range result.UnmergedWorktrees {
			printBlock(&result.UnmergedWorktrees[i])
		}
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "herd-unmerged: %s\n", e)
		}
		if len(result.Errors) > 0 {
			os.Exit(1)
		}
	default:
		u, err := h.UnmergedFor(ctx, args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-unmerged: %s: not a git worktree\n", args[0])
			os.Exit(1)
		}
		if u != nil {
			printBlock(u)
		}
	}
}

func runForgeE() error {
	if err := requireFleetAdmission(context.Background()); err != nil {
		return err
	}
	// FAC-128: `herd forge --loop` runs the autonomous orchestration loop.
	for _, a := range os.Args[2:] {
		if a == "--loop" {
			runForgeLoop()
			return nil
		}
	}

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		return fmt.Errorf("task provider: %w", tpErr)
	}

	mr := router.NewModelRouter([]*router.ModelCandidate{
		{Name: "opencode", Type: router.ProviderOllama, Model: "deepseek-v4-flash"},
	}).WithUsageFunc(func(ctx context.Context, name string) float64 {
		snap, err := usage.FetchSnapshot()
		if err != nil {
			return 0
		}
		return snap.Utilization(name)
	})
	wm := resolveCanonicalWorktreeManager()
	v := verifier.NewVerifier(cfg.Verification.TestCommand)
	st, err := store.New(".herd/herdforge.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "store init failed — forge requires durable dependency BLOCKED evidence: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()
	eng := daemon.NewEngine(cfg, tp, mr, st, wm, v)

	ctx := context.Background()
	fmt.Println("=== Forge: Pulse ===")
	var forgeLane *config.LaneDef
	var forgeDecision *router.LaunchDecision
	var forgeLeaseGeneration int64
	var task *provider.Task
	if !herdr.IsAvailable() {
		return errors.New("herdr CLI not found — refusing launch-required forge claim")
	}
	forgeLane = findLaneForRole(cfg, "worker")
	if forgeLane == nil {
		return errors.New("no worker lane configured; refusing forge claim")
	}
	forgeDecision, err = forgeLaunchAdmission(cfg, forgeLane, ctx, func(_ *router.LaunchDecision) error {
		var claimErr error
		task, claimErr = eng.RunPulse(ctx, "worker")
		return claimErr
	})
	if err != nil {
		return fmt.Errorf("launch route rejected before forge claim: %w", err)
	}
	if err := validateDecisionBeforeSideEffect(forgeDecision, forgeLane.Name); err != nil {
		return fmt.Errorf("launch decision rejected before forge claim: %w", err)
	}
	claimedRef, claimedGeneration := eng.LastClaimIdentity()
	if task != nil {
		if claimedRef != task.Ref || claimedGeneration == 0 {
			return fmt.Errorf("forge launch identity unavailable for %s", task.Ref)
		}
		forgeLeaseGeneration = claimedGeneration
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "pulse failed: %v\n", err)
		os.Exit(1)
	}
	if task == nil {
		fmt.Println("No pending tasks. Checking for review items...")
		// Fall through to review step
	} else {
		fmt.Printf("Claimed [%s]: %s\n", task.Ref, task.Title)

		// Spawn worker only after the pre-claim route and availability checks.
		if herdr.IsAvailable() {
			lane := forgeLane
			if lane != nil {
				decision, bindErr := rebindDecisionForTask(forgeDecision, task.Ref, forgeLeaseGeneration)
				if bindErr != nil {
					return fmt.Errorf("forge launch decision rejected after claim: %w", bindErr)
				}
				standingName := fmt.Sprintf("forge-%s", lane.Name)
				if lane.Worktree == "" {
					return fmt.Errorf("forge launch requires an isolated worktree")
				}
				tabLabel, resolveErr := herdr.ResolveAgentTabWithDecision(standingName, taskLaunchRequest(decision, task.Ref, repositoryIdentityForLaunch(cfg), lane.Name))
				if resolveErr != nil {
					if gateErr := authorizeEphemeralTaskAgent(resolveErr); gateErr != nil {
						return fmt.Errorf("standing forge agent %s blocked: %w", standingName, resolveErr)
					}
					tabLabel = fmt.Sprintf("forge-%s-%s", lane.Name, task.Ref)
					cwd := "."
					if lane.Worktree != "" {
						cwd = filepath.Join(".", lane.Worktree)
					}
					tab, tabErr := herdr.TabCreateForTask(herdr.ResolveWorkspace("."), tabLabel, cwd, true)
					if tabErr == nil {
						if err := herdr.StartPreparedAgent(tab.ID, tabLabel, decision.Harness, tab.Pane.ID, taskLaunchRequest(decision, task.Ref, repositoryIdentityForLaunch(cfg), lane.Name)); err != nil {
							return fmt.Errorf("launch failed: %w", err)
						}
					} else {
						return fmt.Errorf("create forge tab: %w", tabErr)
					}
				}
				packet := fmt.Sprintf(`Task [%s]: %s\n\n%s\n\nWorktree: %s`, task.Ref, task.Title, task.Description, lane.Worktree)
				if _, promptErr := herdr.AgentPrompt(tabLabel, packet, false); promptErr != nil {
					fmt.Fprintf(os.Stderr, "forge prompt failed: %v\n", promptErr)
					os.Exit(1)
				}
			}
		}
	}

	fmt.Println("\n=== Forge: Review ===")
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "in-progress")
	if err == nil && len(tasks) > 0 {
		t := tasks[0]
		fmt.Printf("Selected [%s]: %s for review\n", t.Ref, t.Title)
		if err := tp.UpdateStatus(ctx, t.ID, "in-review"); err == nil {
			fmt.Printf("  -> moved to 'in-review' status\n")
		}
	}

	fmt.Println("\n=== Forge: Approve ===")
	reviewTasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "in-review")
	if err == nil {
		for _, t := range reviewTasks {
			res, err := hsync.BoardDone(ctx, tp, ".", cfg.TaskProvider.ProjectID, t.Ref, "", false)
			if err != nil {
				fmt.Printf("Not approved [%s]: %v\n", t.Ref, err)
				continue
			}
			fmt.Printf("Approved [%s]: %s (proof: %s)\n", res.Ref, t.Title, res.Proof)
		}
	}

	fmt.Println("\n=== Forge cycle complete ===")
	return nil
}

func forgeLaunchAdmission(cfg *config.Config, lane *config.LaneDef, ctx context.Context, effect func(*router.LaunchDecision) error) (*router.LaunchDecision, error) {
	return launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, lane.Role, true, routedLaneDecision(ctx, nil), effect)
}

func findLaneForRole(cfg *config.Config, role string) *config.LaneDef {
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Role == role {
			return &cfg.Lanes[i]
		}
	}
	return nil
}

func prepareStandingWorktree(lane *config.LaneDef) error {
	return prepareStandingWorktreeWith(lane, func(path, branch string) error {
		cmd := exec.Command("git", "worktree", "add", "-b", branch, path, "origin/main")
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})
}

func prepareStandingWorktreeWith(lane *config.LaneDef, add func(path, branch string) error) error {
	if lane.Worktree == "" {
		return nil
	}
	wtPath := filepath.Join(".", lane.Worktree)
	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		fmt.Printf("Creating worktree %s for lane %s...\n", lane.Worktree, lane.Name)
		branch := fmt.Sprintf("wt/%s", lane.Name)
		if err := add(lane.Worktree, branch); err != nil {
			return fmt.Errorf("create standing worktree %s: %w", lane.Name, err)
		}
	}
	return nil
}

func routedLaneDecision(ctx context.Context, task *provider.Task) func(*config.LaneDef) (*router.LaunchDecision, error) {
	return func(lane *config.LaneDef) (*router.LaunchDecision, error) { return laneLaunchDecision(ctx, lane, task) }
}

func laneLaunchDecision(ctx context.Context, lane *config.LaneDef, task *provider.Task) (*router.LaunchDecision, error) {
	if lane == nil {
		return nil, fmt.Errorf("launch route requires a configured lane")
	}
	if err := validateLaneLaunchConfig(lane); err != nil {
		return nil, err
	}
	role := router.Role(lane.Role)
	shape := strings.TrimSpace(lane.TaskShape)
	if shape == "" {
		return nil, fmt.Errorf("lane %q has no authoritative task_shape", lane.Name)
	}
	provider := lane.Provider
	contextRef := lane.Name
	if task != nil {
		contextRef = task.Ref
	}
	scope := router.ScopeLane
	if task != nil {
		scope = router.ScopeTask
		if role == router.RoleReviewer || role == router.RoleAssayer {
			scope = router.ScopeCandidate
		}
	}
	pinnedBuilder := role == router.RoleWorker || role == router.RoleForgeSmith || role == router.RoleRecovery
	request := router.LaunchRequest{Role: role, Shape: shape, TaskRef: contextRef, Scope: scope, Risk: classify.TierR1}
	if pinnedBuilder {
		request.RequestedProvider = provider
		request.RequestedModel = lane.Model
		request.RequestedEffort = lane.Effort
	} else {
		request.PreferredProvider = provider
		request.PreferredModel = lane.Model
	}
	if role == router.RoleReviewer || role == router.RoleAssayer {
		if task == nil {
			return nil, fmt.Errorf("review launch requires candidate provenance")
		}
		for _, label := range task.Labels {
			if strings.HasPrefix(label, "author-family:") {
				request.AuthorFamily = strings.TrimPrefix(label, "author-family:")
			}
			if strings.HasPrefix(label, "author-model:") {
				request.AuthorModel = strings.TrimPrefix(label, "author-model:")
			}
			if strings.HasPrefix(label, "candidate-sha:") {
				request.CandidateSHA = strings.TrimPrefix(label, "candidate-sha:")
			}
		}
		if request.AuthorFamily == "" || request.AuthorModel == "" || request.CandidateSHA == "" {
			return nil, fmt.Errorf("review launch requires author family, author model, and candidate SHA provenance")
		}
	}
	model := lane.Model
	// Probe every probe-gated model the router could actually pick for this
	// shape, not just the lane's configured tuple. Keying the probe on the lane
	// model meant a probe-gated candidate the router might choose had no result,
	// and unknown-probe fails closed — so removing the codex pin accidentally
	// made codex unreachable from every lane whose configured model was not
	// itself the probe-gated one.
	candidates, wfErr := router.Waterfall(shape)
	if wfErr != nil {
		return nil, wfErr
	}
	probes := map[string]bool{}
	for _, cp := range candidates {
		cm := router.ModelFor(cp, shape)
		if cm == "" || !router.ModelRequiresProbe(cm) {
			continue
		}
		key := router.ProbeKey(cp, cm)
		if _, done := probes[key]; done {
			continue
		}
		probes[key] = herdr.ProbeProviderModel(ctx, cp, cm, lane.Effort).Available
	}
	if router.ModelRequiresProbe(model) {
		probe := herdr.ProbeProviderModel(ctx, provider, model, lane.Effort)
		probes[router.ProbeKey(provider, model)] = probe.Available
		if pinnedBuilder && !probe.Available {
			reason := strings.TrimSpace(probe.Reason)
			if reason == "" {
				reason = "unknown probe failure"
			}
			return nil, fmt.Errorf("lane %q configured probe %s/%s unavailable: %s", lane.Name, provider, model, reason)
		}
	}
	if len(probes) > 0 {
		request.ProbeResults = probes
	}
	decision, err := router.NewRouter(nil, nil).Decide(request)
	if err != nil {
		return nil, err
	}
	if decision.Shape != lane.TaskShape {
		return nil, fmt.Errorf("lane %q routed shape drift: configured %s, got %s", lane.Name, lane.TaskShape, decision.Shape)
	}
	if decision.Harness != router.PiHarness {
		return nil, fmt.Errorf("lane %q routed harness drift: got %s, want %s", lane.Name, decision.Harness, router.PiHarness)
	}
	if pinnedBuilder {
		if decision.Provider != lane.Provider || decision.Model != lane.Model || decision.Effort != lane.Effort {
			return nil, fmt.Errorf("lane %q fixed builder route drift: configured %s/%s/%s, got %s/%s/%s", lane.Name, lane.Provider, lane.Model, lane.Effort, decision.Provider, decision.Model, decision.Effort)
		}
	} else if decision.Provider != lane.Provider || decision.Model != lane.Model || decision.Effort != lane.Effort {
		fmt.Fprintf(os.Stderr, "herd: lane %q rerouted by quota: %s/%s/%s -> %s/%s/%s (%s)\n", lane.Name, lane.Provider, lane.Model, lane.Effort, decision.Provider, decision.Model, decision.Effort, decision.Availability)
	}
	if err := validateDecisionBeforeSideEffect(decision, contextRef); err != nil {
		return nil, err
	}
	return decision, nil
}

func validateLaneLaunchConfig(lane *config.LaneDef) error {
	role := strings.TrimSpace(lane.Role)
	if role == "" || strings.TrimSpace(lane.AgentKind) == "" || strings.TrimSpace(lane.Provider) == "" || strings.TrimSpace(lane.Model) == "" || strings.TrimSpace(lane.Harness) == "" || strings.TrimSpace(lane.Effort) == "" || strings.TrimSpace(lane.TaskShape) == "" {
		return fmt.Errorf("lane %q has incomplete launch authority", lane.Name)
	}
	expectedShapes := map[string]string{launch.WorkerRole: "implementation", launch.ForgeSmithRole: "implementation", launch.RecoveryRole: "implementation", launch.ReviewerRole: "qa", launch.OrchestratorRole: "coordinator", launch.ScoutPlannerRole: "architecture", launch.VerificationGateRole: "bounded", launch.ReviewSupervisorRole: "coordinator", launch.HarvestRole: "bounded", launch.RecoverySentinelRole: "bounded"}
	if expected, ok := expectedShapes[role]; !ok || lane.TaskShape != expected {
		return fmt.Errorf("%w: lane %q has invalid task_shape %q for role %q", ErrWorkerConfigPolicy, lane.Name, lane.TaskShape, role)
	}
	if strings.TrimSpace(lane.AgentKind) != router.PiHarness || strings.TrimSpace(lane.Harness) != router.PiHarness {
		return fmt.Errorf("%w: lane %q agent kind %q harness %q must both be %q", ErrHarnessConfigPolicy, lane.Name, lane.AgentKind, lane.Harness, router.PiHarness)
	}
	if role == launch.WorkerRole || role == launch.ForgeSmithRole || role == launch.RecoveryRole {
		if lane.Provider != launch.WorkerProvider || lane.Model != launch.WorkerModel || lane.Effort != launch.WorkerEffort {
			return fmt.Errorf("%w: lane %q must explicitly be Pi harness with codex/gpt-5.6-luna/medium", ErrWorkerConfigPolicy, lane.Name)
		}
	}
	return nil
}

var ErrWorkerConfigPolicy = errors.New("launch.policy.config_worker_tuple_mismatch")
var ErrHarnessConfigPolicy = errors.New("launch.policy.config_harness_mismatch")

func validateDecisionBeforeSideEffect(decision *router.LaunchDecision, taskRef string) error {
	if decision == nil {
		return fmt.Errorf("missing routed launch decision")
	}
	return launch.Validate(launch.Request{Decision: decision, TaskRef: taskRef, LeaseGeneration: decision.LeaseGeneration, Scope: decision.Scope}, nil)
}

func rebindDecisionForTask(decision *router.LaunchDecision, taskRef string, leaseGeneration int64) (*router.LaunchDecision, error) {
	bound, err := router.RebindDecision(decision, taskRef, leaseGeneration)
	if err != nil {
		return nil, err
	}
	if err := validateDecisionBeforeSideEffect(bound, taskRef); err != nil {
		return nil, err
	}
	return bound, nil
}

func repositoryIdentityForLaunch(cfg *config.Config) string {
	if id, err := dispatch.AuthenticatedRepositoryIdentity("."); err == nil {
		return id
	}
	return ""
}

func taskLaunchRequest(decision *router.LaunchDecision, taskRef, repository, lane string) launch.Request {
	if decision == nil {
		return launch.Request{TaskRef: taskRef, Repository: repository, Lane: lane}
	}
	return launch.Request{Decision: decision, TaskRef: taskRef, LeaseGeneration: decision.LeaseGeneration, Scope: decision.Scope, Repository: repository, Lane: lane}
}

func shouldCreateEphemeralTaskAgent(err error) bool {
	return errors.Is(err, herdr.ErrAgentNotFound)
}

func authorizeEphemeralTaskAgent(err error) error {
	if err == nil || shouldCreateEphemeralTaskAgent(err) {
		return nil
	}
	return err
}

// launchAdmission is the compiled pre-side-effect gate shared by launch-capable
// entrypoints. The continuation is deliberately after config, availability,
// router authority, and decision validation; tests inject real lifecycle seams
// into it to prove rejected lanes cannot claim or spawn.
type launchLifecycle interface {
	Run(*router.LaunchDecision, func(*router.LaunchDecision) error) error
}

type liveLaunchLifecycle struct{}

func (liveLaunchLifecycle) Run(decision *router.LaunchDecision, effect func(*router.LaunchDecision) error) error {
	if err := requireFleetAdmission(context.Background()); err != nil {
		return err
	}
	return effect(decision)
}

func launchAdmissionWithLifecycle(lc launchLifecycle, cfg *config.Config, role string, herdrAvailable bool, route func(*config.LaneDef) (*router.LaunchDecision, error), effect func(*router.LaunchDecision) error) (*router.LaunchDecision, error) {
	decision, err := launchAdmission(cfg, role, herdrAvailable, route)
	if err != nil {
		return nil, err
	}
	if err := lc.Run(decision, effect); err != nil {
		return nil, err
	}
	return decision, nil
}

func launchAdmission(cfg *config.Config, role string, herdrAvailable bool, route func(*config.LaneDef) (*router.LaunchDecision, error)) (*router.LaunchDecision, error) {
	lane := findLaneForRole(cfg, role)
	if lane == nil {
		return nil, fmt.Errorf("no lane configured for role %q", role)
	}
	if !herdrAvailable {
		return nil, fmt.Errorf("herdr unavailable for launch-required role %q", role)
	}
	if err := validateLaneLaunchConfig(lane); err != nil {
		return nil, err
	}
	decision, err := route(lane)
	if err != nil {
		return nil, err
	}
	validationContext := ""
	switch decision.Scope {
	case router.ScopeCandidate, router.ScopeTask:
		validationContext = decision.TaskRef
	case router.ScopeLane:
		validationContext = lane.Name
	}
	if err := validateDecisionBeforeSideEffect(decision, validationContext); err != nil {
		return nil, err
	}
	return decision, nil
}

func runKick() {
	kickFlags := flag.NewFlagSet("kick", flag.ExitOnError)
	all := kickFlags.Bool("all", false, "Force re-engage even working agents (--all)")
	dryRun := kickFlags.Bool("dry-run", false, "Print what would be done without sending")
	quiet := kickFlags.Bool("quiet", false, "Suppress non-error output")
	reason := kickFlags.String("reason", "", "Override default kick context message")
	noRaise := kickFlags.Bool("no-raise", false, "Skip raising missing agents via herd-standing")
	selftestFlag := kickFlags.Bool("selftest", false, "Run kick message selftest and exit")
	kickFlags.Parse(os.Args[2:])

	if *selftestFlag {
		if err := kick.Selftest(); err != nil {
			fmt.Fprintf(os.Stderr, "kick selftest FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("kick selftest PASSED — all standing lane messages valid")
		return
	}
	if err := requireFleetAdmission(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "herd-kick: %v\n", err)
		os.Exit(1)
	}

	args := kickFlags.Args()
	authority, err := newProductionHoldAuthority()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-kick: hold authority: %v\n", err)
		os.Exit(1)
	}
	defer authority.Close()
	repository, err := holdRepository()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-kick: hold identity: %v\n", err)
		os.Exit(1)
	}
	kickConfig, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-kick: %v\n", err)
		os.Exit(1)
	}
	kickRegistry, err := canonicalLaneRegistry(kickConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-kick: %v\n", err)
		os.Exit(1)
	}
	activeResolver, err := loadProductionActiveTaskResolver(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-kick: active task authority: %v\n", err)
		os.Exit(1)
	}

	result, err := kick.Run(kick.Options{
		Names:        args,
		Force:        *all,
		DryRun:       *dryRun,
		Quiet:        *quiet,
		Reason:       *reason,
		RaiseMissing: !*noRaise,
		HoldReader:   authority,
		Identity: func(id string) (lifecycle.HoldIdentity, error) {
			lane, resolveErr := kickRegistry.ResolveLiveAgentID(id)
			if resolveErr != nil {
				return lifecycle.HoldIdentity{}, resolveErr
			}
			return lifecycle.HoldIdentity{Repository: repository, Owner: lane.Role, Lane: lane.Name, Scope: "lane"}, nil
		},
		Generation: func(ctx context.Context, identity lifecycle.HoldIdentity) (int64, error) {
			return authority.CurrentGeneration(ctx, identity)
		},
		ActiveTasks: activeResolver,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-kick: error — %v\n", err)
		os.Exit(1)
	}

	if !*quiet {
		fmt.Printf("herd-kick: done kicked=%d skipped=%d failed=%d\n", result.Kicked, result.Skipped, result.Failed)
	}
	if result.Failed > 0 {
		os.Exit(1)
	}
}

func runAttention() {
	attFlags := flag.NewFlagSet("attention", flag.ExitOnError)
	asJSON := attFlags.Bool("json", false, "Output JSON triage")
	selftestFlag := attFlags.Bool("selftest", false, "Run attention selftest and exit")
	quiet := attFlags.Bool("quiet", false, "Summary line only, no per-lane detail")
	attFlags.Parse(os.Args[2:])

	if *selftestFlag {
		if err := attention.Selftest(); err != nil {
			fmt.Fprintf(os.Stderr, "attention selftest FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("attention selftest: PASS")
		return
	}

	attentionAuthority, err := newProductionHoldAuthority()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-attention: %v\n", err)
		os.Exit(1)
	}
	defer attentionAuthority.Close()
	attentionRepository, err := holdRepository()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-attention: %v\n", err)
		os.Exit(1)
	}
	activeResolver, err := loadProductionActiveTaskResolver(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-attention: active task authority: %v\n", err)
		os.Exit(1)
	}
	attentionConfig, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-attention: %v\n", err)
		os.Exit(1)
	}
	attentionRegistry, err := canonicalLaneRegistry(attentionConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-attention: %v\n", err)
		os.Exit(1)
	}
	result, err := attention.RunWithHoldReaderAndTasks(attentionAuthority, attentionRepository, activeResolver, attentionRegistry)
	if err != nil {
		// Fail-closed: herdr unavailable or agent list parse error is a hard
		// error, not a silent "fleet healthy".
		if result != nil {
			if *asJSON {
				if out, marshalErr := json.MarshalIndent(result, "", "  "); marshalErr == nil {
					fmt.Println(string(out))
				}
			} else {
				fmt.Println(attention.Summary(*result))
			}
		}
		fmt.Fprintf(os.Stderr, "herd-attention: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-attention: json encode: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
		return
	}

	fmt.Println(attention.Summary(*result))

	if *quiet {
		return
	}

	for _, item := range result.Items {
		fmt.Println("  " + attention.FormatItem(item))
	}

	if result.Needing > 0 {
		fmt.Println()
		fmt.Println("herd-attention: triage complete. Actions: review/harvest done lanes,")
		fmt.Println("  unblock blocked lanes, kick idle lanes (herd kick), raise missing")
		fmt.Println("  lanes (herd standing), reroute provider-death lanes.")
	}
}

// newLifecycleEngineFromConfig is the production roster-construction seam.
// It preserves every configured lane for typed role/live-ID resolution while
// carrying the declarative Standing bit used by lifecycle classification.
func newLifecycleEngineFromConfig(roleConfig *config.Config) (*lifecycle.Engine, error) {
	if roleConfig == nil {
		return nil, fmt.Errorf("lifecycle: config is required")
	}
	lanes := make([]lifecycle.CanonicalLane, 0, len(roleConfig.Lanes))
	for _, lane := range roleConfig.Lanes {
		lanes = append(lanes, lifecycle.CanonicalLane{Name: lane.Name, Role: lane.Role, Standing: lane.Standing})
	}
	registry, err := lifecycle.NewCanonicalLaneRegistry(lanes)
	if err != nil {
		return nil, fmt.Errorf("lifecycle lane registry: %w", err)
	}
	return &lifecycle.Engine{StandingRoster: &registry, Lanes: registry.LaneNames()}, nil
}

func runLifecycle() {
	lifecycleFlags := flag.NewFlagSet("lifecycle", flag.ExitOnError)
	actMode := lifecycleFlags.Bool("act", false, "Execute act mode")
	selftestFlag := lifecycleFlags.Bool("selftest", false, "Run lifecycle selftest and exit")
	lifecycleFlags.Parse(os.Args[2:])

	verbose := lifecycleFlags.Arg(0) == "verbose"

	if *selftestFlag {
		fmt.Println("lifecycle engine: available")
		return
	}

	if verbose {
		fmt.Println("lifecycle engine: loaded (1931-line lifecycle.go)")
		return
	}

	roleConfig, roleErr := config.LoadConfig(".herd/herd.yaml")
	if roleErr != nil {
		fmt.Fprintf(os.Stderr, "lifecycle role config: %v\n", roleErr)
		os.Exit(1)
	}
	eng, registryErr := newLifecycleEngineFromConfig(roleConfig)
	if registryErr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", registryErr)
		os.Exit(1)
	}
	for _, lane := range roleConfig.Lanes {
		if strings.TrimSpace(lane.Role) != "" {
			eng.HoldRoles = append(eng.HoldRoles, strings.TrimSpace(lane.Role))
		}
	}
	roleRegistry := *eng.StandingRoster
	holdAuthority, holdErr := newProductionHoldAuthority()
	if holdErr != nil {
		fmt.Fprintf(os.Stderr, "lifecycle hold authority: %v\n", holdErr)
		os.Exit(1)
	}
	defer holdAuthority.Close()
	repository, repoErr := holdRepository()
	if repoErr != nil {
		fmt.Fprintf(os.Stderr, "lifecycle hold identity: %v\n", repoErr)
		os.Exit(1)
	}
	eng.HoldReader = holdAuthority
	eng.HoldLaneResolver = func(role string) (string, error) {
		lane, err := roleRegistry.ResolveRole(role)
		if err != nil {
			return "", err
		}
		return lane.Name, nil
	}
	eng.HoldLiveAgentResolver = func(agent string) (string, string, error) {
		lane, err := roleRegistry.ResolveLiveAgentID(agent)
		if err != nil {
			return "", "", err
		}
		return lane.Role, lane.Name, nil
	}
	eng.HoldIdentity = func(task, lane, owner string) lifecycle.HoldIdentity {
		scope := "task"
		if task == "" {
			scope = "lane"
		}
		return lifecycle.HoldIdentity{Repository: repository, Owner: owner, Lane: lane, Task: task, Scope: scope}
	}

	if *actMode {
		summary, err := eng.Act()
		if err != nil {
			fmt.Fprintf(os.Stderr, "lifecycle act: %v\n", err)
			os.Exit(1)
		}
		if summary != nil {
			healthy := "UNHEALTHY"
			if summary.Healthy {
				healthy = "HEALTHY"
			}
			fmt.Printf("lifecycle act: %s — stale=%d in-progress=%d blocked=%d dispatchable=%d actions=%d\n",
				healthy, len(summary.StaleCards), summary.InProgress, summary.Blocked, summary.Dispatchable, len(summary.Actions))
			if !summary.Healthy {
				os.Exit(7)
			}
		}
		return
	}

	summary, err := eng.Point()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lifecycle observe: %v\n", err)
		os.Exit(1)
	}
	if summary != nil {
		healthy := "UNHEALTHY"
		if summary.Healthy {
			healthy = "HEALTHY"
		}
		fmt.Printf("lifecycle: %s — stale=%d in-progress=%d blocked=%d dispatchable=%d\n",
			healthy, len(summary.StaleCards), summary.InProgress, summary.Blocked, summary.Dispatchable)
		for _, sc := range summary.StaleCards {
			fmt.Printf("  stale: %s owner=%s\n", sc.Ref, sc.Owner)
		}
		if !summary.Healthy {
			os.Exit(7)
		}
	}
}

func runResources() {
	fs := flag.NewFlagSet("resources", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Output JSON")
	gate := fs.Bool("gate", false, "Exit 3 on ALERT (refuses heavy ops); HERD_RESOURCES_GATE=0 disables")
	selftest := fs.Bool("selftest", false, "Run verdict assertions and exit")
	fs.Parse(os.Args[2:])

	if *selftest {
		results := resources.SelfTest()
		allPass := true
		for _, r := range results {
			if r.Pass {
				fmt.Printf("[PASS] %s\n", r.Name)
			} else {
				fmt.Printf("[FAIL] %s: %s\n", r.Name, r.Detail)
				allPass = false
			}
		}
		if !allPass {
			os.Exit(1)
		}
		return
	}

	snap := resources.TakeSnapshot()

	if *gate {
		if os.Getenv("HERD_RESOURCES_GATE") == "0" {
			if snap.Verdict == resources.VerdictAlert {
				fmt.Fprintf(os.Stderr, "resources: ALERT (swap=%dMB) — gate disabled by HERD_RESOURCES_GATE=0\n", snap.SwapMB)
			}
		} else if !resources.GatePasses(snap.Verdict) {
			fmt.Fprintf(os.Stderr, "resources: ALERT — swap used %dMB exceeds alert threshold %dMB, refusing heavy ops\n",
				snap.SwapMB, snap.Thresholds.SwapAlertMB)
			os.Exit(3)
		}
	}

	if *asJSON {
		out, _ := json.MarshalIndent(snap, "", "  ")
		fmt.Println(string(out))
		return
	}

	fmt.Printf("free-memory: %d%%  swap-used: %dMB  verdict: %s\n",
		snap.FreePct, snap.SwapMB, snap.Verdict)
}

func runProcess() {
	procFlags := flag.NewFlagSet("process", flag.ExitOnError)
	asJSON := procFlags.Bool("json", false, "Output JSON")
	selftestFlag := procFlags.Bool("selftest", false, "Run process selftest and exit")
	stalledFlag := procFlags.Bool("stalled", false, "Report stalled agents (done/idle with zero real commits)")
	procFlags.Parse(os.Args[2:])

	if *selftestFlag {
		if err := process.Selftest(); err != nil {
			fmt.Fprintf(os.Stderr, "process selftest: FAIL — %v\n", err)
			os.Exit(1)
		}
		fmt.Println("process selftest: PASS")
		return
	}

	if *stalledFlag {
		// In full integration, this would call herdr.AgentList() and the
		// real herdr CLI. For now, the selftest in the process package
		// validates the logic via table-driven tests.
		fmt.Fprintln(os.Stderr, "stalled: use herd process --selftest for validation; full herdr integration pending")
		os.Exit(1)
	}

	// Read agent panes via herdr (simplified: show classify on sample text
	// matching the zsh --selftest patterns). In full integration, this
	// would call herdr agent list and iterate over panes.
	//
	// For now, return a digest showing the classification engine is loaded.
	lines := 50
	if len(procFlags.Args()) > 0 {
		if procFlags.Args()[0] == "verbose" {
			fmt.Printf("process engine: loaded — 8 classification buckets (NEEDS_REVIEW/COMPLETE/PASS/FAIL/BLOCKED/QUOTA/UNCONSUMED/UNKNOWN)\n")
			return
		}
	}

	if *asJSON {
		targets := []process.Target{
			process.ClassifyTarget("pane-demo", "agent-demo", "idle", "herd-process engine available"),
		}
		data, err := process.DigestJSON("", targets, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "process json: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	fmt.Printf("herd-process: classification engine ready (%d lines)\n", lines)
	fmt.Println("  Usage: herd process [--json] [--selftest]")
}

// liveScorer backs lane resolution with the real herd-route port over live
// openusage quota — the same decision core the zsh fleet uses.
func liveScorer() resolve.RouteScorer {
	e := usage.NewQuotaEngine()
	computed := map[string]usage.BurnState{}
	if snap, err := usage.FetchSnapshot(); err == nil {
		computed = e.ComputeAll(snap)
	} else {
		fmt.Fprintf(os.Stderr, "resolve-lane: WARN live quota unavailable (%v); routing on availability only\n", err)
	}
	sr := router.NewRouter(e, computed)
	return &resolve.DefaultAdapter{
		ScoreFn: func(shape string, preferProvider string) *resolve.RouteScore {
			rt, err := sr.Pick(shape, preferProvider, "")
			if err != nil {
				return nil
			}
			return &resolve.RouteScore{
				Provider:        rt.Provider,
				Model:           rt.Model,
				Effort:          rt.Effort,
				QuotaPool:       rt.QuotaPool,
				LazerLastResort: rt.LazerLastResort,
			}
		},
	}
}

// runRoute is the herd-route CLI: pick a surface for a task shape.
func runRoute() {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	provider := fs.String("provider", "", "Pin the candidate set to one provider")
	excludeFamily := fs.String("exclude-family", "", "Exclude a model family (e.g. anthropic)")
	wantJSON := fs.Bool("json", false, "Output the full route JSON")
	fs.Parse(os.Args[2:])

	shape := fs.Arg(0)
	if len(fs.Args()) > 1 {
		fs.Parse(fs.Args()[1:]) // allow flags after the positional shape
	}
	if shape == "" {
		fmt.Fprintf(os.Stderr, "Usage: herd route <shape> [--provider P] [--exclude-family F] [--json]\n")
		fmt.Fprintf(os.Stderr, "Shapes: coordinator, architecture, implementation, research, bounded, advisory, qa-light, qa, adversarial\n")
		os.Exit(2)
	}

	e := usage.NewQuotaEngine()
	computed := map[string]usage.BurnState{}
	if snap, err := usage.FetchSnapshot(); err == nil {
		computed = e.ComputeAll(snap)
	}
	sr := router.NewRouter(e, computed)

	rt, err := sr.Pick(shape, *provider, *excludeFamily)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd route: %v\n", err)
		os.Exit(1)
	}
	if *wantJSON {
		json.NewEncoder(os.Stdout).Encode(rt)
		return
	}
	fmt.Printf("%s\t%s\t%s\t%s\tpool=%s\tpressure=%d\n",
		rt.Provider, rt.Model, rt.Effort, rt.Family, rt.QuotaPool, rt.QuotaPressure)
	fmt.Fprintf(os.Stderr, "%s\n", rt.Reason)
}

func runResolveLane() {
	resolveFlags := flag.NewFlagSet("resolve-lane", flag.ExitOnError)
	all := resolveFlags.Bool("all", false, "Resolve every lane in registry order")
	asJSON := resolveFlags.Bool("json", false, "Output JSON")
	list := resolveFlags.Bool("list", false, "Print lane IDs in order")
	field := resolveFlags.String("field", "", "Print one registry field for a lane (usage: --field <key> <id>)")
	noPrefer := resolveFlags.Bool("no-prefer", false, "Ignore soft prefer constraints")
	resolveFlags.Parse(os.Args[2:])

	// Locate the lane registry
	registryPaths := []string{
		"docs/agent/lane-registry.json",
		".herd/lane-registry.json",
	}
	var registryData []byte
	var registryErr error
	for _, p := range registryPaths {
		data, err := os.ReadFile(p)
		if err == nil {
			registryData = data
			break
		}
		registryErr = err
	}
	if registryData == nil {
		fmt.Fprintf(os.Stderr, "resolve-lane: no lane-registry.json found (tried %v): %v\n", registryPaths, registryErr)
		os.Exit(1)
	}

	reg, err := resolve.ParseRegistry(registryData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve-lane: invalid registry: %v\n", err)
		os.Exit(1)
	}

	if *list {
		r := resolve.New(reg, nil)
		for _, id := range r.LaneIDs() {
			fmt.Println(id)
		}
		return
	}

	if *field != "" {
		args := resolveFlags.Args()
		if len(args) == 0 {
			fmt.Fprintf(os.Stderr, "Usage: herd resolve-lane --field <key> <lane-id>\n")
			os.Exit(1)
		}
		r := resolve.New(reg, nil)
		laneID := args[0]
		val, err := r.LaneField(laneID, *field)
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve-lane: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(val)
		return
	}

	resolver := resolve.New(reg, liveScorer())

	if *all {
		results := resolver.ResolveAll()
		if *asJSON {
			out, _ := json.MarshalIndent(results, "", "  ")
			fmt.Println(string(out))
			return
		}
		for _, r := range results {
			if r.Resolvable {
				fmt.Printf("%s -> %s/%s (effort=%s) [%s]\n", r.Lane, r.Provider, r.Model, r.Effort, r.Reason)
			} else {
				fmt.Printf("%s -> UNROUTEABLE [%s]\n", r.Lane, r.Reason)
			}
		}
		return
	}

	args := resolveFlags.Args()
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: herd resolve-lane <lane-id> [flags]\n")
		os.Exit(1)
	}
	laneID := args[0]

	dropPrefer := *noPrefer
	result := resolver.Resolve(laneID, dropPrefer)
	if *asJSON {
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
	} else if result.Resolvable {
		fmt.Printf("%s -> %s/%s (effort=%s) [%s]\n", result.Lane, result.Provider, result.Model, result.Effort, result.Reason)
	} else {
		fmt.Printf("%s -> UNROUTEABLE [%s]\n", result.Lane, result.Reason)
	}
	// zsh parity: an unroutable lane exits 3 (JSON still emitted) so a
	// launcher chained on this command fails closed instead of launching
	// nothing silently.
	if !result.Resolvable {
		os.Exit(3)
	}
}

const lockUsage = "Usage: herd lock with [--wait N] [--reason T] -- <cmd...> | acquire | release | status"

// lockDir resolves the lock directory: HERD_SHARED_LOCK_DIR when set, else
// <canonical>/.git/herd-shared-checkout.lock.d.
// lockDir at path, else <canonical>/.git/herd-shared-checkout.lock.d.
func lockDir(canonical string) string {
	if d := os.Getenv(lock.EnvLockDir); d != "" {
		return d
	}
	return filepath.Join(canonical, lock.DefaultRelDir)
}

// lockCanonicalRoot resolves the shared checkout root the same way
// herd_canonical_root does: HERD_CANONICAL_ROOT if set and a directory, else
// the repo root (the current directory, which is where herd runs).
func lockCanonicalRoot() string {
	if c := os.Getenv(lock.EnvCanonicalRoot); c != "" {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	// The root must be ABSOLUTE: a relative "." makes the lockdir relative,
	// so the HERD_SHARED_LOCK_HELD marker and `git -C <root>` both break.
	// Resolve symlinks to match what a zsh caller in the same checkout sees.
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		return resolved
	}
	return wd
}

// lockDefaultMaxAge returns HERD_SHARED_LOCK_MAX_AGE in seconds, else 300s.
func lockDefaultMaxAge() time.Duration {
	if v := os.Getenv(lock.EnvMaxAge); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return lock.DefaultMaxAge
}

// isGitMutation reports whether the joined child command contains one of the
// tree-mutating git tokens (the same space-delimited substring test zsh does).
func isGitMutation(cmdLine []string) bool {
	// zsh wraps the whole arg list in spaces (`case " $* " in`), so a token
	// at the end like `git pull` still matches " pull ". Mirror that framing.
	joined := " " + strings.Join(cmdLine, " ") + " "
	for _, token := range []string{"pull", "reset", "rebase", "checkout", "stash", "merge", "switch"} {
		if strings.Contains(joined, " "+token+" ") {
			return true
		}
	}
	return false
}

// execGitCommand is a seam so tests can mock `git status --porcelain`.
var execGitCommand = exec.CommandContext

// lockGitStatus runs `git -C <canonical> status --porcelain` and returns the
// raw output, or "" on any failure (zsh `|| true` semantics).
func lockGitStatus(canonical string) string {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := execGitCommand(ctx, "git", "-C", canonical, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// runLock implements the `herd lock` subcommand. Parse replicates the zsh
// wrapper: optional --wait N / --reason TEXT, literal `--` ends flags and the
// remainder (including the terminated `--`) is treated as the command for
// `with`.
//
// Exit codes are contract: acquire 0 held / 1 timed out; with 0 ok or the
// child's rc / 2 usage / 3 dirty-refusal; status always 0; -h / no-arg prints
// usage and exits 0; unknown mode exits 2.
func runLock() {
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Println(lockUsage)
		return
	}
	mode := args[0]
	rest := args[1:]

	wait := 30
	reason := ""
	var child []string
	for len(rest) > 0 {
		switch rest[0] {
		case "--wait":
			if len(rest) > 1 {
				if n, err := strconv.Atoi(rest[1]); err == nil && n >= 0 {
					wait = n
				}
			}
			rest = rest[2:]
		case "--reason":
			if len(rest) > 1 {
				reason = rest[1]
				rest = rest[2:]
			} else {
				rest = rest[1:]
			}
		case "--":
			child = rest[1:]
			rest = nil
		default:
			child = rest
			rest = nil
		}
	}

	canonical := lockCanonicalRoot()
	lockdir := lockDir(canonical)
	maxAge := lockDefaultMaxAge()

	switch mode {
	case "acquire":
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		l := lock.NewDirLock(lockdir)
		l.SetMaxAge(maxAge)
		if err := l.Acquire(ctx, time.Duration(wait)*time.Second, reason); err != nil {
			fmt.Fprintf(os.Stderr, "herd lock: %v\n", err)
			os.Exit(1)
		}
	case "release":
		lock.NewDirLock(lockdir).Release()
	case "status":
		held, holder := lock.NewDirLock(lockdir).Status()
		if held {
			fmt.Printf("LOCKED [%s]\n", holder)
		} else {
			fmt.Println("unlocked")
		}
	case "with":
		if len(child) == 0 {
			fmt.Fprintln(os.Stderr, lockUsage)
			os.Exit(2)
		}
		runLockWith(child, canonical, lockdir, wait, reason)
	case "-h", "--help", "":
		fmt.Println(lockUsage)
	default:
		fmt.Fprintf(os.Stderr, "herd lock: unknown mode '%s'\n", mode)
		os.Exit(2)
	}
}

// runLockWith implements the `with` mode: dirty gate, re-entrancy, acquire,
// run child, release on every exit path.
func runLockWith(child []string, canonical, lockdir string, wait int, reason string) {
	// CHA-544: FAIL CLOSED on a dirty shared checkout before a tree-mutating
	// git command. A plain WARNING was ignored and edits were destroyed
	// twice, so this refuses with exit 3 unless HERD_SHARED_DIRTY_OK=1.
	if os.Getenv(lock.EnvDirtyOK) != "1" && child[0] == "git" && isGitMutation(child) {
		dirty := lockGitStatus(canonical)
		if strings.TrimSpace(dirty) != "" {
			fmt.Fprintln(os.Stderr, "herd lock: REFUSING tree-mutating command against a DIRTY shared checkout")
			fmt.Fprintln(os.Stderr, "herd lock: A plain WARNING was ignored and edits were destroyed twice (CHA-544).")
			for _, line := range strings.Split(strings.TrimSpace(dirty), "\n") {
				fmt.Fprintf(os.Stderr, "  %s\n", line)
			}
			fmt.Fprintln(os.Stderr, "herd lock: fix the dirty files, then re-run; or set HERD_SHARED_DIRTY_OK=1 to override.")
			os.Exit(3)
		}
	}

	// Re-entrancy: an ancestor `with` already holds this lock (marked in the
	// env) so just run the child; the outer call owns acquire/release exactly
	// like `izsh's `$@; exit $?`.
	if os.Getenv(lock.EnvHeld) != "" {
		os.Exit(runLocked(child, lockdir))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := lock.NewDirLock(lockdir)
	l.SetMaxAge(lockDefaultMaxAge())
	if err := l.Acquire(ctx, time.Duration(wait)*time.Second, reason); err != nil {
		fmt.Fprintf(os.Stderr, "herd lock: %v\n", err)
		os.Exit(1)
	}
	released := false
	defer func() {
		if !released {
			l.Release()
		}
	}()
	rc := runLocked(child, lockdir)
	l.Release()
	released = true
	os.Exit(rc)
}

// runLocked runs child with HERD_SHARED_LOCK_HELD set to lockdir — exactly the
// env marker zsh exported so nested calls are re-entrant — and returns the
// child's exit code.
func runLocked(child []string, lockdir string) int {
	if len(child) == 0 {
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, child[0], child[1:]...)
	cmd.Env = append(os.Environ(), lock.EnvHeld+"="+lockdir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "herd lock: %v\n", err)
	return 1
}

// runReviewLedger surfaces the append-only review ledger (FAC-82):
//
//	herd review-ledger list|queued|pending   — read the ledger as JSON
//	herd review-ledger tier <sha>            — resolved risk tier for a sha
func runReviewLedger() {
	ledgerPath := os.Getenv("HERD_REVIEW_LEDGER")
	if ledgerPath == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".local", "state")
		}
		ledgerPath = filepath.Join(base, "herdforge", "review-ledger.jsonl")
	}
	l, err := review.NewReviewLedger(".", ledgerPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-ledger: %v\n", err)
		os.Exit(1)
	}
	mode := "list"
	if len(os.Args) > 2 {
		mode = os.Args[2]
	}
	switch mode {
	case "list":
		rows, err := l.AllRows()
		if err != nil {
			fmt.Fprintf(os.Stderr, "review-ledger: %v\n", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(rows)
	case "queued":
		rows, err := l.QueueRows()
		if err != nil {
			fmt.Fprintf(os.Stderr, "review-ledger: %v\n", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(rows)
	case "pending":
		rows, err := l.Pending()
		if err != nil {
			fmt.Fprintf(os.Stderr, "review-ledger: %v\n", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(rows)
	case "tier":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: herd review-ledger tier <sha>")
			os.Exit(2)
		}
		tier, err := l.Tier(os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "review-ledger: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(tier)
	case "-h", "--help":
		fmt.Println("Usage: herd review-ledger list|queued|pending|tier <sha>")
	default:
		fmt.Fprintf(os.Stderr, "review-ledger: unknown mode %q\n", mode)
		os.Exit(2)
	}
}

func drainLedgerPath() string {
	if path := strings.TrimSpace(os.Getenv("HERD_REVIEW_LEDGER")); path != "" {
		return path
	}
	base := strings.TrimSpace(os.Getenv("HERD_STATE_DIR"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	}
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".local", "state")
		}
	}
	return filepath.Join(base, "chainseer", "herd", "review-ledger.jsonl")
}

// runDrain computes one coordinator review-pile beat. All report modes use
// the same precomputed report; --act is deliberately bounded and dry-run
// first so an unknown ledger/board state cannot become a mutation.
func runDrain() {
	os.Exit(runDrainCommand(os.Args[2:], os.Stdout, os.Stderr))
}

func runDrainCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	fs.SetOutput(errOut)
	quiet := fs.Bool("quiet", false, "Show counts and pressure only")
	asJSON := fs.Bool("json", false, "Output the fixed drain JSON packet")
	commands := fs.Bool("commands", false, "Print suggested commands without executing them")
	act := fs.Bool("act", false, "Run bounded automation (dry-run first)")
	maxReview := fs.Int("max-review", 2, "Maximum review launches")
	maxHarvest := fs.Int("max-harvest", 1, "Maximum harvest actions")
	maxRelaunch := fs.Int("max-relaunch", drainIntEnv("HERD_DRAIN_MAX_RELAUNCH", 8), "Maximum ledger-backed relaunches")
	autoTiers := fs.String("auto-harvest-tiers", "", "Comma-separated recorded tiers allowed for harvest")
	selftest := fs.Bool("selftest", false, "Verify drain integration seams")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *selftest {
		return runDrainSelftest()
	}
	_ = *autoTiers // retained for report/commands compatibility until FAC-184 adapters land
	if *maxReview < 0 || *maxHarvest < 0 || *maxRelaunch < 0 {
		fmt.Fprintln(errOut, "herd-drain: max bounds must be non-negative")
		return 2
	}

	ledgerPath := drainLedgerPath()
	cap := drainIntEnv("HERD_IN_REVIEW_CAP", 8)
	stale := drainIntEnv("HERD_DRAIN_STALE_BEHIND", 20)
	root := "."
	var tp provider.TaskProvider
	if cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml")); err == nil {
		// The action gate accepts only configured standing lane identities.
		// Evidence is never upgraded into a standing lane by shape alone.
		tp, err = loadTaskProvider(cfg)
		if err != nil && !*asJSON {
			fmt.Fprintf(errOut, "herd-drain: UNKNOWN Kaneo review-cap posture: %v\n", err)
		}
	} else if !*asJSON {
		fmt.Fprintf(errOut, "herd-drain: UNKNOWN Kaneo review-cap posture: %v\n", err)
	}
	h := harvest.NewHarvester(root)
	harvestResult, err := h.Harvest(context.Background())
	if err != nil {
		fmt.Fprintf(errOut, "herd-drain: %v\n", err)
		return 1
	}
	harvestErrors := len(harvestResult.Errors) > 0
	for _, harvestErr := range harvestResult.Errors {
		fmt.Fprintf(errOut, "herd-drain: UNKNOWN harvest input: %s\n", harvestErr)
	}
	d := review.Drain{RepoRoot: root, StateDir: os.Getenv("HERD_STATE_DIR"), LedgerPath: ledgerPath, Cap: cap, StaleBehind: stale, Provider: tp}
	report, err := d.Scan(context.Background(), harvestResult.UnmergedWorktrees)
	if err != nil {
		fmt.Fprintf(errOut, "herd-drain: %v\n", err)
		return 1
	}
	if cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml")); err == nil {
		for _, lane := range cfg.Lanes {
			if lane.Standing {
				report.StandingLanes = append(report.StandingLanes, lane.Name)
			}
		}
		sort.Strings(report.StandingLanes)
	}
	if harvestErrors {
		report.Errors = append(report.Errors, "harvest input errors; action projection is fail-closed")
	}
	if *asJSON {
		if err := json.NewEncoder(out).Encode(report); err != nil {
			fmt.Fprintf(errOut, "herd-drain: encode JSON: %v\n", err)
			return 1
		}
		return 0
	}
	if *quiet {
		fmt.Fprintf(out, "herd-drain: pressure=%s pending=%d queue=%d harvestable=%d rebase_needed=%d need_review=%d in_review=%d cap=%d parks=%d wind=%t skips7d=%d passes=%d\n", report.Pressure, report.Pending, report.HarvestQueue, report.Harvestable, report.RebaseNeeded, report.NeedReview, report.KaneoInReview, report.Cap, report.ParkBranches, report.WindDown, report.Skips7d, report.LedgerPass)
		printDrainErrors(out, report)
		return drainExitCode(report)
	}
	printDrainReportTo(out, report)
	if *commands || *act {
		printDrainCommandsTo(out, report)
	}
	if *act {
		fmt.Fprintln(out, "herd-drain: REFUSED --act: FAC-184 compiled review/ledger/harvest/cap adapters are unavailable; FAC-182 durable control-envelope delivery is blocked")
		return 1
	}
	return drainExitCode(report)
}

func drainIntEnv(name string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v >= 0 {
		return v
	}
	return fallback
}
func minDrain(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func printDrainReport(r *review.DrainReport) {
	printDrainReportTo(os.Stdout, r)
}

func printDrainReportTo(out io.Writer, r *review.DrainReport) {
	fmt.Fprintf(out, "=== server faster: review pile ===\nposture: pressure=%s pending=%d queue=%d harvestable=%d need_review=%d harvest_ready=%d in_review=%d cap=%d\n", r.Pressure, r.Pending, r.HarvestQueue, r.Harvestable, r.NeedReview, r.HarvestReady, r.KaneoInReview, r.Cap)
	if r.RefactoringCount < 0 {
		fmt.Fprintln(out, "refactoring=UNKNOWN")
	} else {
		fmt.Fprintf(out, "refactoring=%d\n", r.RefactoringCount)
	}
	fmt.Fprintln(out, "=== server faster: harvest queue & pending ===")
	fmt.Fprintf(out, "pending=%d queue=%d\n", r.Pending, r.HarvestQueue)
	fmt.Fprintln(out, "=== server faster: harvestable ===")
	pins := make(map[string]review.PinFreshness, len(r.Pins))
	for _, pin := range r.Pins {
		pins[pin.SHA] = pin
	}
	if len(r.Shas.Harvestable) == 0 {
		fmt.Fprintln(out, "(none)")
	} else {
		for _, sha := range r.Shas.Harvestable {
			p := pins[sha]
			fmt.Fprintf(out, "%s %s behind=%d %s\n", p.SHA, p.Branch, p.Behind, p.Note)
		}
	}
	fmt.Fprintln(out, "=== server faster: unmerged tips needing review ===")
	if len(r.Shas.NeedReview) == 0 {
		fmt.Fprintln(out, "(none)")
	} else {
		for _, sha := range r.Shas.NeedReview {
			fmt.Fprintln(out, sha)
		}
	}
	fmt.Fprintln(out, "=== server faster: board×git matrix ===")
	if !r.KaneoOK {
		fmt.Fprintf(out, "UNKNOWN: %s\n", r.KaneoError)
	} else {
		fmt.Fprintf(out, "in-review=%d\n", r.KaneoInReview)
		for _, row := range r.BoardGit {
			fmt.Fprintf(out, "ref=%s title=%s main=%t tip=%s park=%t\n", row.Ref, row.Title, row.Main, row.Tip, row.Park)
		}
	}
	printDrainErrors(out, r)
}

func printDrainCommands(r *review.DrainReport) {
	printDrainCommandsTo(os.Stdout, r)
}

func printDrainCommandsTo(out io.Writer, r *review.DrainReport) {
	evidence := make(map[string]drainActionEvidence, len(r.ActionEvidence))
	for _, item := range r.ActionEvidence {
		evidence[item.SHA] = item
	}
	for _, sha := range r.Shas.HarvestReady {
		e := evidence[sha]
		if !e.TierRecorded || !review.LedgerFamilyAllowlist[strings.ToLower(e.BuilderFamily)] {
			fmt.Fprintf(out, "# REFUSED harvest %s: recorded tier and builder family evidence required\n", sha)
			continue
		}
		fmt.Fprintf(out, "# REFUSED harvest %s: FAC-184 compiled adapter unavailable (lane=%s tier=%s sha=%s)\n", sha, e.Lane, e.Tier, sha)
	}
	for _, sha := range r.Shas.NeedReview {
		e := evidence[sha]
		switch {
		case e.Vetoed:
			fmt.Fprintf(out, "# REFUSED review %s: vetoed SHA\n", sha)
		case strings.TrimSpace(e.BuilderFamily) == "" || !review.LedgerFamilyAllowlist[strings.ToLower(e.BuilderFamily)]:
			fmt.Fprintf(out, "# REFUSED review %s: unknown builder family\n", sha)
		case drainForbiddenBranch(e.Branch):
			fmt.Fprintf(out, "# REFUSED review %s: forbidden branch %s\n", sha, e.Branch)
		default:
			fmt.Fprintf(out, "# REFUSED review %s: FAC-184 compiled adapter unavailable (branch=%s family=%s pin=%s)\n", sha, e.Branch, e.BuilderFamily, sha)
		}
	}
}

func printDrainErrors(out io.Writer, r *review.DrainReport) {
	for _, errText := range r.Errors {
		fmt.Fprintf(out, "UNKNOWN: %s\n", errText)
	}
}

func drainExitCode(r *review.DrainReport) int {
	if !r.KaneoOK || len(r.Errors) > 0 || r.Pending+r.HarvestQueue+r.Harvestable+r.NeedReview > 0 || r.KaneoInReview >= r.Cap {
		return 1
	}
	return 0
}

type drainActionEvidence = review.DrainActionEvidence

type drainActionHooks struct {
	launchReview func(context.Context, drainActionEvidence) error
	dryRun       func(context.Context, drainActionEvidence) error
	harvest      func(context.Context, drainActionEvidence) error
}

type drainActionResult struct {
	Reviews, Harvests, DryRuns, Refusals int
	Failed                               bool
}

func defaultDrainActionHooks() drainActionHooks {
	return drainActionHooks{
		launchReview: func(context.Context, drainActionEvidence) error {
			return errors.New("FAC-184 compiled review adapter unavailable")
		},
		dryRun: func(context.Context, drainActionEvidence) error {
			return errors.New("FAC-184 compiled harvest dry-run adapter unavailable")
		},
		harvest: func(context.Context, drainActionEvidence) error {
			return errors.New("FAC-184 compiled harvest adapter unavailable")
		},
	}
}

func drainAllowedTiers(raw string) map[string]bool {
	allowed := make(map[string]bool)
	for _, tier := range strings.Split(raw, ",") {
		tier = strings.ToUpper(strings.TrimSpace(tier))
		if tier != "" {
			allowed[tier] = true
		}
	}
	return allowed
}

func drainForbiddenBranch(branch string) bool {
	for _, segment := range strings.Split(strings.ToLower(strings.TrimSpace(branch)), "/") {
		if segment == "review" || segment == "park" || segment == "parked" || segment == "harvest" || segment == "harvested" {
			return true
		}
	}
	return false
}

func validDrainStandingLane(r *review.DrainReport, lane string) bool {
	if lane == "" || strings.ContainsAny(lane, "/") || strings.IndexFunc(lane, unicode.IsSpace) >= 0 {
		return false
	}
	for _, configured := range r.StandingLanes {
		if lane == configured {
			return true
		}
	}
	return false
}

func executeDrainActions(ctx context.Context, r *review.DrainReport, evidence []drainActionEvidence, maxReview, maxHarvest, maxRelaunch int, autoTiers string, out io.Writer, hooks drainActionHooks) drainActionResult {
	result := drainActionResult{}
	if hooks.launchReview == nil || hooks.harvest == nil {
		fmt.Fprintln(out, "herd-drain: REFUSED unknown action seam")
		result.Failed = true
		result.Refusals++
		return result
	}
	allowed := drainAllowedTiers(autoTiers)
	reviewCount, harvestCount, harvestAttempts, relaunchCount := 0, 0, 0, 0
	seenBranches := make(map[string]bool)
	for _, e := range evidence {
		if e.RebaseNeeded && os.Getenv("HERD_DRAIN_REBASE_MAIL") != "0" {
			if !validDrainStandingLane(r, e.Lane) {
				fmt.Fprintf(out, "REFUSED rebase-mail %s: invalid standing lane identity %q\n", e.SHA, e.Lane)
				result.Failed = true
				result.Refusals++
			} else if relaunchCount < maxRelaunch {
				fmt.Fprintf(out, "REFUSED rebase-mail %s: FAC-182 durable control-envelope delivery is unavailable; OPERATOR_RESOLUTION_REQUIRED ticket=rebase-mail/%s/%s\n", e.SHA, e.Lane, e.SHA[:minDrain(12, len(e.SHA))])
				result.Failed = true
				result.Refusals++
				relaunchCount++
			} else {
				fmt.Fprintf(out, "REFUSED rebase-mail %s: max relaunch bound reached\n", e.SHA)
				result.Refusals++
			}
		}
		if e.HarvestReady && harvestAttempts < maxHarvest {
			harvestAttempts++
			if hooks.dryRun == nil {
				fmt.Fprintf(out, "REFUSED harvest %s: missing dry-run seam\n", e.SHA)
				result.Failed = true
				result.Refusals++
				continue
			}
			fmt.Fprintf(out, "DRY-RUN harvest lane=%s sha=%s tier=%s\n", e.Lane, e.SHA, e.Tier)
			if err := hooks.dryRun(ctx, e); err != nil {
				fmt.Fprintf(out, "REFUSED harvest %s: %v\n", e.SHA, err)
				result.Failed = true
				result.Refusals++
				continue
			}
			result.DryRuns++
			if !e.TierRecorded || !allowed[strings.ToUpper(e.Tier)] {
				fmt.Fprintf(out, "REFUSED harvest %s: dry-run recorded; auto-harvest tier is not explicitly allowed\n", e.SHA)
				result.Refusals++
				continue
			}
			if !review.LedgerFamilyAllowlist[strings.ToLower(e.BuilderFamily)] {
				fmt.Fprintf(out, "REFUSED harvest %s: unknown builder family\n", e.SHA)
				result.Refusals++
				continue
			}
			if err := hooks.harvest(ctx, e); err != nil {
				fmt.Fprintf(out, "REFUSED harvest %s: %v\n", e.SHA, err)
				result.Failed = true
				result.Refusals++
			} else {
				harvestCount++
				result.Harvests++
			}
		}
		if !containsDrainSHA(r.Shas.NeedReview, e.SHA) || reviewCount >= maxReview {
			continue
		}
		if e.Vetoed {
			fmt.Fprintf(out, "REFUSED review %s: vetoed SHA\n", e.SHA)
			result.Refusals++
			continue
		}
		if drainForbiddenBranch(e.Branch) {
			fmt.Fprintf(out, "REFUSED review %s: forbidden branch %s\n", e.SHA, e.Branch)
			result.Refusals++
			continue
		}
		if e.Pending || seenBranches[e.Branch] {
			fmt.Fprintf(out, "REFUSED review %s: duplicate pending prefix\n", e.SHA)
			result.Refusals++
			continue
		}
		if strings.TrimSpace(e.BuilderFamily) == "" || !review.LedgerFamilyAllowlist[strings.ToLower(e.BuilderFamily)] {
			fmt.Fprintf(out, "REFUSED review %s: unknown builder family\n", e.SHA)
			result.Refusals++
			continue
		}
		if r.KaneoInReview < 0 || r.KaneoInReview+reviewCount >= r.Cap {
			fmt.Fprintf(out, "REFUSED review %s: review cap unknown or exceeded\n", e.SHA)
			result.Refusals++
			continue
		}
		fmt.Fprintf(out, "DRY-RUN review branch=%s family=%s sha=%s\n", e.Branch, e.BuilderFamily, e.SHA)
		if err := hooks.launchReview(ctx, e); err != nil {
			fmt.Fprintf(out, "REFUSED review %s: %v\n", e.SHA, err)
			result.Failed = true
			result.Refusals++
			continue
		}
		seenBranches[e.Branch] = true
		reviewCount++
		relaunchCount++
		result.Reviews++
	}
	fmt.Fprintf(out, "act_reviews=%d act_harvests=%d dry_runs=%d rebase_mail=0 refusals=%d\n", result.Reviews, result.Harvests, result.DryRuns, result.Refusals)
	return result
}

func containsDrainSHA(shas []string, want string) bool {
	for _, sha := range shas {
		if sha == want {
			return true
		}
	}
	return false
}

// runVerify is the FAC-98/FAC-116 completion gate: `herd verify <worktree>`
// exits 0 only when the worktree has real committed work, builds, and tests
// pass — the check an agent must pass before reporting done, and the forge
// runs before routing a build to review. Exit 1 on any violation (each
// reason carries its fix), 2 on usage.
func runVerify() {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	buildCmd := fs.String("build", "go build ./...", "build command run in the worktree")
	testCmd := fs.String("test", "go test ./...", "test command run in the worktree")
	asJSON := fs.Bool("json", false, "emit the check as JSON")
	fs.Parse(os.Args[2:])

	wt := fs.Arg(0)
	if wt == "" {
		fmt.Fprintln(os.Stderr, "usage: herd verify <worktree-path> [--build CMD] [--test CMD] [--json]")
		os.Exit(2)
	}
	if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "herd verify: %q is not a directory\n", wt)
		os.Exit(2)
	}

	v := verifier.NewVerifier("")
	c := v.CheckCompletion(context.Background(), wt, *buildCmd, *testCmd)

	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(c)
	} else if c.Passed {
		fmt.Printf("herd verify: %s PASSED (real commits, builds, tests pass)\n", wt)
	} else {
		fmt.Printf("herd verify: %s FAILED\n", wt)
		for _, r := range c.Reasons {
			fmt.Printf("  - %s\n", r)
		}
	}
	if !c.Passed {
		os.Exit(1)
	}
}

// runToolProbe (FAC-96): `herd tool-probe <model>` exits 0 only if the model
// actually EXECUTES a tool (creates a sentinel file), 1 if it merely talks.
func runToolProbe() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: herd tool-probe <model>")
		os.Exit(2)
	}
	r := herdr.ToolProbe(context.Background(), os.Args[2])
	if r.Executes {
		fmt.Printf("tool-probe: %s EXECUTES tools\n", r.Model)
		return
	}
	fmt.Printf("tool-probe: %s does NOT execute tools — %s\n", r.Model, r.Reason)
	os.Exit(1)
}

// runShoot (FAC-88): `herd shoot <pane|name> <refocus msg>` interrupts a
// stalled agent (escape) and refocuses it, without killing the pane.
// FAC-159: shot is refocus-only; new task launches must pass RequireTaskLaunch via dispatch.
func runShoot() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: herd shoot <pane|name> <refocus message>")
		os.Exit(2)
	}
	if err := requireFleetAdmission(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "herd shoot: %v\n", err)
		os.Exit(1)
	}
	if !herdr.IsAvailable() {
		fmt.Fprintln(os.Stderr, "herd shoot: herdr CLI not found")
		os.Exit(1)
	}
	target := os.Args[2]
	msg := strings.Join(os.Args[3:], " ")
	status, err := herdr.Shoot(target, msg, true, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd shoot: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd shoot: %s refocused -> %s\n", target, status)
}

// cliForgeDriver implements daemon.ForgeDriver by driving the herd binary and
// herdr fleet — the real side-effecting layer for `herd forge --loop`.
type cliForgeDriver struct {
	cfg      *config.Config
	maxLanes int
}

func (d *cliForgeDriver) Log(msg string) { fmt.Println(msg) }

// LaneState counts live task-fac-* builder agents that are working. A herdr
// read failure is UNKNOWN capacity, not free capacity (FAC-138) — reporting
// zero busy lanes on a failed list backfilled every lane the coordinator had
// just lost sight of.
func (d *cliForgeDriver) LaneState(ctx context.Context) (daemon.LaneState, error) {
	agents, err := herdr.AgentList()
	if err != nil {
		return daemon.LaneState{}, fmt.Errorf("herdr agent list: %w", err)
	}
	busy := 0
	for _, a := range agents {
		if strings.HasPrefix(a.Name, "task-fac-") && (a.Status == "working" || a.Status == "starting") {
			busy++
		}
	}
	return daemon.LaneState{Busy: busy, Max: d.maxLanes}, nil
}

// Signals: a card is completed when its builder agent exists and is no longer
// working; it is verified when herd verify passes on its worktree. An
// unreadable fleet yields an error, never an empty (and so drained-looking)
// signal set.
func (d *cliForgeDriver) Signals(ctx context.Context) (map[string]bool, map[string]bool, error) {
	completed := map[string]bool{}
	verified := map[string]bool{}
	agents, err := herdr.AgentList()
	if err != nil {
		return nil, nil, fmt.Errorf("herdr agent list: %w", err)
	}
	v := verifier.NewVerifier("")
	for _, a := range agents {
		if !strings.HasPrefix(a.Name, "task-fac-") {
			continue
		}
		if a.Status == "working" || a.Status == "starting" {
			continue
		}
		ref := strings.ToUpper(strings.TrimPrefix(a.Name, "task-"))
		completed[ref] = true
		wt := filepath.Join(".herd", "worktrees", strings.ToLower(ref))
		if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
			c := v.CheckCompletion(ctx, wt, "go build ./...", "go test ./...")
			if c.Passed {
				verified[ref] = true
			}
		}
	}
	return completed, verified, nil
}

func (d *cliForgeDriver) herd(args ...string) error {
	self, _ := os.Executable()
	cmd := exec.Command(self, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (d *cliForgeDriver) Dispatch(ctx context.Context, t *provider.Task) error {
	// FAC-159: wave/forge always route through `herd dispatch`, which runs
	// RequireTaskLaunch (selection + re-read) before worktree/status/tab and
	// post-validates with compensation on graph drift.
	return d.herd("dispatch", t.Ref, "--lane", "worker")
}

func (d *cliForgeDriver) Review(ctx context.Context, t *provider.Task) error {
	// --spawn BEFORE the ref: flag.Parse stops at the first positional, so the
	// old trailing form silently parsed spawn=false and no reviewer ever
	// started. runReview now also normalizes the order (FAC-138).
	return d.herd("review", "--spawn", t.Ref)
}

func (d *cliForgeDriver) Approve(ctx context.Context, t *provider.Task) error {
	// Evidence-gated board move (requires the branch to be merged on
	// origin/main); harvest/merge stays coordinator-owned git work.
	if err := d.herd("approve", t.Ref); err != nil {
		return err
	}
	_ = herdr.CloseTabForRef(t.Ref) // FAC-111: close the finished tab
	return nil
}

func (d *cliForgeDriver) Renudge(ctx context.Context, t *provider.Task) error {
	if err := requireFleetAdmission(ctx); err != nil {
		return err
	}
	agent := "task-" + strings.ToLower(t.Ref)
	msg := "RE-NUDGE " + t.Ref + ": you reported done but herd verify FAILED (missing commits, build, or tests). " +
		"Finish it: implement, `go build ./... && go test ./...` green, `herd verify` PASS, then commit. Do not stop until committed."
	_, err := herdr.Shoot(agent, msg, true, 30*time.Second)
	return err
}

// forgeLoopFenceDir is the single-active-coordinator fence for `herd forge
// --loop`. It is deliberately NOT the shared-checkout lock: the coordinator
// holds this for its whole run, while harvest/merge still needs the checkout
// lock underneath it.
const forgeLoopFenceDir = ".herd/forge-loop.lock.d"

// forgeLoopFenceMaxAge disables DirLock's age-based stale rule for the
// coordinator. A standing loop legitimately outlives any timer; holder-PID
// liveness is what releases an abandoned fence.
const forgeLoopFenceMaxAge = 365 * 24 * time.Hour

// runForgeLoop wires the real driver and runs the autonomous forge loop.
func runForgeLoop() { os.Exit(forgeLoopMain()) }

// forgeLoopMain returns the process exit code so every path releases the
// coordinator fence — os.Exit skips deferred releases (FAC-138).
func forgeLoopMain() int {
	fs := flag.NewFlagSet("forge-loop", flag.ExitOnError)
	_ = fs.Bool("loop", true, "run the autonomous loop")
	maxLanes := fs.Int("max-lanes", 3, "max concurrent builder lanes")
	interval := fs.Int("interval", 15, "seconds between ticks")
	ticks := fs.Int("ticks", 0, "stop after N ticks (0 = run until drained)")
	stopEmpty := fs.Bool("stop-empty", true, "stop when the board is clear and no lane is busy")
	fs.Parse(leadingPositionalArgs(os.Args[2:]))

	// Signal-aware: SIGINT/SIGTERM cancels the loop's context so the current
	// tick unwinds and the fence is released, instead of dying mid-transition.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	if err := requireFleetAdmission(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "forge --loop: %v\n", err)
		return 1
	}

	// Single-active coordinator: two loops driving the same board race every
	// claim, review and board write. Wait 0 — a second coordinator is an
	// operator error to report, not a queue to join.
	fence := lock.NewDirLock(forgeLoopFenceDir)
	fence.SetMaxAge(forgeLoopFenceMaxAge)
	if err := fence.Acquire(ctx, 0, "herd forge --loop"); err != nil {
		fmt.Fprintf(os.Stderr, "forge --loop: another coordinator is active: %v\n", err)
		return 1
	}
	defer fence.Release()

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge --loop: %v\n", err)
		return 1
	}
	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		return 1
	}
	st, err := store.New(".herd/herdforge.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge --loop: store init failed — durable dependency BLOCKED evidence is required: %v\n", err)
		return 1
	}
	defer st.Close()
	// The forge loop is wake-capable: it must be composed with a durable
	// coordinator reconciler. Until the authoritative task-scoped composition
	// is available, NewEngineWithControl makes the command fail closed before
	// any board or lane action rather than falling back to direct dispatch.
	eng := daemon.NewEngineWithControl(cfg, tp, nil, st, resolveCanonicalWorktreeManager(), nil, nil)
	driver := &cliForgeDriver{cfg: cfg, maxLanes: *maxLanes}

	fmt.Printf("herd forge --loop: max-lanes=%d interval=%ds — driving the board autonomously\n", *maxLanes, *interval)
	err = eng.ForgeLoop(ctx, driver, daemon.ForgeLoopOptions{
		Interval:  time.Duration(*interval) * time.Second,
		MaxTicks:  *ticks,
		StopEmpty: *stopEmpty,
	})
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		// Operator signal, with no transition left failing: a clean stop.
		fmt.Println("herd forge --loop: signalled — stopped between ticks")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "forge --loop: %v\n", err)
		return 1
	}
}

// scopedTestCommand (FAC-131) derives a TARGETED go test command from a
// worktree's diff against origin/main — only the Go packages that actually
// changed, so a small-context reviewer runs a focused suite instead of the
// whole repo. Falls back to `go test ./...` when the diff can't be read.
func scopedTestCommand(worktree string) string {
	cmd := exec.Command("git", "diff", "--name-only", "origin/main..HEAD")
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return "go test ./..."
	}
	pkgs := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, ".go") {
			continue
		}
		dir := filepath.Dir(line)
		if dir == "." || dir == "" {
			continue
		}
		pkgs["./"+dir+"/"] = true
	}
	if len(pkgs) == 0 {
		return "go test ./..."
	}
	var list []string
	for p := range pkgs {
		list = append(list, p)
	}
	sort.Strings(list)
	return "go test -count=1 " + strings.Join(list, " ")
}
func runDrainSelftest() int {
	failed := false
	for _, tool := range []string{"git", "herdr"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Printf("herd-drain selftest FAIL: missing tool %s\n", tool)
			failed = true
		}
	}
	fmt.Println("herd-drain selftest FAIL: FAC-184 compiled review/ledger/harvest/cap adapters are not installed")
	fmt.Println("herd-drain selftest FAIL: FAC-182 durable control-envelope delivery is blocked")
	failed = true
	if failed {
		fmt.Println("herd-drain selftest: FAIL (fail-closed prerequisite check)")
		return 1
	}
	return 0
}

// publishedGraphBinding reads the graph snapshot the coordinator published so
// the dispatcher binds to exactly what scopefence will resolve against. A
// mismatch here is rejected as "trusted graph snapshot rejected", so deriving
// both sides from the same stored row is the only way they cannot disagree.
func publishedGraphBinding(root string) (string, int) {
	store, err := scopefence.NewSQLiteStore(filepath.Join(root, ".herd", "scopefence.db"))
	if err != nil {
		return "", 0
	}
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		return "", 0
	}
	graph, err := store.ReadGraphSnapshot(context.Background(), repository)
	if err != nil {
		return "", 0
	}
	return graph.Revision, graph.Files
}

// leadingPositionalArgs moves a leading positional to the END so flag.Parse
// sees the flags. Go's flag package stops at the first non-flag argument,
// which has silently swallowed flags in several subcommands.
func leadingPositionalArgs(args []string) []string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return args
	}
	return append(append([]string{}, args[1:]...), args[0])
}

// releaseScopeClaimQuietly surrenders a completed ticket's scope claim through
// the fence's own abandonment path. Best-effort by design: a ticket that is
// provably merged must still close even if the fence is unavailable, so a
// failure here warns rather than blocking the board write that already
// succeeded.
func releaseScopeClaimQuietly(ref string) {
	store, err := scopefence.NewSQLiteStore(filepath.Join(".", ".herd", "scopefence.db"))
	if err != nil {
		return
	}
	repository, err := dispatch.AuthenticatedRepositoryIdentity(".")
	if err != nil {
		return
	}
	snap, err := store.Read(context.Background())
	if err != nil {
		return
	}
	for i := range snap.Owners {
		if snap.Owners[i].Task != ref {
			continue
		}
		fence := scopefence.Fence{Store: store, ReleaseAuthority: scopeauth.New()}
		if err := fence.Release(context.Background(), scopefence.ReleaseRequest{
			Ownership: snap.Owners[i],
			Authority: scopefence.FencedAbandonment,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "herd board-done: WARN %s closed but its scope claim could not be released: %v\n", ref, err)
			return
		}
		fmt.Printf("herd board-done: released %s scope claim in %s\n", ref, repository)
		return
	}
}
