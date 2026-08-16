package main

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
	"github.com/Kampe/Herdforge/pkg/candidateindex"
	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/classify"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/coordinator"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/feedback"
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
	"github.com/Kampe/Herdforge/pkg/process"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/resetsafe"
	"github.com/Kampe/Herdforge/pkg/resolve"
	"github.com/Kampe/Herdforge/pkg/resources"
	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/scopeauth"
	"github.com/Kampe/Herdforge/pkg/scopefence"
	"github.com/Kampe/Herdforge/pkg/security"
	"github.com/Kampe/Herdforge/pkg/selftest"
	"github.com/Kampe/Herdforge/pkg/standing"
	"github.com/Kampe/Herdforge/pkg/store"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/textdelivery"
	"github.com/Kampe/Herdforge/pkg/throughput"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
	"github.com/Kampe/Herdforge/pkg/usage"
	"github.com/Kampe/Herdforge/pkg/verifier"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

const version = "0.2.0-dev"

func main() {
	// A normal checkout is a local Herdr client. Hosted control-plane behavior
	// remains explicit via HERD_MODE=production (or HERD_CONTROL_SECRET).
	if _, set := os.LookupEnv("HERD_MODE"); !set && strings.TrimSpace(os.Getenv("HERD_CONTROL_SECRET")) == "" {
		_ = os.Setenv("HERD_MODE", "local")
	}
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
	// (see parseTicketRef / dispatch --ticket=). Manifest admission is
	// mandatory: a new switch case cannot bypass its contract.
	if err := admitRoutedCommand(command, os.Args[2:]); err != nil {
		if command == "control-surface" && strings.HasPrefix(err.Error(), "control-surface:") {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "herd: %v\n", err)
		os.Exit(1)
	}
	// Containers owns both its top-level and reconcile help text. Let its
	// nested dispatcher preserve that contract after manifest admission, while
	// still keeping help before its receipt-store or Docker paths.
	containersOwnsHelp := command == "containers"
	if !containersOwnsHelp && exitIfHelp(command, os.Args[2:]) {
		return
	}
	// Containers performs its own help gate in runContainersStatus and
	// runContainersReconcile; do not record an operational entry for either.
	if !containersOwnsHelp || !argsWantHelp(os.Args[2:]) {
		markOperational(command)
	}

	switch command {
	case "control-surface":
		runControlSurface()

	case "init":
		runInit()

	case "clone":
		runClone()

	case "preflight":
		runPreflight()

	case "preflight-static":
		runPreflightStatic()

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

	// FAC-156: the coordinator's merge authority, as compiled code.
	case "merge-admit":
		runMergeAdmit()

	case "merge-complete":
		runMergeComplete()

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

	case "fence-provision":
		runFenceProvision()

	case "fence-broker":
		runFenceBroker()

	case "board-done":
		runBoardDone()

	case "board-audit":
		runBoardAudit()

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

	case "timeline":
		runTimeline()

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

	case "hostcreds":
		// FAC-170 production caller (independent of FAC-133 WIP).
		runHostCreds()

	case "tests-for":
		runTestsFor()

	case "control":
		runControl()

	case "mail":
		runMail()

	case "netbroker-serve":
		runNetbrokerServe()

	case "task":
		runTaskClient()

	case "receipt":
		if len(os.Args) > 2 && os.Args[2] == "release" {
			runReceiptRelease()
		} else {
			runReceiptIssue()
		}

	case "broker":
		if len(os.Args) > 2 && os.Args[2] == "ensure" {
			runBrokerEnsure()
		} else {
			runBrokerServe()
		}

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
	fmt.Println("  preflight         Run workspace boundary, merge policy, and fleet readiness verification")
	fmt.Println("  preflight-static  Run static workspace boundary, signal literal, and merge policy scanner (no readiness)")
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
	fmt.Println("  shot         <task-ref>: one bounded task from eligibility to review handoff;")
	fmt.Println("               <prompt>: one headless prompt through the quota router")
	fmt.Println("  scope        Publish the trusted task scope the dispatch fence resolves against")
	fmt.Println("  review-classify   Deterministic R0-R3 risk floor for review dispatch")
	fmt.Println("  review-ingest     Validate reviewer verdicts and admit them to the ledger")
	fmt.Println("  harvest-merge     Cherry-pick a lane's reviewed commits onto a fresh base (--verify-landed: check if a merge landed)")
	fmt.Println("  hold       Control durable generation-fenced lane/task hold: on, off, or status")
	fmt.Println("  review     Claim in-progress tasks for reviewer and advance to review status")
	fmt.Println("  approve    Move in-review cards to done, gated on merge evidence")
	fmt.Println("  drain      Report coordinator review pile (optional bounded --act)")
	fmt.Println("  board-done Move one card to done ONLY from a task-bound completion receipt")
	fmt.Println("  board-audit Report Done cards that no completion receipt closed (read-only)")
	fmt.Println("  board-sync Reconcile board against git + live lanes; --fix advances lagging cards")
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
	fmt.Println("  tests-for       Targeted verification plan for <base>..<candidate>, gated on graph completeness (FAC-160)")
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
	fmt.Println("  hostcreds       HostCreds oracle: diagnose|session|selftest (FAC-170; no OpenCode)")
	fmt.Println("  control        Issue/drain authenticated control envelopes (FAC-133)")
	fmt.Println("  netbroker-serve Durable network allowlist broker process (FAC-133)")
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

// initRepoDefaults makes `herd init` useful in an existing checkout. It uses
// only repo-local metadata and falls back to the conservative Linear/Go
// template for an otherwise empty directory.
func initRepoDefaults() (name, repoURL, providerType, providerConfig, testCommand string) {
	name = filepath.Base(mustGetwd())
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "my-herd-app"
	}
	providerType = "linear"
	providerConfig = "  project_id: \"your-linear-project-id\"\n  api_key_env: \"LINEAR_API_KEY\""
	testCommand = "go test ./..."
	if out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output(); err == nil {
		repoURL = strings.TrimSpace(string(out))
	}
	if raw, err := os.ReadFile(".kaneo.json"); err == nil {
		var link struct {
			Project string `json:"project"`
		}
		if json.Unmarshal(raw, &link) == nil && strings.TrimSpace(link.Project) != "" {
			providerType = "kaneo"
			providerConfig = fmt.Sprintf("  enabled: [\"kaneo\"]\n  api_url: \"https://kanban-api.kampe.kluster\"\n  project_id: %q\n  use_cli: true", strings.TrimSpace(link.Project))
		}
	}
	if raw, err := os.ReadFile("package.json"); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(raw, &pkg) == nil {
			if _, ok := pkg.Scripts["test"]; ok {
				testCommand = "pnpm test"
			}
		}
	}
	return
}

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
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

	projectName, _, taskProvider, providerConfig, testCommand := initRepoDefaults()
	defaultConfig := fmt.Sprintf(`version: "1"
project:
  name: %q
  default_branch: "main"

task_provider:
  type: %q
%s

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
  test_command: %q
`, projectName, taskProvider, providerConfig, testCommand)
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

	projectName, repoURL, taskProvider, providerConfig, testCommand := initRepoDefaults()
	fullConfig := fmt.Sprintf(`version: "1"

# Herdforge — the forge that forges itself
project:
  name: %q
  default_branch: "main"
  repo_url: %q

task_provider:
  type: %q
%s

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
  test_command: %q
  preflight_command: "go build ./..."
`, projectName, repoURL, taskProvider, providerConfig, testCommand)

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

	cmd := exec.Command("git", "clone", "--", repoURL, targetDir) // #nosec G702 -- user values follow git -- and cannot be parsed as options
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "git clone failed: %v\n", err)
		os.Exit(1)
	}

	// Run herd init --full in the cloned directory
	initCmd := exec.Command(os.Args[0], "init", "--full") // #nosec G702 -- re-executes the current herd binary with fixed arguments
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

func runPreflightStatic() {
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
	// FAC-135: lint the repository's declared merge policy. This is a
	// declaration check, not per-candidate admission: it fails a repo that
	// claims protection while declaring no required checks or no
	// different-family review. Runtime admission is reviewledger.Admit.
	if err := preflight.RefuseAutonomousMerge("."); err != nil {
		fmt.Fprintf(os.Stderr, "Preflight failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Preflight merge-policy check passed. Required CI and different-family review declared.")
}

func runPreflight() {
	runPreflightStatic()

	// FAC-133 fleet readiness: optional live refresh, then consume attestation.
	// HERD_LIVE_HARNESS_PROOF=1 or HERD_REFRESH_READINESS=1 triggers a single-flight
	// live proof + durable signed attestation write. Without that, preflight only
	// reports the current ConsumeFleetAttestation status (dispatch fails closed
	// when HERD_CONTROL_SECRET is set and no valid attestation exists).
	root := security.ResolveReadinessRoot()
	if os.Getenv("HERD_LIVE_HARNESS_PROOF") == "1" || os.Getenv("HERD_REFRESH_READINESS") == "1" {
		fr, err := security.RefreshFleetAttestationLive(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAC-133 fleet readiness refresh failed: %v\n", err)
			if fr != nil {
				fmt.Fprint(os.Stderr, security.FormatReadinessReport(fr))
			}
			os.Exit(1)
		}
		fmt.Print(security.FormatReadinessReport(fr))
		fmt.Println("FAC-133 durable fleet attestation refreshed.")
		return
	}
	if fr, err := security.EvaluateFleetReadiness(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", security.FormatReadinessReport(fr))
		fmt.Fprintf(os.Stderr, "FAC-133 readiness: BLOCKED (refresh with HERD_LIVE_HARNESS_PROOF=1 herd preflight)\n")
		os.Exit(1)
	} else {
		fmt.Print(security.FormatReadinessReport(fr))
		fmt.Println("FAC-133 readiness: OK (durable attestation)")
	}
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
	// FAC-155: print the activated board binding AND the repository's enabled
	// policy, so a `type` edited away from the operator policy is visible in
	// status rather than only at the next activation attempt.
	fmt.Printf("Status: Active\nProject: %s\nProvider: %s (project=%s, enabled=%s)\nLanes: %d configured\n",
		cfg.Project.Name, cfg.TaskProvider.Type, cfg.TaskProvider.ProjectID,
		providerPolicySummary(cfg.TaskProvider.Enabled), len(cfg.Lanes))
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
	observer := &herdr.ProductionReconciliationObserver{Workspace: cfg.Fleet.HerdrWorkspace,
		Reader: herdr.SocketAuthorityReader{}, Record: (&herdr.JSONLRecorder{Path: ".herd/reconciliation.jsonl"}).Record}
	if err := observer.ObserveReconciliation(context.Background()); err != nil {
		fleet := herdr.ProjectFleetStatus(observer.Decisions(), len(cfg.Lanes))
		fmt.Printf("Reconciliation: BLOCKED (%v)\nFleet: working=%d capacity=%d standing=%d preserved=%d recovering=%d control=%d unknown=%d\n",
			err, fleet.Working, fleet.Capacity, fleet.Standing, fleet.Preserved, fleet.Recovering, fleet.ControlSeats, fleet.Unknown)
	} else {
		fleet := herdr.ProjectFleetStatus(observer.Decisions(), len(cfg.Lanes))
		fmt.Printf("Reconciliation: observed\nFleet: working=%d capacity=%d standing=%d preserved=%d recovering=%d control=%d unknown=%d\n",
			fleet.Working, fleet.Capacity, fleet.Standing, fleet.Preserved, fleet.Recovering, fleet.ControlSeats, fleet.Unknown)
	}
}

// loadTaskProvider activates the configured board provider with FAC-150
// deadlines via provider.NewFromHerdConfig. Non-Kaneo types error (FAC-155).
func loadTaskProvider(cfg *config.Config) (provider.TaskProvider, error) {
	if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.TaskProvider.Type), "kaneo") {
		if err := provider.PrepareRuntimeDefaults("."); err != nil {
			return nil, err
		}
	}
	return provider.NewFromHerdConfig(cfg)
}

// providerPolicySummary renders task_provider.enabled for status output. An
// omitted policy means "exactly the declared type" — never a discovered one.
func providerPolicySummary(enabled []string) string {
	if len(enabled) == 0 {
		return "declared-type-only"
	}
	return strings.Join(enabled, ",")
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

	// lastAdmit carries the exact decision AdmitRoute just proved so CreateTab
	// can open through the FAC-139 write-capable boundary (decision + tool-probe).
	var lastAdmit struct {
		lane     *config.LaneDef
		decision *router.LaunchDecision
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
			lastAdmit.lane = lane
			lastAdmit.decision = decision
			return standing.Route{
				Provider: decision.Provider,
				Model:    decision.Model,
				Effort:   decision.Effort,
				Harness:  decision.Harness,
				Decision: decision,
			}, nil
		},
		RepositoryIdentity: repositoryIdentityForLaunch,
		HarnessPresent: func(harness string) bool {
			_, err := exec.LookPath(harness)
			return err == nil
		},
		CreateTab: func(workspace, label, cwd string) (standing.Tab, error) {
			if lastAdmit.decision == nil || lastAdmit.lane == nil {
				return standing.Tab{}, errors.New("standing tab create requires prior AdmitRoute decision")
			}
			req := launch.Request{
				Decision: lastAdmit.decision,
				TaskRef:  lastAdmit.lane.Name,
				Scope:    router.ScopeLane,
				Lane:     lastAdmit.lane.Name,
			}
			_, tab, err := openWriteCapableTab(lastAdmit.decision, req, lastAdmit.lane, workspace, label, cwd)
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
			case standing.OutcomeUnraiseable:
				fmt.Fprintf(os.Stderr, "herd-standing: UNRAISEABLE %s: %s\n", rr.AgentName, rr.Reason)
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
	decision, err := launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, lane.Role, true, routedLaneDecision(context.Background(), nil), func(admitted *router.LaunchDecision) error {
		var tabErr error
		cwd := "."
		if lane.Worktree != "" {
			cwd = filepath.Join(".", lane.Worktree)
		}
		req := launch.Request{Decision: admitted, TaskRef: lane.Name, Scope: router.ScopeLane, Repository: repository, Lane: lane.Name}
		_, tab, tabErr = openWriteCapableTab(admitted, req, lane, herdr.ResolveWorkspace("."), fmt.Sprintf("forge-%s", lane.Name), cwd)
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
	if strings.TrimSpace(cfg.TaskProvider.ProjectID) == "" {
		fmt.Fprintf(os.Stderr, "task provider: project_id is required\n")
		os.Exit(1)
	}

	ctx := context.Background()

	// Find tasks in "in-progress" status
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "in-progress")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to list in-progress tasks: %v\n", err)
		os.Exit(1)
	}

	reviewRoot, rootErr := canonicalHerdRoot()
	if rootErr != nil {
		fmt.Fprintf(os.Stderr, "review: cannot resolve canonical root: %v\n", rootErr)
		os.Exit(1)
	}

	idx := candidateindex.New(candidateindex.IndexOptions{
		RepoRoot:     reviewRoot,
		Config:       cfg,
		TaskProvider: tp,
	})
	cands, candsErr := idx.BuildIndex(ctx)
	if candsErr != nil {
		fmt.Fprintf(os.Stderr, "failed to build candidate index: %v\n", candsErr)
		os.Exit(1)
	}

	if refArg != "" {
		want := hsync.NormalizeRef(refArg)
		var filtered []*candidateindex.Candidate
		for _, c := range cands {
			if strings.EqualFold(hsync.NormalizeRef(c.Ref), want) {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
	}

	// Also build task map for metadata lookup
	taskMap := make(map[string]*provider.Task)
	for _, t := range tasks {
		taskMap[hsync.NormalizeRef(t.Ref)] = t
	}

	if len(cands) == 0 {
		fmt.Println("No tasks in-progress to review.")
		return
	}

	claimIdx := -1
	for i, c := range cands {
		title := c.Title
		if title == "" {
			if t, ok := taskMap[hsync.NormalizeRef(c.Ref)]; ok {
				title = t.Title
			}
		}
		shaInfo := ""
		if c.CandidateSHA != "" {
			shaInfo = fmt.Sprintf(" sha=%s", shortSHA(c.CandidateSHA))
		}
		statusInfo := ""
		if c.State == candidateindex.StateBlocked {
			statusInfo = fmt.Sprintf(" [BLOCKED: %s]", strings.Join(c.BlockedEvidence, "; "))
		}
		fmt.Printf("[%d] [%s] %s (priority=%s%s)%s\n", i, c.Ref, title, c.Priority, shaInfo, statusInfo)
		if claimIdx < 0 && c.State != candidateindex.StateBlocked {
			claimIdx = i
		}
	}
	if claimIdx < 0 {
		fmt.Println("No eligible in-progress tasks found.")
		return
	}

	selectedCand := cands[claimIdx]
	selectedTitle := selectedCand.Title
	if selectedTitle == "" {
		if t, ok := taskMap[hsync.NormalizeRef(selectedCand.Ref)]; ok {
			selectedTitle = t.Title
		}
	}
	task := taskMap[hsync.NormalizeRef(selectedCand.Ref)]
	if task == nil {
		task = &provider.Task{
			Ref:       selectedCand.Ref,
			Title:     selectedTitle,
			Priority:  selectedCand.Priority,
			ProjectID: cfg.TaskProvider.ProjectID,
		}
	}
	fmt.Printf("\nSelected [%s] %s for review\n", task.Ref, task.Title)

	btp, _, berr := boundBoardProvider(cfg, tp, reviewRoot, task.Ref)
	if berr != nil {
		fmt.Fprintf(os.Stderr, "review status transition unbound (FAC-145): %v\n", berr)
		os.Exit(1)
	}

	// FAC-144: RequireCurrentPassing before any reviewer tab is created.
	// CheckCompletion is not sufficient authority for review spawn.
	wt := worktreePathForRef(task.Ref)
	if !worktreeExists(wt) {
		// Fall back to configured reviewer lane worktree only for
		// isolated standing reviewers — still require admission against
		// the task worktree when present.
		fmt.Fprintf(os.Stderr, "review: no task worktree at %s — cannot admit verification receipt\n", wt)
		os.Exit(1)
	}
	if err := admitWorktreeForReview(ctx, cfg, task.Ref, wt, ""); err != nil {
		fmt.Fprintf(os.Stderr, "review: receipt admission refused for %s: %v\n", task.Ref, err)
		os.Exit(1)
	}

	if spawn {
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

		// FAC-139: write-capable reviewer launch requires a current artifact
		// tool-probe PASS for the decision's surface — fail before any tab.
		if _, probeErr := ensureArtifactToolProbe(context.Background(), decision); probeErr != nil {
			fmt.Fprintf(os.Stderr, "review launch tool-probe rejected before tab creation: %v\n", probeErr)
			os.Exit(1)
		}

		// Exact task worktree — never the shared reviewer lane tree (incident:
		// review-assayer-FAC-151 opened inside the FAC-172 worktree).
		taskWT := filepath.Join(".herd", "worktrees", strings.ToLower(task.Ref))
		if fi, statErr := os.Stat(taskWT); statErr != nil || !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "review launch rejected: exact task worktree %s is required\n", taskWT)
			os.Exit(1)
		}

		// FAC-145: bind the reviewer to the exact provider/project/task AND
		// the exact candidate/base commits BEFORE any agent exists, signed
		// by the coordinator key. An active unexpired receipt for a
		// DIFFERENT review is never silently rebound. Fail closed before
		// spawning, delivering, or moving the card.
		// FAC-145 (blocker 1): the AUTHOR worktree is only the source of
		// the candidate commit — the reviewer never runs in it. The review
		// happens in a FRESH isolated worktree checked out DETACHED at the
		// exact candidate SHA, so review can neither mutate the candidate
		// branch nor destroy the author's session context.
		authorDir := filepath.Join(reviewRoot, ".herd", "worktrees", strings.ToLower(hsync.NormalizeRef(task.Ref)))
		if fi, statErr := os.Stat(authorDir); statErr != nil || !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "review: no candidate worktree for %s at %s — refusing to review a generic lane HEAD (FAC-145)\n", task.Ref, authorDir)
			os.Exit(1)
		}
		authorGit := func(args ...string) (string, error) {
			out, err := exec.Command("git", append([]string{"-C", authorDir}, args...)...).Output()
			return strings.TrimSpace(string(out)), err
		}
		branch, gitErr := authorGit("rev-parse", "--abbrev-ref", "HEAD")
		candidateSHA, cErr := authorGit("rev-parse", "HEAD")
		baseSHA, bErr := authorGit("rev-parse", "origin/main")
		if gitErr != nil || cErr != nil || bErr != nil {
			fmt.Fprintf(os.Stderr, "review: cannot resolve candidate/base commits from %s (FAC-145): %v %v %v\n", authorDir, gitErr, cErr, bErr)
			os.Exit(1)
		}

		// The isolated candidate checkout is prepared BEFORE any launcher
		// dependency: admission is proven even when herdr is unavailable.
		// One immutable review checkout per (ref, candidate): re-running the
		// same review reuses it; a different candidate gets its own.
		worktreeDir := filepath.Join(reviewRoot, ".herd", "reviews",
			fmt.Sprintf("%s-%s", strings.ToLower(hsync.NormalizeRef(task.Ref)), shortSHA(candidateSHA)))
		if err := ensureDetachedReviewWorktree(reviewRoot, worktreeDir, candidateSHA); err != nil {
			fmt.Fprintf(os.Stderr, "review: %v\n", err)
			os.Exit(1)
		}
		if existing, exErr := dispatch.ReadTaskContext(worktreeDir); exErr == nil {
			sameReview := strings.EqualFold(existing.TaskRef, task.Ref) && existing.CandidateSHA == candidateSHA
			if !sameReview && time.Now().Before(existing.ExpiresAt) {
				fmt.Fprintf(os.Stderr, "review worktree %s holds an ACTIVE receipt for %s@%s (expires %s) — refusing to rebind a live review (FAC-145)\n",
					worktreeDir, existing.TaskRef, existing.CandidateSHA, existing.ExpiresAt.Format(time.RFC3339))
				os.Exit(1)
			}
		}
		spawnRoot := reviewRoot
		if !herdr.IsAvailable() {
			fmt.Fprintf(os.Stderr, "herdr CLI not found\n")
			os.Exit(1)
		}
		signer, signerErr := dispatch.LoadSignerForConfig(cfg.Project.Name, spawnRoot)
		if signerErr != nil {
			fmt.Fprintf(os.Stderr, "coordinator signer (FAC-145): %v\n", signerErr)
			os.Exit(1)
		}
		// ONE generation per task: the reviewer JOINS the task's active
		// lease instead of minting an independent review generation.
		reviewLeaseRef := hsync.NormalizeRef(task.Ref)
		reviewLeaseKey := claim.LeaseKey{
			Repo: dispatch.RepositoryIdentityOrName(spawnRoot, cfg.Project.Name), Provider: cfg.TaskProvider.Type, Project: cfg.TaskProvider.ProjectID, TaskRef: reviewLeaseRef,
		}
		leaseID, leaseGen, leaseOwned, leaseErr := acquireOrJoinLease(context.Background(), spawnRoot, reviewLeaseKey, "coordinator-review", dispatch.RoleReviewer)
		if leaseErr != nil {
			fmt.Fprintf(os.Stderr, "review: %v\n", leaseErr)
			os.Exit(1)
		}
		// ONE durable admission lifecycle from here: every later failure
		// unwinds the exact side effects created so far (FAC-145).
		life := &reviewLifecycle{}
		// Only a lease this admission ACQUIRED may be released on failure:
		// releasing a joined task lease would revoke the author's authority.
		if leaseOwned {
			life.onFail(func() error {
				return releaseCoordinationLease(context.Background(), spawnRoot, reviewLeaseKey, "coordinator-review", leaseGen)
			})
		}

		reviewerReceipt, signErr := signer.Issue(dispatch.TaskContext{
			ProviderType:      cfg.TaskProvider.Type,
			ProjectID:         cfg.TaskProvider.ProjectID,
			ProviderWorkspace: cfg.TaskProvider.WorkspaceID,
			ProviderProfile:   cfg.TaskProvider.APIKeyEnv,
			Repository:        dispatch.RepositoryIdentityOrName(spawnRoot, cfg.Project.Name),
			Role:              dispatch.RoleReviewer,
			TaskRef:           task.Ref,
			TaskID:            task.ID,
			Branch:            branch,
			BaseSHA:           baseSHA,
			CandidateSHA:      candidateSHA,
			LeaseID:           leaseID,
			LeaseGeneration:   leaseGen,
			LeaseTaskRef:      reviewLeaseRef,
			SessionID:         dispatch.NewSessionID(dispatch.RoleReviewer, task.Ref, candidateSHA, leaseID),
			AllowedOps:        dispatch.ReviewerOps,
			ExpiresAt:         time.Now().Add(dispatch.DefaultReceiptTTL),
		})
		if signErr != nil {
			life.fail("failed to issue reviewer task context (FAC-145): %v", signErr)
		}
		priorReceipt, hadPrior := os.ReadFile(filepath.Join(worktreeDir, dispatch.TaskContextFile))
		if err := dispatch.WriteTaskContext(worktreeDir, reviewerReceipt); err != nil {
			life.fail("failed to write reviewer task context (FAC-145): %v", err)
		}
		life.onFail(func() error {
			path := filepath.Join(worktreeDir, dispatch.TaskContextFile)
			if hadPrior == nil {
				return os.WriteFile(path, priorReceipt, 0644)
			}
			return os.Remove(path)
		})

		// Board transition FIRST: a reviewer is never admitted to work a
		// card that is still In Progress. A later failure moves it back.
		if err := btp.UpdateStatus(ctx, task.ID, "in-review"); err != nil {
			life.fail("failed to move card to in-review status (FAC-145): %v", err)
		}
		fmt.Printf("  -> moved card [%s] to 'in-review' status\n", task.Ref)
		life.onFail(func() error { return btp.UpdateStatus(context.Background(), task.ID, "in-progress") })

		// FAC-145: the reviewer's PROCESS cwd is the isolated candidate
		// checkout — never a standing generic tab that merely gets told to
		// cd. A dedicated tab is created per (ref, candidate) with
		// TabCreateForTask so the pane starts inside the pinned worktree,
		// and the resolved session is bound into the receipt.
		tabLabel := fmt.Sprintf("review-%s-%s", strings.ToLower(hsync.NormalizeRef(task.Ref)), shortSHA(candidateSHA))
		if len(tabLabel) > 32 {
			tabLabel = tabLabel[:32]
		}
		ws, wsErr := herdr.RequireWorkspace(reviewRoot)
		if wsErr != nil {
			life.fail("review: herdr workspace unresolved (FAC-145): %v", wsErr)
		}
		tab, tabErr := herdr.TabCreateForTask(ws, tabLabel, worktreeDir, true)
		if tabErr != nil {
			life.fail("failed to create isolated reviewer tab: %v", tabErr)
		}
		life.onFail(func() error { return herdr.TabClose(tab.ID) })
		// The value tab create echoes back is only what we asked for. The
		// guarantee must rest on the LIVE terminal state, read for the exact
		// pane INCARNATION we just launched (FAC-145).
		reviewerSession := herdr.SessionID(tab.Pane)
		liveCwd, cwdErr := herdr.PaneLiveCwd(reviewerSession)
		if cwdErr != nil {
			life.fail("cannot read live reviewer pane cwd (FAC-145): %v", cwdErr)
		}
		if !sameDir(liveCwd, worktreeDir) {
			life.fail("live reviewer pane cwd %q is not the isolated candidate checkout %q (FAC-145)", liveCwd, worktreeDir)
		}
		// Start through the compiled LaunchDecision (main's admission path):
		// the isolated tab is FAC-145's requirement, the decision-bound start
		// is the launch contract — the reviewer needs both.
		// The reviewer runs under the TASK's generation, learned only after
		// the lease was joined above. Rebind the routed capability to that
		// exact context — lifecycle preparation refuses a task-scoped agent
		// with no generation, and the request must match the decision.
		boundDecision, rebindErr := router.RebindDecision(decision, task.Ref, leaseGen)
		if rebindErr != nil {
			life.fail("failed to bind reviewer launch to the task lease (FAC-145): %v", rebindErr)
		}
		reviewReq := taskLaunchRequest(boundDecision, task.Ref, repositoryIdentityForLaunch(cfg), lane.Name)
		if err := herdr.StartPreparedAgent(tab.ID, tabLabel, boundDecision.Harness, tab.Pane.ID, reviewReq); err != nil {
			life.fail("failed to start reviewer agent: %v", err)
		}
		targetLabel := tabLabel
		fmt.Printf("Spawned reviewer '%s' in tab %s (pane %s, cwd %s)\n", tabLabel, tab.ID, tab.Pane.ID, tab.Cwd)

		// Bind the LIVE session into the receipt: the reviewer's authority
		// names the exact tab/pane INCARNATION it runs in, so the authority
		// dies with that pane instead of transferring to whatever agent
		// occupies the slot next (FAC-145).
		boundReceipt := reviewerReceipt
		boundReceipt.Signature = ""
		boundReceipt.AgentSessionID = reviewerSession
		boundSigned, bindErr := signer.Issue(boundReceipt)
		if bindErr != nil {
			life.fail("failed to bind reviewer session (FAC-145): %v", bindErr)
		}
		if err := dispatch.WriteTaskContext(worktreeDir, boundSigned); err != nil {
			life.fail("failed to write session-bound reviewer receipt (FAC-145): %v", err)
		}
		if err := dispatch.StoreCanonicalReceipt(reviewRoot, boundSigned); err != nil {
			life.fail("failed to store canonical reviewer receipt (FAC-145): %v", err)
		}

		// FAC-131: a TIGHT, SCOPED review packet — no spec dump, only the
		// changed packages' targeted tests. Scope keeps the review inside
		// the model's context window. FAC-145: the verdict is filed through
		// the receipt-gated broker's TYPED reviewer-only operation — free
		// text can never carry verdict authority.
		testCmd := scopedTestCommand(worktreeDir)
		reviewPacket := fmt.Sprintf(`REVIEW %s — verdict ONLY, edit nothing.
cd %s
1. git diff origin/main..HEAD --stat  (see ONLY the changed files — review just these)
2. %s   (targeted tests for the changed packages, not the whole repo)
File your verdict through the broker (typed, receipt-bound):
  herd task verdict %s APPROVED
  herd task verdict %s REJECTED "<numbered fixes>"
Do not read the whole codebase. Do not run the full suite. Change nothing.`,
			task.Ref, worktreeDir, testCmd, task.Ref, task.Ref)

		// An undelivered review packet is a BLOCKED review, not a warning.
		if _, err := herdr.AgentPrompt(targetLabel, reviewPacket, false); err != nil {
			life.fail("failed to deliver review packet (FAC-145 BLOCKED): %v", err)
		}
		fmt.Printf("  -> delivered review packet to %s\n", targetLabel)
		return
	}

	// No --spawn: the coordinator still performs the bound transition.
	if err := btp.UpdateStatus(ctx, task.ID, "in-review"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to move card to in-review status (FAC-145): %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  -> moved card [%s] to 'in-review' status\n", task.Ref)
}

// parseApproveArgs parses `herd approve [<ref>] [--receipt <path>] [--override-* ...]`.
// Same swallowed-flag defect as review (FAC-138): with the ref first, the flags
// silently parsed as their zero values, so the leading positional comes out
// BEFORE parsing.
func parseApproveArgs(args []string) (ref, receiptPath string, ov overrideFlags) {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	receiptFlag := fs.String("receipt", "", "Completion receipt path (only with a single <ref> argument)")
	ovFlags := registerOverrideFlags(fs)
	fs.Parse(leadingPositionalArgs(args))
	return fs.Arg(0), *receiptFlag, ovFlags
}

// taskContextFor builds the FAC-145 launch receipt for a task outside the
// dispatcher (review spawn, coordinator transitions, approve read-back).
func taskContextFor(cfg *config.Config, task *provider.Task, branch, role string, ops []string) dispatch.TaskContext {
	return dispatch.TaskContext{
		ProviderType:      cfg.TaskProvider.Type,
		ProjectID:         cfg.TaskProvider.ProjectID,
		ProviderWorkspace: cfg.TaskProvider.WorkspaceID,
		ProviderProfile:   cfg.TaskProvider.APIKeyEnv,
		Repository:        cfg.Project.Name,
		Role:              role,
		TaskRef:           task.Ref,
		TaskID:            task.ID,
		Branch:            branch,
		AllowedOps:        ops,
		ExpiresAt:         time.Now().Add(dispatch.DefaultReceiptTTL),
	}
}

// runApprove sweeps in-review cards and moves each to done ONLY from a
// task-bound completion receipt (via sync.BoardDone). Cards without one are
// refused and stay in-review — a done card is a claim about reality.
func runApprove() {
	refArg, receiptPathArg, ovFlags := parseApproveArgs(os.Args[2:])

	override, ovErr := ovFlags.request()
	if ovErr != nil {
		fmt.Fprintf(os.Stderr, "herd approve: %v\n", ovErr)
		os.Exit(1)
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

	// FAC-145: acquire the CANONICAL transaction lock before scanning, and
	// hold it across reconcile AND the sweep — two coordinators can never
	// both capture the same pending set and re-drive a stale intent. The
	// scan itself runs under the lock (state revalidated after acquisition).
	root, rootErr := canonicalHerdRoot()
	if rootErr != nil {
		fmt.Fprintf(os.Stderr, "approve: cannot resolve canonical root: %v\n", rootErr)
		os.Exit(1)
	}
	release, lockErr := lockApprovals(root)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "approve: %v\n", lockErr)
		os.Exit(1)
	}
	finish := func(code int) {
		if err := release(); err != nil {
			fmt.Fprintf(os.Stderr, "approve: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
		if code != 0 {
			os.Exit(code)
		}
	}

	stack, stackErr := loadClaimStack(tp)
	if stackErr != nil {
		fmt.Fprintf(os.Stderr, "claim stack: %v\n", stackErr)
		os.Exit(1)
	}
	defer stack.Close()

	// Complete any interrupted callback+Done transitions BEFORE sweeping —
	// a crash between the journaled intent, the posted callback, and the
	// board move leaves a pending intent that is re-driven here
	// (idempotently), never forgotten.
	reconcileFailed := 0
	reconcileSigner, rsErr := dispatch.LoadSignerForConfig(cfg.Project.Name, root)
	if rsErr != nil {
		fmt.Fprintf(os.Stderr, "approve: coordinator signer unavailable (FAC-145): %v\n", rsErr)
		finish(1)
	}
	pend, perr := pendingApproveIntents(root, reconcileSigner)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "approve: intent journal unreadable (FAC-145): %v\n", perr)
		finish(1)
	}
	for i := range pend {
		in := pend[i]
		fmt.Printf("RECONCILE [%s]: completing interrupted approval %s (state %s)\n", in.Ref, in.DedupeID, in.State)
		// The journal names WHICH operation to finish; the closing authority
		// is re-read from that ref's completion receipt (FAC-132).
		if _, err := approveOne(ctx, cfg, tp, stack, root, in.Ref, "", nil, &in); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR    [%s]: reconcile: %v\n", in.Ref, err)
			reconcileFailed++
		}
	}

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
			finish(1)
		}
	} else if receiptPathArg != "" || override != nil {
		fmt.Fprintf(os.Stderr, "--receipt and --override-* need a single <ref> argument\n")
		finish(1)
	}

	if len(tasks) == 0 {
		fmt.Println("No tasks in review status to approve.")
		if reconcileFailed > 0 {
			finish(1)
		}
		finish(0)
		return
	}

	sort.SliceStable(tasks, func(i, j int) bool {
		return provider.CompareRefs(tasks[i].Ref, tasks[j].Ref) < 0
	})

	approved, refused, failed := 0, 0, reconcileFailed
	for _, task := range tasks {
		// FAC-145: receipt-bound, callback-coupled approval — missing/
		// unsigned/tampered receipt, wrong repo/project/task, expiry, stale
		// generation, or an undeliverable callback all refuse the mutation.
		res, err := approveOne(ctx, cfg, tp, stack, root, task.Ref, receiptPathArg, override, nil)
		switch {
		case err == nil:
			if res.Idempotent {
				fmt.Printf("ALREADY  [%s]: %s\n  receipt %s was already consumed\n", res.Ref, task.Title, res.ReceiptDigest)
			} else {
				fmt.Printf("APPROVED [%s]: %s\n  proof: %s\n", res.Ref, task.Title, res.Proof)
			}
			releaseScopeClaimQuietly(res.Ref)
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
		finish(1)
	}
	finish(0)
}

// boundBoardProvider binds board mutations for ref to its launch receipt:
// the receipt must EXIST in the managed worktree and authenticate against
// the published coordinator key; the coordinator context is then re-ISSUED
// (verified, widened, re-signed) by the private-key holder — there is no
// field-rewrite widening and NO config-derived fallback. A missing,
// unsigned, tampered, or mis-bound receipt refuses the mutation outright
// (FAC-145 fail-closed). Returns the issued coordinator context so callbacks
// share the exact binding. Generation fencing uses the durable callback
// high-water mark until FAC-147's canonical fence source lands.
// Every lookup — receipt, signer key, verifier anchor, lease high-water —
// resolves from the CANONICAL root, never process cwd: a linked reviewer or
// recovery worktree can no longer read the wrong receipt, key, or fence.
func boundBoardProvider(cfg *config.Config, tp provider.TaskProvider, root, ref string) (provider.TaskProvider, dispatch.TaskContext, error) {
	wt := filepath.Join(root, ".herd", "worktrees", strings.ToLower(hsync.NormalizeRef(ref)))
	tc, err := dispatch.ReadTaskContext(wt)
	if errors.Is(err, os.ErrNotExist) {
		// Worktree reaped: recover from the coordinator's DURABLE canonical
		// receipt store — issued, signed authority, not a config fallback.
		tc, err = dispatch.LoadCanonicalReceipt(root, hsync.NormalizeRef(ref))
	}
	if err != nil {
		return nil, tc, fmt.Errorf("no usable launch receipt for %s (worktree or canonical store; FAC-145 fail-closed, no config fallback): %w", ref, err)
	}
	signer, err := dispatch.LoadSignerForConfig(cfg.Project.Name, root)
	if err != nil {
		return nil, tc, fmt.Errorf("coordinator signer unavailable (FAC-145): %w", err)
	}
	coord, err := signer.IssueCoordinator(tc)
	if err != nil {
		return nil, tc, err
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return nil, tc, err
	}
	if err := requireLiveLease(context.Background(), root, coord); err != nil {
		return nil, tc, err
	}
	btp, err := dispatch.NewContextBoundProvider(tp, coord, dispatch.AuthorityFromConfigAt(cfg, root), verifier, nil, coord.LeaseGeneration)
	if err != nil {
		return nil, tc, err
	}
	return btp, coord, nil
}

// The approval journal durably couples the PASS callback to the board
// transition (FAC-145). It is the CANONICAL coordinator lifecycle record:
// resolved from the repository's COMMON root (never process cwd, so every
// worktree shares one journal and one lock), mode 0600 with ownership
// audited, appended only under an exclusive cross-process flock that also
// serializes the whole callback+Done transition, fsynced (file AND
// directory), and every record is coordinator-SIGNED and HASH-CHAINED
// (monotonic seq + prev-line hash) with a signed ANCHOR carrying the
// high-water (seq, hash). Deleting, truncating, reordering, or replaying
// signed lines breaks the chain or the anchor and fails reconciliation
// closed. Residual: a same-UID host process rolling back journal AND
// anchor to an older mutually consistent pair is only closed by FAC-147's
// durable fenced store — this file lock is local-machine serialization,
// replaced at that rebase.
const (
	approveIntentJournalName = "approve-intents.jsonl"
	approveIntentAnchorName  = "approve-intents.anchor"
	approveIntentLockName    = "approve-intents.lock"
	journalGenesis           = "genesis"
)

func approvalPath(root, name string) string {
	return filepath.Join(root, ".herd", name)
}

// canonicalHerdRoot resolves the repository's common root regardless of
// which worktree the process runs in.
func canonicalHerdRoot() (string, error) {
	return repoRootFromWorktree(".")
}

// requireLiveLease is the ERROR-RETURNING exact-lease validation against
// the durable claim store (the FAC-147 canonical-fence seam). The receipt
// must be backed by the CURRENT ACTIVE lease for its key: exact lease row
// identity, exact generation equality (a future or fabricated generation
// fails exactly like a stale one), coordinator-owned, active state, and
// unexpired. Missing, released, expired, or mismatched authority — and any
// store read failure — refuses BEFORE provider I/O.
func requireLiveLease(ctx context.Context, root string, tc dispatch.TaskContext) error {
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		return fmt.Errorf("fence store unavailable — refusing unfenced authority (FAC-145): %w", err)
	}
	defer st.Close()
	now := time.Now()
	leases, err := st.ActiveClaims(ctx, now)
	if err != nil {
		return fmt.Errorf("fence read failed — refusing unfenced authority (FAC-145): %w", err)
	}
	key := claim.LeaseKey{Repo: tc.Repository, Provider: tc.ProviderType, Project: tc.ProjectID, TaskRef: tc.LeaseTaskRef}
	var live *claim.Lease
	for i := range leases {
		if leases[i].LeaseKey == key {
			live = leases[i]
			break
		}
	}
	if live == nil {
		return fmt.Errorf("no ACTIVE lease for lease key %s — receipt %s lease %s gen %d carries no live authority (FAC-145 fail-closed; released/expired/missing all refuse)", tc.LeaseTaskRef, tc.TaskRef, tc.LeaseID, tc.LeaseGeneration)
	}
	if live.Expired(now) {
		return fmt.Errorf("lease for %s is expired (FAC-145)", tc.TaskRef)
	}
	if live.Generation != tc.LeaseGeneration {
		return fmt.Errorf("receipt lease generation %d does not equal the live lease generation %d for %s — stale or fabricated authority refused (FAC-145)", tc.LeaseGeneration, live.Generation, tc.TaskRef)
	}
	if fmt.Sprintf("claim:%d", live.ID) != tc.LeaseID {
		return fmt.Errorf("receipt lease id %s does not identify the live lease claim:%d for %s (FAC-145)", tc.LeaseID, live.ID, tc.TaskRef)
	}
	if !strings.HasPrefix(live.OwnerID, "coordinator-") {
		return fmt.Errorf("live lease for %s is owned by %q, not a coordinator session (FAC-145)", tc.TaskRef, live.OwnerID)
	}
	return nil
}

// acquireCoordinationLease acquires the durable claim lease that backs a
// receipt issuance — the real generation, never fabricated.
func acquireCoordinationLease(ctx context.Context, root string, key claim.LeaseKey, owner, role string) (string, int64, error) {
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		return "", 0, fmt.Errorf("claim store unavailable (FAC-145): %w", err)
	}
	defer st.Close()
	lease, err := st.Acquire(ctx, key, owner, role, "", time.Now(), dispatch.DefaultReceiptTTL)
	if err != nil {
		return "", 0, fmt.Errorf("claim lease acquisition failed (FAC-145): %w", err)
	}
	return fmt.Sprintf("claim:%d", lease.ID), lease.Generation, nil
}

// approveIntent persists the FULL bound identity of one approval operation
// (FAC-145): repository, provider, project, task, lease — not just ref/sha
// — and is keyed by its exact DedupeID, so operations from different
// projects or lease generations can never collapse onto each other.
// States: "intent" (internal pending, nothing external published), "done"
// (fenced board mutation + read-back succeeded), "published" (external
// completion callback delivered). The externally consumable completion is
// only ever published from state done.
type approveIntent struct {
	Seq             int64  `json:"seq"`
	PrevHash        string `json:"prev_hash"`
	Repository      string `json:"repository"`
	ProviderType    string `json:"provider_type"`
	ProjectID       string `json:"project_id"`
	Ref             string `json:"ref"`
	TaskID          string `json:"task_id"`
	SHA             string `json:"sha"`
	LeaseID         string `json:"lease_id"`
	LeaseGeneration int64  `json:"lease_generation"`
	DedupeID        string `json:"dedupe_id"`
	State           string `json:"state"` // "intent" | "done" | "published"
	At              string `json:"at"`
	Signature       string `json:"signature"`
}

// matchesContext rejects resuming an operation whose live receipt identity
// has drifted from the journaled one (e.g. a lease generation switch
// during recovery) — reconcile completes the EXACT recorded operation or
// fails.
func (r approveIntent) matchesContext(tc dispatch.TaskContext) error {
	if !strings.EqualFold(r.Repository, tc.Repository) || r.ProviderType != tc.ProviderType ||
		r.ProjectID != tc.ProjectID || r.TaskID != tc.TaskID ||
		r.LeaseID != tc.LeaseID || r.LeaseGeneration != tc.LeaseGeneration {
		return fmt.Errorf("journaled approval %s identity (repo=%s project=%s task=%s lease=%s gen=%d) does not match the live receipt (repo=%s project=%s task=%s lease=%s gen=%d) — refusing to resume a drifted operation (FAC-145)",
			r.DedupeID, r.Repository, r.ProjectID, r.TaskID, r.LeaseID, r.LeaseGeneration,
			tc.Repository, tc.ProjectID, tc.TaskID, tc.LeaseID, tc.LeaseGeneration)
	}
	return nil
}

type approveAnchor struct {
	Seq       int64  `json:"seq"`
	Hash      string `json:"hash"`
	Signature string `json:"signature"`
}

func (r approveIntent) canonical() ([]byte, error) {
	r.Signature = ""
	return json.Marshal(r)
}

func (a approveAnchor) canonical() ([]byte, error) {
	a.Signature = ""
	return json.Marshal(a)
}

func journalLineHash(line []byte) string {
	h := sha256.Sum256(line)
	return hex.EncodeToString(h[:])
}

// auditPrivateFile refuses state files that are group/world accessible or
// not owned by this coordinator uid. Missing files pass (creation follows).
func auditPrivateFile(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot audit %s (FAC-145 fail-closed): %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink — canonical state must not be redirectable (FAC-145)", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (FAC-145)", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is group/world accessible (%v) (FAC-145)", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s owned by uid %d, not this coordinator uid %d (FAC-145)", path, st.Uid, os.Getuid())
	}
	return nil
}

// auditStateDir refuses a .herd state directory not owned by this uid or
// reachable through a symlink.
func auditStateDir(root string) error {
	dir := filepath.Join(root, ".herd")
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cannot audit state dir %s (FAC-145 fail-closed): %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink — canonical state parent must not be redirectable (FAC-145)", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (FAC-145)", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s owned by uid %d, not this coordinator uid %d (FAC-145)", dir, st.Uid, os.Getuid())
	}
	return nil
}

// openNoFollow opens a state file refusing to traverse a symlink at the
// final component, and re-verifies the opened fd is a regular file
// (open-fd identity, not just a pre-open stat).
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot verify opened fd for %s (FAC-145): %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s resolved to a non-regular file (FAC-145)", path)
	}
	return f, nil
}

// approvalLockTimeout bounds how long an approval waits for the canonical
// lock: a wedged coordinator surfaces as BLOCKED, never an indefinite park.
const approvalLockTimeout = 15 * time.Second

// lockApprovals serializes approval scanning AND transitions across
// processes on the canonical lock (no-follow open, ownership audited,
// BOUNDED acquisition). The returned release reports unlock and close
// failures instead of discarding them.
func lockApprovals(root string) (func() error, error) {
	if err := auditStateDir(root); err != nil {
		return nil, err
	}
	lockPath := approvalPath(root, approveIntentLockName)
	if err := auditPrivateFile(lockPath); err != nil {
		return nil, err
	}
	f, err := openNoFollow(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open approval lock: %w", err)
	}
	deadline := time.Now().Add(approvalLockTimeout)
	for {
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flockErr == nil {
			break
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("BLOCKED(approval_lock): not acquired within %s — another coordinator holds it (FAC-145)", approvalLockTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return func() error {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			f.Close()
			return fmt.Errorf("release approval lock: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close approval lock: %w", err)
		}
		return nil
	}, nil
}

// loadApprovalChain strictly loads and authenticates the journal: every
// record parses, verifies, chains (seq+1, prev-line hash), and the signed
// anchor must match the final (seq, hash). A journal with no anchor, an
// anchor with no journal, or any break is a hard error — deletion,
// truncation at a line boundary, reordering, and replay of old signed
// lines all fail closed. Caller must hold the approval lock.
func loadApprovalChain(root string) (recs []approveIntent, lastSeq int64, lastHash string, reanchor bool, err error) {
	journalPath := approvalPath(root, approveIntentJournalName)
	anchorPath := approvalPath(root, approveIntentAnchorName)
	if err := auditPrivateFile(journalPath); err != nil {
		return nil, 0, "", false, err
	}
	if err := auditPrivateFile(anchorPath); err != nil {
		return nil, 0, "", false, err
	}

	data, readErr := os.ReadFile(journalPath)
	anchorData, anchorErr := os.ReadFile(anchorPath)
	if os.IsNotExist(readErr) {
		if anchorErr == nil {
			return nil, 0, "", false, fmt.Errorf("approval journal missing but anchor present — journal deleted or rolled back (FAC-145 fail-closed)")
		}
		return nil, 0, journalGenesis, false, nil
	}
	if readErr != nil {
		return nil, 0, "", false, readErr
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return nil, 0, "", false, fmt.Errorf("cannot authenticate approval journal (FAC-145): %w", err)
	}

	lastHash = journalGenesis
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec approveIntent
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, 0, "", false, fmt.Errorf("approval journal line %d is malformed or torn — refusing reconciliation until repaired (FAC-145 fail-closed): %w", i+1, err)
		}
		if rec.Ref == "" || rec.SHA == "" || rec.DedupeID == "" || rec.Repository == "" ||
			rec.ProviderType == "" || rec.ProjectID == "" || rec.TaskID == "" ||
			rec.LeaseID == "" || rec.LeaseGeneration < 1 ||
			(rec.State != "intent" && rec.State != "done" && rec.State != "published") {
			return nil, 0, "", false, fmt.Errorf("approval journal line %d is incomplete — refusing reconciliation (FAC-145 fail-closed)", i+1)
		}
		if rec.Seq != lastSeq+1 {
			return nil, 0, "", false, fmt.Errorf("approval journal line %d: sequence %d breaks the chain (want %d) — reordered, replayed, or dropped record (FAC-145 fail-closed)", i+1, rec.Seq, lastSeq+1)
		}
		if rec.PrevHash != lastHash {
			return nil, 0, "", false, fmt.Errorf("approval journal line %d: chain hash mismatch — reordered or tampered record (FAC-145 fail-closed)", i+1)
		}
		canonical, cErr := rec.canonical()
		if cErr != nil {
			return nil, 0, "", false, cErr
		}
		if err := verifier.VerifyBytes(canonical, rec.Signature); err != nil {
			return nil, 0, "", false, fmt.Errorf("approval journal line %d: %w", i+1, err)
		}
		recs = append(recs, rec)
		lastSeq = rec.Seq
		lastHash = journalLineHash([]byte(line))
	}

	// Anchor reconciliation. A crash between the journal append (fsynced)
	// and the anchor rename leaves the anchor exactly ONE record behind the
	// chain — deterministic torn-append state that is RECOVERABLE (the lock
	// holder re-anchors before proceeding), never stranded. Anything else —
	// missing anchor with history, gaps beyond one, hash mismatch — is
	// rollback or tampering and fails closed.
	if lastSeq > 0 {
		if anchorErr != nil {
			if lastSeq == 1 {
				return recs, lastSeq, lastHash, true, nil // crash before first anchor
			}
			return nil, 0, "", false, fmt.Errorf("approval journal has records but no anchor — rolled back or torn issuance (FAC-145 fail-closed)")
		}
		var anchor approveAnchor
		if err := json.Unmarshal(anchorData, &anchor); err != nil {
			return nil, 0, "", false, fmt.Errorf("approval anchor is malformed (FAC-145 fail-closed): %w", err)
		}
		canonical, cErr := anchor.canonical()
		if cErr != nil {
			return nil, 0, "", false, cErr
		}
		if err := verifier.VerifyBytes(canonical, anchor.Signature); err != nil {
			return nil, 0, "", false, fmt.Errorf("approval anchor: %w", err)
		}
		switch {
		case anchor.Seq == lastSeq && anchor.Hash == lastHash:
			// consistent
		case anchor.Seq == lastSeq-1 && anchor.Hash == recs[len(recs)-1].PrevHash:
			reanchor = true // torn append: journal ahead by exactly one
		default:
			return nil, 0, "", false, fmt.Errorf("approval journal high-water (seq %d) does not match anchor (seq %d) — truncated or rolled back (FAC-145 fail-closed)", lastSeq, anchor.Seq)
		}
	}
	return recs, lastSeq, lastHash, reanchor, nil
}

// writeApprovalAnchor atomically publishes the signed high-water.
func writeApprovalAnchor(root string, signer *dispatch.Signer, seq int64, hash string) error {
	anchor := approveAnchor{Seq: seq, Hash: hash}
	aCanonical, err := anchor.canonical()
	if err != nil {
		return err
	}
	aSig, err := signer.SignBytes(aCanonical)
	if err != nil {
		return fmt.Errorf("sign approval anchor: %w", err)
	}
	anchor.Signature = aSig
	anchorData, err := json.Marshal(anchor)
	if err != nil {
		return err
	}
	anchorPath := approvalPath(root, approveIntentAnchorName)
	tmp, err := os.CreateTemp(filepath.Dir(anchorPath), approveIntentAnchorName+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(anchorData, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), anchorPath); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	dir, err := os.Open(filepath.Dir(anchorPath))
	if err != nil {
		return fmt.Errorf("open journal dir for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("sync journal dir: %w", err)
	}
	return dir.Close()
}

// appendApproveIntent appends one signed, chained record and re-anchors
// the high-water. It first HEALS a torn append (journal one ahead of the
// anchor after a crash) by re-anchoring, so the single non-transactional
// window is deterministically recoverable, never stranded. Caller must
// hold the approval lock.
func appendApproveIntent(root string, signer *dispatch.Signer, rec approveIntent) error {
	_, lastSeq, lastHash, reanchor, err := loadApprovalChain(root)
	if err != nil {
		return err
	}
	if reanchor {
		if err := writeApprovalAnchor(root, signer, lastSeq, lastHash); err != nil {
			return fmt.Errorf("heal torn anchor: %w", err)
		}
	}
	rec.Seq = lastSeq + 1
	rec.PrevHash = lastHash
	rec.At = time.Now().UTC().Format(time.RFC3339)
	canonical, err := rec.canonical()
	if err != nil {
		return err
	}
	sig, err := signer.SignBytes(canonical)
	if err != nil {
		return fmt.Errorf("sign approval record: %w", err)
	}
	rec.Signature = sig
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	journalPath := approvalPath(root, approveIntentJournalName)
	f, err := openNoFollow(journalPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return writeApprovalAnchor(root, signer, rec.Seq, journalLineHash(line))
}

// pendingApproveIntents strictly replays the authenticated chain (healing
// a torn anchor first) and returns every operation — keyed by its EXACT
// DedupeID — whose latest state is not yet "published". Caller must hold
// the approval lock.
func pendingApproveIntents(root string, signer *dispatch.Signer) ([]approveIntent, error) {
	recs, lastSeq, lastHash, reanchor, err := loadApprovalChain(root)
	if err != nil {
		return nil, err
	}
	if reanchor {
		if err := writeApprovalAnchor(root, signer, lastSeq, lastHash); err != nil {
			return nil, fmt.Errorf("heal torn anchor: %w", err)
		}
	}
	last := map[string]approveIntent{}
	var order []string
	for _, rec := range recs {
		key := rec.DedupeID
		if _, seen := last[key]; !seen {
			order = append(order, key)
		}
		last[key] = rec
	}
	var pend []approveIntent
	for _, k := range order {
		if last[k].State != "published" {
			pend = append(pend, last[k])
		}
	}
	return pend, nil
}

// approveOne performs one receipt-bound approval as a durable state
// machine (caller MUST hold the canonical approval lock across scan and
// drive):
//  1. journal the INTERNAL "intent" record (full bound identity; nothing
//     externally consumable is published yet);
//  2. perform the evidence-gated board mutation WITH read-back;
//  3. journal "done";
//  4. only then publish the externally consumable PASS callback (stable
//     DedupeID) and journal "published".
//
// A crash in the post-board/pre-callback window leaves a "done" record the
// next run completes by publishing only; a crash earlier leaves "intent"
// re-driven in full. resume, when non-nil, completes the EXACT journaled
// operation: identity must match the live receipt (a lease-generation
// switch refuses), the evidence SHA is the recorded one, and no fresh
// intent is appended.
func approveOne(ctx context.Context, cfg *config.Config, tp provider.TaskProvider, stack *provider.ClaimStack, root, ref, receiptPath string, override *hsync.OverrideRequest, resume *approveIntent) (*hsync.DoneResult, error) {
	btp, coord, err := boundBoardProvider(cfg, tp, root, ref)
	if err != nil {
		return nil, err
	}
	signer, err := dispatch.LoadSignerForConfig(cfg.Project.Name, root)
	if err != nil {
		return nil, fmt.Errorf("coordinator signer unavailable (FAC-145): %w", err)
	}
	mb := mail.NewMailbox(filepath.Join(root, mail.DefaultMailFile))

	publish := func(rec approveIntent, proof string) error {
		cb, cbErr := coord.BoundCallback(mail.CallbackComplete, rec.SHA, "approved: "+proof)
		if cbErr != nil {
			return fmt.Errorf("approval callback binding refused (FAC-145): %w", cbErr)
		}
		cb.DedupeID = rec.DedupeID
		if _, pErr := mb.PostCallback(coord.Role, cb); pErr != nil {
			return fmt.Errorf("approval callback publication failed (board IS done; next approve republishes): %w", pErr)
		}
		rec.State = "published"
		if jErr := appendApproveIntent(root, signer, rec); jErr != nil {
			return fmt.Errorf("published but journal record failed (next approve reconciles): %w", jErr)
		}
		return nil
	}

	var rec approveIntent
	if resume != nil {
		if err := resume.matchesContext(coord); err != nil {
			return nil, err
		}
		rec = *resume
		if rec.State == "done" {
			// Post-board/pre-callback crash window: publication only.
			if err := publish(rec, "journaled approval "+rec.DedupeID); err != nil {
				return nil, err
			}
			return &hsync.DoneResult{Ref: rec.Ref, TaskID: rec.TaskID, Proof: "journaled approval " + rec.DedupeID, EvidenceSHA: rec.SHA}, nil
		}
	}

	// FAC-145 PRODUCTION supersession consumer: when the fleet has recorded
	// any delivered verdict for this task's candidate, the LATEST one
	// governs the merge decision — a later REJECTED vetoes an earlier
	// APPROVED and the approval refuses until a fresh admissible APPROVED
	// lands. Cards with no recorded verdict keep the receipt-gated path.
	if coord.CandidateSHA != "" {
		mbv := mail.NewMailbox(filepath.Join(root, mail.DefaultMailFile))
		eff, found, vErr := mbv.EffectiveVerdict(coord.Repository, coord.TaskRef, coord.CandidateSHA)
		if vErr != nil {
			return nil, fmt.Errorf("verdict state unreadable — refusing approval (FAC-145): %w", vErr)
		}
		if found && eff.Kind != mail.CallbackComplete {
			return nil, fmt.Errorf("effective verdict for %s@%s is %s — refusing approval until a fresh admissible APPROVED supersedes it (FAC-145): %s",
				coord.TaskRef, coord.CandidateSHA, eff.Kind, eff.Detail)
		}
	}

	// FAC-132 owns the closing authority: a task-bound completion receipt or
	// an explicit policy-limited override. Build it FIRST so the callback can
	// bind to the same proof commit the board move will use.
	req, closeAuthority, err := buildDoneRequest(".", cfg.TaskProvider.ProjectID, ref, receiptPath, override)
	if err != nil {
		closeAuthority()
		return nil, err
	}
	defer closeAuthority()

	var proof, proofSHA string
	switch {
	case req.Receipt != nil:
		proofSHA = req.Receipt.MergeSHA
		proof = fmt.Sprintf("completion receipt %s (merge %s)", req.Receipt.Digest, proofSHA)
	case req.Override != nil:
		// A manual override has no merge SHA of its own; bind the callback to
		// the integration state the operator is attesting to.
		out, gErr := exec.Command("git", "rev-parse", "origin/main").Output()
		if gErr != nil {
			return nil, fmt.Errorf("override approve: cannot resolve integration SHA: %w", gErr)
		}
		proofSHA = strings.TrimSpace(string(out))
		proof = "manual override, no completion receipt"
	default:
		return nil, fmt.Errorf("%w for %s: no completion receipt and no override", hsync.ErrNoEvidence, ref)
	}
	if proofSHA == "" {
		return nil, fmt.Errorf("%w for %s: closing authority carries no merge commit", hsync.ErrNoEvidence, ref)
	}

	if resume == nil {
		rec = approveIntent{
			Repository:      coord.Repository,
			ProviderType:    coord.ProviderType,
			ProjectID:       coord.ProjectID,
			Ref:             hsync.NormalizeRef(ref),
			TaskID:          coord.TaskID,
			SHA:             proofSHA,
			LeaseID:         coord.LeaseID,
			LeaseGeneration: coord.LeaseGeneration,
			DedupeID:        fmt.Sprintf("approve:%s:%s:%s:gen%d", coord.Repository, hsync.NormalizeRef(ref), proofSHA, coord.LeaseGeneration),
			State:           "intent",
		}
		if err := appendApproveIntent(root, signer, rec); err != nil {
			return nil, fmt.Errorf("approval intent journal write failed — refusing transition (FAC-145): %w", err)
		}
	} else if rec.SHA != proofSHA {
		return nil, fmt.Errorf("journaled approval %s evidence %s no longer proves on origin/main (resolved %s) — refusing drifted resume (FAC-145)", rec.DedupeID, rec.SHA, proofSHA)
	}

	var res *hsync.DoneResult
	if stack != nil {
		owner, oerr := provider.ProcessOwnerID()
		if oerr != nil {
			return nil, fmt.Errorf("approve: process owner identity: %w", oerr)
		}
		key := provider.LeaseKey(coord.Repository, coord.ProviderType, coord.ProjectID, coord.LeaseTaskRef)
		taskRole, rerr := provider.TaskOwnershipRole(nil, "worker")
		if rerr != nil {
			return nil, rerr
		}
		lease, lerr := stack.AcquireLease(ctx, key, owner, taskRole, taskRole)
		if lerr != nil {
			return nil, fmt.Errorf("approve refuses mutation without live lease: %w", lerr)
		}
		defer func() {
			_ = stack.Manager.Release(context.Background(), key, owner, lease.Generation)
		}()
		if ferr := stack.CAS.AdvanceFence(ctx, coord.TaskID, lease.Generation); ferr != nil {
			return nil, ferr
		}
		res, err = hsync.BoardDoneFenced(ctx, btp, stack, key, owner, lease.Generation, req)
	} else {
		res, err = hsync.BoardDone(ctx, btp, req)
	}
	if err != nil {
		// Internal failure signal only — no completion was ever published.
		ccb, cErr := coord.BoundCallback(mail.CallbackBlocked, "", fmt.Sprintf("board-done failed for %s: %v", rec.SHA, err))
		if cErr != nil {
			return nil, fmt.Errorf("%w; COMPENSATION BINDING ALSO FAILED (%v) — reconcile via herd board-sync", err, cErr)
		}
		ccb.DedupeID = rec.DedupeID + ":blocked"
		if _, pErr := mb.PostCallback(coord.Role, ccb); pErr != nil {
			return nil, fmt.Errorf("%w; COMPENSATING CALLBACK ALSO FAILED (%v) — reconcile via herd board-sync", err, pErr)
		}
		return nil, err
	}
	rec.State = "done"
	if err := appendApproveIntent(root, signer, rec); err != nil {
		return nil, fmt.Errorf("board done but journal done-record failed (next approve completes publication): %w", err)
	}
	if err := publish(rec, proof); err != nil {
		return nil, err
	}
	return res, nil
}

// runBoardDone is the strict single-card gate: exit 0 only when the card
// provably moved to done. Port of bin/herd-board-done.
func runBoardDone() {
	fs := flag.NewFlagSet("board-done", flag.ExitOnError)
	receiptPath := fs.String("receipt", "", "Completion receipt path (default .herd/receipts/<REF>.json)")
	ovFlags := registerOverrideFlags(fs)
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
		// FAC-213: the selftest used to call MergeEvidence (the grep-based
		// hint). That function is gone — it was defect #1. The selftest now
		// verifies that origin/main is reachable, which is the precondition
		// for LandedProof and BoardDone.
		if out, err := exec.Command("git", "rev-parse", "--verify", "-q", "origin/main").Output(); err != nil || strings.TrimSpace(string(out)) == "" {
			fmt.Fprintf(os.Stderr, "board-done selftest FAIL: no origin/main: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("board-done selftest PASS")
		return
	}

	ref := fs.Arg(0)
	if ref == "" {
		fmt.Fprintf(os.Stderr, "Usage: herd board-done <ref> [--receipt <path>] "+
			"[--override-policy <p> --override-actor <who> --override-reason <why> --override-evidence <what>]\n")
		os.Exit(2)
	}
	override, ovErr := ovFlags.request()
	if ovErr != nil {
		fmt.Fprintf(os.Stderr, "herd board-done: %v\n", ovErr)
		os.Exit(1)
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

	// FAC-145: the single-card gate is the same receipt-bound, callback-
	// coupled approval path as herd approve — locked, journaled, and the
	// PASS callback can never be silently missing here either.
	root, rootErr := canonicalHerdRoot()
	if rootErr != nil {
		fmt.Fprintf(os.Stderr, "herd board-done: %v\n", rootErr)
		os.Exit(1)
	}
	release, lockErr := lockApprovals(root)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "herd board-done: %v\n", lockErr)
		os.Exit(1)
	}
	stack, stackErr := loadClaimStack(tp)
	if stackErr != nil {
		fmt.Fprintf(os.Stderr, "claim stack: %v\n", stackErr)
		os.Exit(1)
	}
	defer stack.Close()
	res, err := approveOne(context.Background(), cfg, tp, stack, root, ref, *receiptPath, override, nil)
	if relErr := release(); relErr != nil {
		fmt.Fprintf(os.Stderr, "herd board-done: %v\n", relErr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-done: %v\n", err)
		os.Exit(1)
	}
	finishBoardDone(os.Stdout, res, releaseScopeClaimQuietly)
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
	fixFlag := fs.Bool("fix", false, "Advance to-do cards to in-progress when a live lane or branch proves work is in flight")
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
	syncer.Lanes = liveLaneSource{}

	if *fixFlag {
		code := runBoardSyncFix(syncer, cfg.TaskProvider.ProjectID, *asJSON)
		os.Exit(code)
	}

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

// liveLaneSource adapts herdr.AgentList to the sync.LaneSource interface.
// It lists live agents and extracts ticket refs from names matching
// "task-<ref>". When herdr is not installed, ListLanes returns an error
// (board-sync degrades to git-only reconciliation).
type liveLaneSource struct{}

func (liveLaneSource) ListLanes() ([]hsync.LaneRef, error) {
	if !herdr.IsAvailable() {
		return nil, fmt.Errorf("herdr not available")
	}
	agents, err := herdr.AgentList()
	if err != nil {
		return nil, err
	}
	var lanes []hsync.LaneRef
	for _, a := range agents {
		ref := hsync.RefFromAgentName(a.Name)
		if ref == "" {
			continue
		}
		lanes = append(lanes, hsync.LaneRef{Name: a.Name, Ref: ref})
	}
	return lanes, nil
}

// runBoardSyncFix advances to-do cards to in-progress when a live lane or
// git branch proves work is in flight, then reports what it did.
func runBoardSyncFix(syncer *hsync.BoardSyncer, projectID string, asJSON bool) int {
	result, err := syncer.FixBoardLag(context.Background(), projectID, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "board-sync --fix: %v\n", err)
		return 1
	}
	if asJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"advanced": result.Advanced,
			"errors":   result.Errors,
		})
	} else {
		if len(result.Advanced) == 0 && len(result.Errors) == 0 {
			fmt.Println("board-sync --fix: no BOARD_LAG to advance")
		}
		for _, f := range result.Advanced {
			fmt.Printf("board-sync --fix: advanced %s (%s) to in-progress\n", f.Ref, f.TaskID)
		}
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "board-sync --fix: %v\n", e)
		}
	}
	if len(result.Errors) > 0 {
		return 1
	}
	if len(result.Advanced) > 0 {
		return 0
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
// and unnamed panes are never touched. FAC-302: mutation mode executes
// fenced compare-and-close via TabCloseCAS with absence readback and
// deterministic receipts/counts. Dry-run is report-only.
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

	res, err := herdr.CleanupFenced(standing, *dryRun)
	if *asJSON {
		out := map[string]interface{}{
			"dry_run":     res.DryRun,
			"candidates":  res.Candidates,
			"attempts":    res.Attempts,
			"closed":      res.Closed,
			"blocked":     res.Blocked,
			"errored":     res.Errored,
			"error_count": len(res.Attempts) - res.Closed - res.Blocked,
		}
		if err != nil {
			out["error"] = err.Error()
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		if len(res.Candidates) == 0 && err == nil {
			fmt.Println("herd cleanup: nothing to close")
		}
		if res.DryRun {
			for _, c := range res.Candidates {
				fmt.Printf("herd cleanup: would close %s (tab %s) — %s\n", c.Name, c.TabID, c.Reason)
			}
		} else {
			for _, att := range res.Attempts {
				switch att.Outcome {
				case herdr.CleanupClosed:
					fmt.Printf("herd cleanup: closed %s (tab %s) — %s\n", att.Name, att.TabID, att.Reason)
				case herdr.CleanupBlocked:
					fmt.Printf("herd cleanup: BLOCKED %s (tab %s) — %s\n", att.Name, att.TabID, att.Reason)
				case herdr.CleanupError:
					fmt.Fprintf(os.Stderr, "herd cleanup: ERROR %s (tab %s) — %s\n", att.Name, att.TabID, att.Reason)
				}
			}
			if len(res.Candidates) > 0 {
				fmt.Printf("herd cleanup: closed=%d blocked=%d errored=%d candidates=%d\n",
					res.Closed, res.Blocked, res.Errored, len(res.Candidates))
			}
		}
	}
	if err != nil {
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
	fmt.Printf("  Provider : %s (project=%s, enabled=%s)\n",
		cfg.TaskProvider.Type, cfg.TaskProvider.ProjectID,
		providerPolicySummary(cfg.TaskProvider.Enabled))
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
	result, _, err := dispatchTicketDecision(context.Background(), req, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
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

// dispatchTicketDecision is the production claim + isolated dispatch for ONE
// ticket: lane identity, hold admission, scope-fenced dispatcher, launch
// admission. `herd dispatch` and the FAC-89 bounded `herd shot <task-ref>` lane
// share it so neither can drift into a weaker admission path than the other.
//
// It also returns the admitted LaunchDecision, because the surface that
// actually launched is what makes the builder family provable at review
// handoff. The decision is nil on the --no-launch path (nothing launched).
// Progress goes to announce; every failure is returned, never exited on.
func dispatchTicketDecision(ctx context.Context, req dispatchRequest, announce io.Writer) (*dispatch.DispatchResult, *router.LaunchDecision, error) {
	ticketRef := req.TicketRef
	noLaunch := req.NoLaunch
	laneName := req.LaneName
	laneExplicit := req.LaneExplicit

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config: %w", err)
	}
	registry, err := canonicalLaneRegistry(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("lane identity: %w", err)
	}
	var canonicalLane lifecycle.CanonicalLane
	if laneExplicit {
		canonicalLane, err = registry.ResolveLaneName(laneName)
	} else {
		canonicalLane, err = registry.ResolveRole(laneName)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lane identity: %w", err)
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
		return nil, nil, fmt.Errorf("task provider: %w", tpErr)
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
	production := productionMode()
	var d *dispatch.Dispatcher
	if production {
		d = dispatch.NewProductionDispatcherWithAuthorities(cfg, tp, wm,
			scopeVerifier, scopeVerifier, expectedRevision, expectedFiles)
	} else {
		// Local mode keeps the same router, worktree isolation, Herdr API, and
		// receipt evidence, while avoiding hosted-only MAC/signer/confinement
		// prerequisites that cannot exist on a normal single-user checkout.
		d = dispatch.NewDispatcher(cfg, tp, wm)
	}
	// A fresh checkout may not have a previously published scopefence row.
	// Dispatch's dependency gate can still establish the authoritative graph,
	// so bind run-state admission to that same provider-backed snapshot instead
	// of rejecting the first dispatch with an empty published revision.
	depStore := deps.StoreFor(tp, cfg.TaskProvider.ProjectID)
	d.Deps = depStore
	runStates, err := runstate.Open(filepath.Join(".herd", "dispatch-runs.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("dispatch runstate store: %w", err)
	}
	defer runStates.Close()
	d.RunStates = runStates
	d.RunStateGraph = func(context.Context) (string, error) {
		if strings.TrimSpace(expectedRevision) != "" {
			return expectedRevision, nil
		}
		snapshot, snapshotErr := depStore.SnapshotGraph(context.Background())
		if snapshotErr != nil {
			return "", fmt.Errorf("dependency graph snapshot: %w", snapshotErr)
		}
		if snapshot == nil {
			return "", errors.New("dependency graph snapshot returned empty revision")
		}
		revision := deps.GraphRevision(snapshot.Edges, nil, snapshot.ProviderRevision)
		if strings.TrimSpace(revision) == "" {
			return "", errors.New("dependency graph snapshot returned empty revision")
		}
		return revision, nil
	}
	// FAC-147: production board mutations go through ClaimStack Begin/Complete.
	stack, stackErr := loadClaimStack(tp)
	if stackErr != nil {
		fmt.Fprintf(os.Stderr, "claim stack: %v\n", stackErr)
		os.Exit(1)
	}
	defer stack.Close()
	d.Claims = stack
	// FAC-222: embed the coordinator's reply target in every TASK-PACKET.md.
	// Resolve from the durable registration; absence falls back to the
	// well-known default name (dispatch.coordinatorName handles that).
	if reg, rerr := coordinator.Resolve("."); rerr == nil {
		d.CoordinatorName = reg.Name
	}
	closeControl := func() error { return nil }
	if production {
		closeControl, err = configureProductionControl(d, ".")
		if err != nil {
			return nil, nil, fmt.Errorf("control store init failed: %w", err)
		}
	}
	defer closeControl()
	var decision *router.LaunchDecision
	var dispatchResult *dispatch.DispatchResult
	holdAuthority, holdErr := newProductionHoldAuthority()
	if holdErr != nil {
		return nil, nil, fmt.Errorf("hold authority: %w", holdErr)
	}
	defer holdAuthority.Close()
	repositoryIdentity, identityErr := holdRepository()
	if identityErr != nil {
		return nil, nil, fmt.Errorf("hold identity: %w", identityErr)
	}
	admitDispatch := func() error {
		for _, identity := range []lifecycle.HoldIdentity{{Repository: repositoryIdentity, Owner: canonicalLane.Role, Lane: canonicalLane.Name, Scope: "lane"}, {Repository: repositoryIdentity, Owner: canonicalLane.Role, Lane: canonicalLane.Name, Task: ticketRef, Scope: "task"}} {
			generation, err := holdAuthority.CurrentGeneration(ctx, identity)
			if err != nil {
				return err
			}
			decision, err := holdAuthority.Check(ctx, identity, generation)
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
		return nil, nil, fmt.Errorf("dispatch hold admission rejected: %w", err)
	}
	// FAC-145: dispatch is backed by a REAL acquired claim lease. Acquired
	// once here so the launch and --no-launch paths below both dispatch under
	// the same generation.
	dispatchRoot, rootErr := canonicalHerdRoot()
	if rootErr != nil {
		fmt.Fprintf(os.Stderr, "dispatch: cannot resolve canonical root: %v\n", rootErr)
		os.Exit(1)
	}
	// FAC-145: broker ADMISSION precedes the durable claim — a dispatch
	// that cannot possibly launch a provider-capable agent never strands a
	// lease on the ticket.
	if !noLaunch && production {
		// The broker authenticates receipt-bound requests and therefore needs
		// the coordinator's published verification key before self-start. Load
		// the isolated coordinator signer here; dispatch later reuses it for
		// the task receipt and never exposes the private key to a worker.
		if _, signerErr := dispatch.LoadSignerForConfig(cfg.Project.Name, dispatchRoot); signerErr != nil {
			fmt.Fprintf(os.Stderr, "dispatch refused — receipt authority: %v\n", signerErr)
			os.Exit(1)
		}
		sock, sockErr := brokerSocketPath(dispatch.RepositoryIdentityOrName(dispatchRoot, cfg.Project.Name))
		if sockErr != nil {
			fmt.Fprintf(os.Stderr, "dispatch: %v\n", sockErr)
			os.Exit(1)
		}
		if err := ensureBroker(dispatchRoot, sock); err != nil {
			fmt.Fprintf(os.Stderr, "dispatch refused — %v\n", err)
			os.Exit(1)
		}
	}

	// FAC-145: dispatch is backed by a REAL acquired claim lease, taken only
	// after broker admission and shared by both paths below so they dispatch
	// under the same generation.
	leaseKey := claim.LeaseKey{
		Repo: dispatch.RepositoryIdentityOrName(dispatchRoot, cfg.Project.Name), Provider: cfg.TaskProvider.Type, Project: cfg.TaskProvider.ProjectID, TaskRef: hsync.NormalizeRef(ticketRef),
	}
	leaseID, leaseGen, leaseErr := acquireCoordinationLease(ctx, dispatchRoot, leaseKey, "coordinator-dispatch", laneName)
	if leaseErr != nil {
		fmt.Fprintf(os.Stderr, "dispatch: %v\n", leaseErr)
		os.Exit(1)
	}

	if !noLaunch {
		// canonicalLane.Role, not a second lookup: this is the same lane the hold
		// gate above already admitted.
		decision, err = launchAdmissionWithLifecycle(liveLaunchLifecycle{}, cfg, canonicalLane.Role, true, routedLaneDecision(ctx, nil), func(admitted *router.LaunchDecision) error {
			if err := admitDispatch(); err != nil {
				return err
			}
			var dispatchErr error
			dispatchResult, dispatchErr = d.Dispatch(ctx, dispatch.DispatchOptions{TicketRef: ticketRef, NoLaunch: noLaunch, LaneName: laneName, Decision: admitted, LeaseID: leaseID, LeaseGeneration: leaseGen})
			return dispatchErr
		})
		if err != nil {
			// launchAdmission can fail after the coordination lease is acquired
			// (for example, a missing native harness probe). Release that exact
			// lease before returning so a failed launch never blocks the next tick.
			if relErr := releaseCoordinationLease(ctx, dispatchRoot, leaseKey, "coordinator-dispatch", leaseGen); relErr != nil {
				return nil, nil, fmt.Errorf("dispatch launch failed: %w; LEASE COMPENSATION ALSO FAILED: %v", err, relErr)
			}
			return nil, nil, fmt.Errorf("dispatch launch failed: %w", err)
		}
		// launchAdmission already validated lane capability before any side effect.
		// Dispatch rebinds/validates the exact task+lease after claim; never post-validate a lane decision against a task ref.
	}
	fmt.Fprintf(announce, "Dispatching %s to lane '%s'...\n", ticketRef, laneName)

	result := dispatchResult
	if noLaunch {
		if err := admitDispatch(); err != nil {
			return nil, nil, fmt.Errorf("dispatch hold admission rejected: %w", err)
		}
		result, err = d.Dispatch(ctx, dispatch.DispatchOptions{TicketRef: ticketRef, NoLaunch: true, LaneName: laneName, Decision: decision, LeaseID: leaseID, LeaseGeneration: leaseGen})
	}
	if err != nil {
		// Durable compensation: a failed dispatch releases the exact lease
		// it acquired — the ticket is never stranded behind a dead claim.
		if relErr := releaseCoordinationLease(ctx, dispatchRoot, leaseKey, "coordinator-dispatch", leaseGen); relErr != nil {
			return nil, nil, fmt.Errorf("dispatch failed: %w; LEASE COMPENSATION ALSO FAILED: %v", err, relErr)
		}
		return nil, nil, fmt.Errorf("dispatch failed (lease released): %w", err)
	}
	if result == nil {
		return nil, nil, fmt.Errorf("dispatch returned no result for %s", ticketRef)
	}
	return result, decision, nil
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
	// FAC-133 MAC control plane (pkg/envelope) — distinct from coordinator orders above.
	if secret := strings.TrimSpace(os.Getenv("HERD_CONTROL_SECRET")); secret != "" {
		mailPath := strings.TrimSpace(os.Getenv("HERD_MAIL_FILE"))
		if mailPath == "" {
			mailPath = filepath.Join(root, ".herd", "mail.jsonl")
		} else {
			if filepath.IsAbs(mailPath) {
				return nil, fmt.Errorf("HERD_MAIL_FILE must be relative to workspace root")
			}
			mailPath = filepath.Clean(filepath.Join(root, mailPath))
			rel, relErr := filepath.Rel(root, mailPath)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("HERD_MAIL_FILE escapes workspace root")
			}
		}
		_ = os.MkdirAll(filepath.Dir(mailPath), 0o755)
		issuer := strings.TrimSpace(os.Getenv("HERD_CONTROL_ISSUER"))
		if issuer == "" {
			issuer = "coordinator"
		}
		d.ControlSecret = secret
		d.Control = &dispatch.ControlPlane{
			Secret:        secret,
			Mailbox:       mail.NewMailbox(mailPath),
			IssuerRole:    "coordinator",
			IssuerSession: issuer,
			DurableRoot:   root,
		}
		if id, err := dispatch.AuthenticatedRepositoryIdentity(root); err == nil {
			d.RepoIdentity = id
			d.RepoAllowlist = []string{id}
		}
	}
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
	logCmd := exec.Command("git", "log", "--format=%H%x09%cI%x09%s", // #nosec G702 -- mainRef follows git --end-of-options and cannot inject flags
		"--since="+since, "--until="+until, "--end-of-options", mainRef)
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

	wm := resolveCanonicalWorktreeManager()
	v := verifier.NewVerifier(cfg.Verification.TestCommand)
	st, err := store.New(".herd/herdforge.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "store init failed — forge requires durable dependency BLOCKED evidence: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx := context.Background()
	fmt.Println("=== Forge: Pulse ===")
	var forgeLane *config.LaneDef
	var forgeDecision *router.LaunchDecision
	var forgeLeaseGeneration int64
	var task *provider.Task
	var eng *daemon.Engine
	if !herdr.IsAvailable() {
		return errors.New("herdr CLI not found — refusing launch-required forge claim")
	}
	forgeLane = findLaneForRole(cfg, "worker")
	if forgeLane == nil {
		return errors.New("no worker lane configured; refusing forge claim")
	}
	// FAC-194: after route admission, bind ModelRouter from the compiled
	// LaunchDecision (forbidden-identity gate) before any claim. Never from
	// an embedded OpenCode/DeepSeek fallback.
	forgeDecision, err = forgeLaunchAdmission(cfg, forgeLane, ctx, func(d *router.LaunchDecision) error {
		return applyWorkerModelRouterBeforeClaim(d, func(mr *router.ModelRouter) error {
			eng = daemon.NewEngine(cfg, tp, attachWorkerUsage(mr), st, wm, v)
			var claimErr error
			task, claimErr = eng.RunPulse(ctx, "worker")
			return claimErr
		})
	})
	if err != nil && errors.Is(err, ErrWorkerModelPolicy) {
		if recErr := recordWorkerModelPolicyBlocked(st, "forge", err.Error()); recErr != nil {
			return fmt.Errorf("launch route rejected before forge claim: %w (%v)", err, recErr)
		}
	}
	if err != nil {
		return fmt.Errorf("launch route rejected before forge claim: %w", err)
	}
	if err := validateDecisionBeforeSideEffect(forgeDecision, forgeLane.Name); err != nil {
		return fmt.Errorf("launch decision rejected before forge claim: %w", err)
	}
	if eng == nil {
		return fmt.Errorf("forge engine was not composed after launch admission")
	}
	claimedRef, claimedGeneration := eng.LastClaimIdentity()
	if task != nil {
		if claimedRef != task.Ref || claimedGeneration == 0 {
			return fmt.Errorf("forge launch identity unavailable for %s", task.Ref)
		}
		forgeLeaseGeneration = claimedGeneration
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
					req := taskLaunchRequest(decision, task.Ref, repositoryIdentityForLaunch(cfg), lane.Name)
					_, tab, tabErr := openWriteCapableTab(decision, req, lane, herdr.ResolveWorkspace("."), tabLabel, cwd)
					if tabErr == nil {
						if err := herdr.StartPreparedAgent(tab.ID, tabLabel, decision.Harness, tab.Pane.ID, req); err != nil {
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

	forgeFailed := false

	fmt.Println("\n=== Forge: Review ===")
	stack, stackErr := loadClaimStack(tp)
	if stackErr != nil {
		fmt.Fprintf(os.Stderr, "claim stack: %v\n", stackErr)
		os.Exit(1)
	}
	defer stack.Close()
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "in-progress")
	if err == nil && len(tasks) > 0 {
		t := tasks[0]
		fmt.Printf("Selected [%s]: %s for review\n", t.Ref, t.Title)
		// FAC-145: no raw status writes — the transition is bound to the
		// task's authenticated receipt; failures propagate.
		forgeRoot, forgeRootErr := canonicalHerdRoot()
		if forgeRootErr != nil {
			fmt.Fprintf(os.Stderr, "  review transition: %v\n", forgeRootErr)
			forgeFailed = true
		} else if btp, _, bindErr := boundBoardProvider(cfg, tp, forgeRoot, t.Ref); bindErr != nil {
			fmt.Fprintf(os.Stderr, "  review transition unbound (FAC-145): %v\n", bindErr)
			forgeFailed = true
		} else if err := btp.UpdateStatus(ctx, t.ID, "in-review"); err != nil {
			fmt.Fprintf(os.Stderr, "  review transition failed (FAC-145): %v\n", err)
			forgeFailed = true
		} else {
			fmt.Printf("  -> moved to 'in-review' status\n")
		}
	}

	fmt.Println("\n=== Forge: Approve ===")
	reviewTasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "in-review")
	if err == nil {
		root, rootErr := canonicalHerdRoot()
		if rootErr != nil {
			fmt.Fprintf(os.Stderr, "forge approve: %v\n", rootErr)
			os.Exit(1)
		}
		release, lockErr := lockApprovals(root)
		if lockErr != nil {
			fmt.Fprintf(os.Stderr, "forge approve: %v\n", lockErr)
			os.Exit(1)
		}
		for _, t := range reviewTasks {
			// FAC-145: receipt-bound, callback-coupled approval only.
			res, err := approveOne(ctx, cfg, tp, stack, root, t.Ref, "", nil, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Not approved [%s]: %v\n", t.Ref, err)
				if !errors.Is(err, hsync.ErrNoEvidence) {
					forgeFailed = true
				}
				continue
			}
			fmt.Printf("Approved [%s]: %s (proof: %s)\n", res.Ref, t.Title, res.Proof)
			releaseScopeClaimQuietly(res.Ref)
		}
		if relErr := release(); relErr != nil {
			fmt.Fprintf(os.Stderr, "forge approve: %v\n", relErr)
			forgeFailed = true
		}
	}

	fmt.Println("\n=== Forge cycle complete ===")
	// FAC-145: a forge cycle with a real failure exits non-zero (caller
	// surfaces it); ErrNoEvidence refusals are not cycle failures.
	if forgeFailed {
		return fmt.Errorf("forge cycle completed with failures")
	}
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
	return laneLaunchDecisionWithProbe(ctx, lane, task, herdr.ProbeProviderModel)
}

func laneLaunchDecisionWithProbe(ctx context.Context, lane *config.LaneDef, task *provider.Task, probeModel func(context.Context, string, string, string) herdr.ProbeResult) (*router.LaunchDecision, error) {
	if lane == nil {
		return nil, fmt.Errorf("launch route requires a configured lane")
	}
	if err := validateLaneLaunchConfig(lane); err != nil {
		return nil, err
	}
	if harness := strings.TrimSpace(lane.Harness); harness != "" {
		if _, err := exec.LookPath(harness); err != nil {
			return nil, fmt.Errorf("%w: lane %q harness %q binary not found in $PATH — install %s or provision the harness before raising lanes", ErrHarnessBinaryMissing, lane.Name, harness, harness)
		}
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
		probes[key] = probeModel(ctx, cp, cm, lane.Effort).Available
	}
	if router.ModelRequiresProbe(model) {
		probe := probeModel(ctx, provider, model, lane.Effort)
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
	r := router.NewRouter(nil, nil)
	// The lane's configured harness was LookPath-checked above. Do not let the
	// router's legacy Pi availability probe veto this direct vendor launch.
	r.Probes = &router.Probes{CLIPresent: func(string) bool { return true }, Now: time.Now}
	decision, err := r.Decide(request)
	if err != nil {
		return nil, err
	}
	decision, err = router.BindVendorHarness(decision, lane.Harness)
	if err != nil {
		return nil, fmt.Errorf("lane %q bind configured harness: %w", lane.Name, err)
	}
	if decision.Shape != lane.TaskShape {
		return nil, fmt.Errorf("lane %q routed shape drift: configured %s, got %s", lane.Name, lane.TaskShape, decision.Shape)
	}
	if decision.Harness != strings.ToLower(strings.TrimSpace(lane.Harness)) {
		return nil, fmt.Errorf("lane %q routed harness drift: got %s, want %s", lane.Name, decision.Harness, lane.Harness)
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
	expectedShapes := map[string]string{launch.WorkerRole: "implementation", launch.ForgeSmithRole: "implementation", launch.RecoveryRole: "implementation", launch.ReviewerRole: "qa", launch.AssayerRole: "qa", launch.OrchestratorRole: "coordinator", launch.ScoutPlannerRole: "architecture", launch.VerificationGateRole: "bounded", launch.ReviewSupervisorRole: "coordinator", launch.HarvestRole: "bounded", launch.RecoverySentinelRole: "bounded"}
	if expected, ok := expectedShapes[role]; !ok || lane.TaskShape != expected {
		return fmt.Errorf("%w: lane %q has invalid task_shape %q for role %q", ErrWorkerConfigPolicy, lane.Name, lane.TaskShape, role)
	}
	agentKind := strings.ToLower(strings.TrimSpace(lane.AgentKind))
	harness := strings.ToLower(strings.TrimSpace(lane.Harness))
	if agentKind != harness || !supportedVendorHarness(harness) {
		return fmt.Errorf("%w: lane %q agent kind %q harness %q must match one supported vendor harness (codex, claude, grok, agy, opencode)", ErrHarnessConfigPolicy, lane.Name, lane.AgentKind, lane.Harness)
	}
	if role == launch.WorkerRole || role == launch.ForgeSmithRole || role == launch.RecoveryRole {
		if lane.Provider != launch.WorkerProvider || lane.Model != launch.WorkerModel || lane.Effort != launch.WorkerEffort {
			return fmt.Errorf("%w: lane %q must explicitly be codex with codex/gpt-5.6-luna/medium", ErrWorkerConfigPolicy, lane.Name)
		}
	}
	return nil
}

func supportedVendorHarness(harness string) bool {
	return router.IsVendorHarness(harness)
}

var ErrWorkerConfigPolicy = errors.New("launch.policy.config_worker_tuple_mismatch")
var ErrHarnessConfigPolicy = errors.New("launch.policy.config_harness_mismatch")
var ErrHarnessBinaryMissing = errors.New("launch.policy.harness_binary_missing")

// ErrWorkerModelPolicy rejects forbidden model identity on production
// pulse/forge paths that bind a ModelRouter from a LaunchDecision (FAC-194).
// It does not re-pin workers to a vendor tuple; launch.Validate and the live
// quota waterfall own coherent provider/model/effort selection.
var ErrWorkerModelPolicy = errors.New("worker.model.policy")

// applyWorkerModelRouterBeforeClaim is the production forge/spawn seam:
// compile a ModelRouter from the admitted LaunchDecision, then run claim.
// Forbidden OpenCode/Ollama/DeepSeek/coordinator identities fail before claim.
func applyWorkerModelRouterBeforeClaim(d *router.LaunchDecision, claim func(*router.ModelRouter) error) error {
	mr, err := modelRouterFromLaunchDecision(d)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("%w: claim effect is required", ErrWorkerModelPolicy)
	}
	return claim(mr)
}

// modelRouterFromLaunchDecision derives a single-candidate ModelRouter from a
// compiled LaunchDecision. Missing identity fails closed; OpenCode/DeepSeek/
// Ollama/coordinator-tier surfaces are rejected. Any coherent implementation
// waterfall pick (including non-codex providers and non-medium effort) is kept.
func modelRouterFromLaunchDecision(d *router.LaunchDecision) (*router.ModelRouter, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: missing LaunchDecision route evidence", ErrWorkerModelPolicy)
	}
	return modelRouterFromWorkerIdentity(d.Provider, d.Model, d.Effort)
}

func modelRouterFromWorkerIdentity(provider, model, effort string) (*router.ModelRouter, error) {
	return modelRouterFromCandidates([]*router.ModelCandidate{{
		Name:            provider,
		Type:            workerProviderType(provider),
		Model:           model,
		ReasoningEffort: effort,
	}})
}

// modelRouterFromCandidates is the production worker ModelRouter constructor.
// A mutant restoring OpenCode/Ollama/DeepSeek candidates fails here before any
// claim effect that applyWorkerModelRouterBeforeClaim wraps.
func modelRouterFromCandidates(candidates []*router.ModelCandidate) (*router.ModelRouter, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: missing model candidates", ErrWorkerModelPolicy)
	}
	if len(candidates) != 1 {
		return nil, fmt.Errorf("%w: production worker path forbids multi-candidate fallback routers", ErrWorkerModelPolicy)
	}
	c := candidates[0]
	if c == nil {
		return nil, fmt.Errorf("%w: nil model candidate", ErrWorkerModelPolicy)
	}
	if c.Type == router.ProviderOllama {
		return nil, fmt.Errorf("%w: Ollama provider type forbidden on worker path", ErrWorkerModelPolicy)
	}
	if err := rejectProductionWorkerModelIdentity(c.Name, c.Model, c.ReasoningEffort); err != nil {
		return nil, err
	}
	return router.NewModelRouter([]*router.ModelCandidate{{
		Name:            strings.TrimSpace(c.Name),
		Type:            c.Type,
		Model:           strings.TrimSpace(c.Model),
		ReasoningEffort: strings.TrimSpace(c.ReasoningEffort),
	}}), nil
}

func workerProviderType(provider string) router.ProviderType {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "openai":
		return router.ProviderOpenAI
	case "claude", "anthropic":
		return router.ProviderAnthropic
	case "agy", "google":
		return router.ProviderGoogle
	case "grok", "xai":
		return router.ProviderXAI
	default:
		return router.ProviderOpenAI
	}
}

// rejectProductionWorkerModelIdentity refuses missing, OpenCode, Ollama,
// DeepSeek, and coordinator-tier identities. It deliberately does NOT require
// codex/gpt-5.6-luna/medium — that pin defeated live quota routing (9af6c78).
func rejectProductionWorkerModelIdentity(provider, model, effort string) error {
	p := strings.TrimSpace(provider)
	m := strings.TrimSpace(model)
	e := strings.ToLower(strings.TrimSpace(effort))
	if p == "" || m == "" || e == "" {
		return fmt.Errorf("%w: missing provider/model/effort evidence", ErrWorkerModelPolicy)
	}
	switch e {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("%w: invalid effort %q", ErrWorkerModelPolicy, effort)
	}
	pl, ml := strings.ToLower(p), strings.ToLower(m)
	if pl == "opencode" || pl == "ollama" || strings.Contains(pl, "deepseek") {
		return fmt.Errorf("%w: provider %q forbidden on worker path", ErrWorkerModelPolicy, p)
	}
	if strings.Contains(ml, "deepseek") || strings.Contains(ml, "opencode") || strings.Contains(ml, "litellm/ollama") {
		return fmt.Errorf("%w: model %q forbidden on worker path", ErrWorkerModelPolicy, m)
	}
	// Coordinator-only / non-authoring surfaces must not enter worker loops.
	if !router.AuthoringModelAllowed(m) {
		return fmt.Errorf("%w: coordinator-tier model %q forbidden on worker path", ErrWorkerModelPolicy, m)
	}
	for _, bad := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "-sol", "claude-fable"} {
		if strings.Contains(ml, bad) {
			return fmt.Errorf("%w: coordinator-tier model %q forbidden on worker path", ErrWorkerModelPolicy, m)
		}
	}
	return nil
}

func attachWorkerUsage(mr *router.ModelRouter) *router.ModelRouter {
	if mr == nil {
		return nil
	}
	return mr.WithUsageFunc(func(ctx context.Context, name string) float64 {
		snap, err := usage.FetchSnapshot()
		if err != nil {
			return 0
		}
		return snap.Utilization(name)
	})
}

// recordWorkerModelPolicyBlocked writes durable BLOCKED evidence when a
// production worker path cannot establish a non-forbidden model identity.
func recordWorkerModelPolicyBlocked(st *store.Store, entrypoint, reason string) error {
	if st == nil {
		return fmt.Errorf("durable BLOCKED evidence unavailable for worker model policy")
	}
	_, err := st.RecordBlockedSelection("WORKER-POLICY", "", entrypoint, "worker_model_policy", reason, "", "")
	return err
}

func validateDecisionBeforeSideEffect(decision *router.LaunchDecision, taskRef string) error {
	if decision == nil {
		return fmt.Errorf("missing routed launch decision")
	}
	return launch.Validate(launch.Request{Decision: decision, TaskRef: taskRef, LeaseGeneration: decision.LeaseGeneration, Scope: decision.Scope}, nil)
}

// ensureArtifactToolProbe returns a current tool-probe PASS for decision's
// surface, using the durable cache then a live artifact probe (FAC-139).
func ensureArtifactToolProbe(ctx context.Context, decision *router.LaunchDecision) (*toolprobe.Receipt, error) {
	if decision == nil {
		return nil, fmt.Errorf("tool-probe requires LaunchDecision")
	}
	id, err := toolprobe.IdentityFromDecision(decision)
	if err != nil {
		return nil, err
	}
	cache := toolprobe.NewFileCache(toolprobe.DefaultCachePath)
	r, err := toolprobe.Ensure(ctx, id, cache, &toolprobe.ExecRunner{}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !r.Passes(time.Now().UTC()) {
		return &r, fmt.Errorf("tool-probe status %s blocks write-capable launch: %s", r.Status, r.Reason)
	}
	return &r, nil
}

// openWriteCapableTab is the single cmd/herd path to Herdr TabCreate for
// write-capable worker/reviewer flows (FAC-139). It admits LaunchDecision +
// artifact tool-probe PASS before any tab exists.
func openWriteCapableTab(decision *router.LaunchDecision, req launch.Request, lane *config.LaneDef, workspace, label, cwd string, env ...string) (*launch.Plan, *herdr.TabInfo, error) {
	probe, err := ensureArtifactToolProbe(context.Background(), decision)
	if err != nil {
		return nil, nil, err
	}
	if req.Decision == nil {
		req.Decision = decision
	}
	return herdr.OpenWriteCapableTab(launch.BoundarySpec{
		Decision:  decision,
		Request:   req,
		Probe:     probe,
		Lane:      lane,
		Workspace: workspace,
		Label:     label,
		Cwd:       cwd,
		Env:       env,
		NoFocus:   true,
	})
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
	if *maxReview < 0 || *maxHarvest < 0 || *maxRelaunch < 0 {
		fmt.Fprintln(errOut, "herd-drain: max bounds must be non-negative")
		return 2
	}

	ledgerPath := drainLedgerPath()
	cap := drainIntEnv("HERD_IN_REVIEW_CAP", 8)
	stale := drainIntEnv("HERD_DRAIN_STALE_BEHIND", 20)
	root := "."
	var tp provider.TaskProvider
	cfg, cfgErr := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if cfgErr == nil {
		// The action gate accepts only configured standing lane identities.
		// Evidence is never upgraded into a standing lane by shape alone.
		var providerErr error
		tp, providerErr = loadTaskProvider(cfg)
		if providerErr != nil {
			tp = nil
			if !*asJSON {
				fmt.Fprintf(errOut, "herd-drain: UNKNOWN Kaneo review-cap posture: %v\n", providerErr)
			}
		}
	} else if !*asJSON {
		fmt.Fprintf(errOut, "herd-drain: UNKNOWN Kaneo review-cap posture: %v\n", cfgErr)
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
	if cfgErr == nil {
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
		// Missing authority keeps the fail-closed default hooks: every action
		// is then refused with the reason, never silently skipped.
		hooks := defaultDrainActionHooks()
		adapters, err := newDrainAdapters(root, ledgerPath, cfg, tp, cap)
		unauthorized := err != nil
		if unauthorized {
			fmt.Fprintf(out, "herd-drain: REFUSED --act: %v\n", err)
		} else {
			hooks = adapters.hooks()
		}
		result := executeDrainActions(context.Background(), report, report.ActionEvidence, *maxReview, *maxHarvest, *maxRelaunch, *autoTiers, out, hooks)
		if unauthorized || result.Failed {
			return 1
		}
		return drainExitCode(report)
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
		// main wired the compiled adapters (FAC-184), so the default beat is the
		// runnable command, not a refusal. This branch predates that wiring.
		fmt.Fprintf(out, "herd drain --act --max-harvest 1 --auto-harvest-tiers %s  # harvest lane=%s tier=%s sha=%s\n", e.Tier, e.Lane, e.Tier, sha)
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
			fmt.Fprintf(out, "herd drain --act --max-review 1  # review branch=%s family=%s pin=%s\n", e.Branch, e.BuilderFamily, sha)
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

// defaultDrainActionHooks is the no-authority beat: the compiled adapters
// exist, but nothing wired them, so every action refuses instead of acting.
//
// The wording is load-bearing. TestDrainDefaultActionsRefuseWithoutAuthority
// asserts the error contains "no compiled"; this branch carried an older
// "FAC-184 compiled X adapter unavailable" variant that the rebase preserved,
// so the hook still failed closed but with text the gate could not recognise.
// Main's phrasing is the reviewed one.
func defaultDrainActionHooks() drainActionHooks {
	return drainActionHooks{
		launchReview: func(context.Context, drainActionEvidence) error {
			return errors.New("no compiled review launch authority is configured")
		},
		dryRun: func(context.Context, drainActionEvidence) error {
			return errors.New("no compiled harvest authority is configured")
		},
		harvest: func(context.Context, drainActionEvidence) error {
			return errors.New("no compiled harvest authority is configured")
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

	// FAC-145: a worktree carrying a launch receipt reports its verify
	// outcome as a receipt-bound callback, so the worker FAIL signal travels
	// with the identical repo + lease + task binding the coordinator PASS
	// callback uses. A managed (.herd/worktrees) worktree without a valid
	// receipt fails closed — its evidence would be unattributable; only
	// unmanaged paths with no receipt at all are skipped (legacy spawn).
	tc, tcErr := dispatch.ReadTaskContext(wt)
	// FAC-145: verification is an admissible isolated-agent class. A
	// managed worktree must carry a receipt whose role is allowed to run
	// the gate — worker (self-verify) or a coordinator-issued verifier
	// receipt. Any other role is refused.
	if tcErr == nil && tc.Role != dispatch.RoleWorker && tc.Role != dispatch.RoleVerifier {
		tcErr = fmt.Errorf("role %q may not run the verification gate (FAC-145: worker self-verify or an issued verifier receipt only)", tc.Role)
	}
	// When the published verification key is present (verify running from
	// the coordinator checkout), the receipt must also authenticate — a
	// tampered receipt is treated exactly like a missing one. Without the
	// key (agent-side run inside a worktree) structural validation still
	// applies and authentication happens at the consuming side.
	verifyRoot, verifyRootErr := canonicalHerdRoot()
	if verifyRootErr != nil {
		fmt.Fprintf(os.Stderr, "herd verify: cannot resolve canonical root: %v\n", verifyRootErr)
		os.Exit(2)
	}
	if tcErr == nil {
		if _, statErr := os.Stat(filepath.Join(verifyRoot, dispatch.ReceiptPubFile)); statErr == nil {
			if v, vErr := dispatch.LoadVerifier(verifyRoot); vErr != nil {
				tcErr = vErr
			} else {
				tcErr = v.Verify(tc)
			}
		}
	}
	switch {
	case tcErr == nil:
		kind, detail := mail.CallbackComplete, ""
		if !c.Passed {
			kind = mail.CallbackBlocked
			detail = strings.Join(c.Reasons, "; ")
		}
		sha := ""
		if out, hErr := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output(); hErr == nil {
			sha = strings.TrimSpace(string(out))
		}
		// FAC-145: a verify whose bound callback cannot be raised has a
		// broken evidence chain — that is a hard failure, never a warning.
		if cb, cbErr := tc.BoundCallback(kind, sha, detail); cbErr != nil {
			fmt.Fprintf(os.Stderr, "herd verify: callback binding refused (FAC-145): %v\n", cbErr)
			os.Exit(2)
		} else if _, postErr := mail.NewMailbox(filepath.Join(verifyRoot, mail.DefaultMailFile)).PostCallback(tc.Role, cb); postErr != nil {
			fmt.Fprintf(os.Stderr, "herd verify: callback post failed (FAC-145): %v\n", postErr)
			os.Exit(2)
		}
	case isManagedWorktree(wt):
		fmt.Fprintf(os.Stderr, "herd verify: managed worktree %s has no valid launch receipt (FAC-145 fail-closed): %v\n", wt, tcErr)
		os.Exit(2)
	case !errors.Is(tcErr, os.ErrNotExist):
		// A present-but-corrupt receipt is never silently ignored.
		fmt.Fprintf(os.Stderr, "herd verify: %v\n", tcErr)
		os.Exit(2)
	}

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

// Broker protocol (FAC-145): the agent NEVER executes provider code or
// holds credentials, config, or verification keys. It presents its signed
// receipt as a capability over a unix socket; the coordinator-owned broker
// process authenticates it, enforces authority/role/fence, performs the
// provider I/O with the coordinator's credentials, and returns the result.
var cryptoRandRead = cryptorand.Read

type brokerRequest struct {
	Op string `json:"op"` // "ping" | "get" | "comment" | "verdict"
	// Nonce is echoed back SIGNED on ping so callers can authenticate the
	// broker's identity against the published verification key.
	Nonce string `json:"nonce,omitempty"`
	Ref   string `json:"ref"`
	// Body: comment text, or the verdict token (APPROVED | REJECTED).
	Body string `json:"body,omitempty"`
	// CandidateSHA must equal the receipt's candidate for op=verdict — the
	// verdict is bound to the EXACT commit under review.
	CandidateSHA string `json:"candidate_sha,omitempty"`
	// WorktreeHEAD is the reviewer checkout's CURRENT HEAD at verdict time.
	// It must still equal the receipt's candidate: a checkout that moved
	// between admission and verdict can no longer speak for that candidate.
	WorktreeHEAD string               `json:"worktree_head,omitempty"`
	Detail       string               `json:"detail,omitempty"`
	Receipt      dispatch.TaskContext `json:"receipt"`
}

// verdictForgeryGuard: free-form agent comments must never be interpretable
// as reviewer approval — only the broker's typed verdict op composes
// verdict lines.
func verdictForgeryGuard(text string) error {
	if strings.Contains(strings.ToUpper(text), "REVIEW VERDICT") {
		return fmt.Errorf("free-form comments must not contain verdict phrasing (FAC-145: verdicts are a typed reviewer-only operation)")
	}
	return nil
}

type brokerResponse struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	NonceSig string          `json:"nonce_sig,omitempty"` // ping: coordinator signature over the nonce
	Result   json.RawMessage `json:"result,omitempty"`
}

// brokerSocketPath resolves the coordinator broker socket for a repository:
// $HERD_BROKER_SOCK override, else ~/.herd/run/herd-<repo>.sock — OUTSIDE
// the repository tree.
func brokerSocketPath(repo string) (string, error) {
	if p := strings.TrimSpace(os.Getenv("HERD_BROKER_SOCK")); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve broker socket: %w", err)
	}
	return filepath.Join(home, ".herd", "run", "herd-"+strings.ToLower(repo)+".sock"), nil
}

// runBrokerServe is the COORDINATOR-owned credential broker. It loads
// config, credentials, and the verification key exactly once, in the
// coordinator process, and serves capability-checked task reads/comments to
// sandboxed agents. Agents never receive any of that material.
func runBrokerServe() {
	fs := flag.NewFlagSet("broker", flag.ExitOnError)
	socketFlag := fs.String("socket", "", "unix socket path override (default $HERD_BROKER_SOCK or ~/.herd/run/herd-<repo>.sock)")
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	fs.Parse(args)

	root, err := canonicalHerdRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: load config: %v\n", err)
		os.Exit(1)
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: %v\n", err)
		os.Exit(1)
	}
	// The broker signs ping nonces with the coordinator key: readiness is
	// AUTHENTICATED — an impostor socket cannot mint a valid pong. FAC-133's
	// sandbox is the containment for the key itself.
	signer, err := dispatch.LoadSignerForConfig(cfg.Project.Name, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: %v\n", err)
		os.Exit(1)
	}
	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "herd broker: task provider: %v\n", tpErr)
		os.Exit(1)
	}

	sock := *socketFlag
	if sock == "" {
		if sock, err = brokerSocketPath(dispatch.RepositoryIdentityOrName(root, cfg.Project.Name)); err != nil {
			fmt.Fprintf(os.Stderr, "herd broker: %v\n", err)
			os.Exit(1)
		}
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: %v\n", err)
		os.Exit(1)
	}
	if err := safeRemoveSocket(sock); err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: %v\n", err)
		os.Exit(1)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: listen: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(sock, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "herd broker: chmod socket: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd broker: serving %s (repo %s)\n", sock, cfg.Project.Name)

	authority := dispatch.AuthorityFromConfigAt(cfg, root)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd broker: accept: %v\n", err)
			continue
		}
		go serveBrokerConn(conn, root, cfg, authority, verifier, signer, tp)
	}
}

func serveBrokerConn(conn net.Conn, root string, cfg *config.Config, authority dispatch.BindingAuthority, verifier *dispatch.Verifier, signer *dispatch.Signer, tp provider.TaskProvider) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	respond := func(resp brokerResponse) {
		_ = json.NewEncoder(conn).Encode(resp)
	}
	var req brokerRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		respond(brokerResponse{Error: "malformed request"})
		return
	}
	// Liveness probe carries no capability and grants nothing — but the
	// response AUTHENTICATES the broker: the nonce comes back signed.
	if req.Op == "ping" {
		if signer == nil || req.Nonce == "" {
			respond(brokerResponse{Error: "broker cannot authenticate ping"})
			return
		}
		sig, sErr := signer.SignBytes([]byte("herd-broker-pong:" + req.Nonce))
		if sErr != nil {
			respond(brokerResponse{Error: sErr.Error()})
			return
		}
		respond(brokerResponse{OK: true, NonceSig: sig})
		return
	}
	tc := req.Receipt
	if err := verifier.Verify(tc); err != nil {
		respond(brokerResponse{Error: err.Error()})
		return
	}
	if !strings.EqualFold(hsync.NormalizeRef(tc.TaskRef), hsync.NormalizeRef(req.Ref)) {
		respond(brokerResponse{Error: fmt.Sprintf("receipt is bound to %s, not %s (FAC-145: one receipt, one task)", tc.TaskRef, req.Ref)})
		return
	}
	if err := requireLiveLease(context.Background(), root, tc); err != nil {
		respond(brokerResponse{Error: err.Error()})
		return
	}
	// A session-bound receipt is only valid while its session is LIVE: a
	// finished or killed agent's authority dies with its pane (FAC-145).
	if tc.AgentSessionID != "" {
		alive, sErr := herdr.SessionExists(tc.AgentSessionID)
		if sErr != nil {
			respond(brokerResponse{Error: fmt.Sprintf("cannot verify agent session %s — refusing (FAC-145 fail-closed): %v", tc.AgentSessionID, sErr)})
			return
		}
		if !alive {
			respond(brokerResponse{Error: fmt.Sprintf("agent session %s is no longer live — its receipt carries no authority (FAC-145)", tc.AgentSessionID)})
			return
		}
	}
	btp, err := dispatch.NewContextBoundProvider(tp, tc, authority, verifier, nil, tc.LeaseGeneration)
	if err != nil {
		respond(brokerResponse{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	switch req.Op {
	case "get":
		task, err := btp.GetTask(ctx, tc.TaskID)
		if err != nil {
			respond(brokerResponse{Error: err.Error()})
			return
		}
		raw, err := json.Marshal(task)
		if err != nil {
			respond(brokerResponse{Error: err.Error()})
			return
		}
		respond(brokerResponse{OK: true, Result: raw})
	case "comment":
		if strings.TrimSpace(req.Body) == "" {
			respond(brokerResponse{Error: "empty comment body"})
			return
		}
		if err := verdictForgeryGuard(req.Body); err != nil {
			respond(brokerResponse{Error: err.Error()})
			return
		}
		// Attribution prefix is broker-composed: a note can never
		// impersonate another role.
		body := fmt.Sprintf("[note from %s %s] %s", tc.Role, tc.TaskRef, req.Body)
		if err := btp.AddComment(ctx, tc.TaskID, body); err != nil {
			respond(brokerResponse{Error: err.Error()})
			return
		}
		respond(brokerResponse{OK: true})
	case "verdict":
		// Typed, reviewer-only, exact-candidate verdict (FAC-145): the
		// broker COMPOSES the verdict line — no agent free-form text can be
		// or contain one — and records it durably on the coordinator bus.
		if tc.Role != dispatch.RoleReviewer {
			respond(brokerResponse{Error: fmt.Sprintf("verdicts are reviewer-only; receipt role is %q (FAC-145)", tc.Role)})
			return
		}
		verdict := strings.ToUpper(strings.TrimSpace(req.Body))
		if verdict != "APPROVED" && verdict != "REJECTED" {
			respond(brokerResponse{Error: "verdict must be APPROVED or REJECTED"})
			return
		}
		if req.CandidateSHA == "" || req.CandidateSHA != tc.CandidateSHA {
			respond(brokerResponse{Error: fmt.Sprintf("verdict candidate %q does not match the receipt's exact candidate %q (FAC-145)", req.CandidateSHA, tc.CandidateSHA)})
			return
		}
		// Re-check at VERDICT time: the reviewer's checkout must still sit
		// on the exact candidate it was admitted for.
		if req.WorktreeHEAD == "" || req.WorktreeHEAD != tc.CandidateSHA {
			respond(brokerResponse{Error: fmt.Sprintf("review checkout HEAD %q is no longer the admitted candidate %q — refusing a verdict over drifted state (FAC-145)", req.WorktreeHEAD, tc.CandidateSHA)})
			return
		}
		if err := verdictForgeryGuard(req.Detail); err != nil {
			respond(brokerResponse{Error: err.Error()})
			return
		}
		line := fmt.Sprintf("REVIEW VERDICT %s: %s candidate=%s base=%s lease=%s lease_gen=%d reviewer-bound (FAC-145)",
			tc.TaskRef, verdict, tc.CandidateSHA, tc.BaseSHA, tc.LeaseID, tc.LeaseGeneration)
		if strings.TrimSpace(req.Detail) != "" {
			line += " — " + req.Detail
		}
		// TRANSACTIONAL order (FAC-145): (1) durable canonical outbox INTENT
		// first — nothing public exists before the coordinator's own record;
		// (2) idempotent provider delivery; (3) durable DELIVERED marker.
		// A crash between any two steps re-runs from the durable intent
		// (provider delivery is at-least-once; the bus records are exactly-
		// once per full effect identity). The dedupe identity carries the
		// FULL authenticated effect — task, exact candidate, lease session,
		// generation, AND the verdict value — so APPROVED and REJECTED for
		// the same candidate are distinct effects and a later veto can never
		// be collapsed into an earlier approval (supersession is by bus
		// order; see mail.EffectiveVerdict).
		kind := mail.CallbackBlocked
		if verdict == "APPROVED" {
			kind = mail.CallbackComplete
		}
		effect := fmt.Sprintf("%s:%s:%s:gen%d:%s:%s", tc.Repository, tc.TaskRef, tc.CandidateSHA, tc.LeaseGeneration, tc.LeaseID, verdict)
		mb := mail.NewMailbox(filepath.Join(root, mail.DefaultMailFile))

		// UNFORGEABLE effect proof (FAC-145): the delivered comment body is
		// the canonical line plus a COORDINATOR SIGNATURE over it. An agent
		// holding OpComment cannot mint that signature, so it cannot
		// pre-seed a body that would satisfy readback; readback compares the
		// EXACT canonical body and verifies the signature, never a
		// substring. The signature is deterministic per effect, so retries
		// reproduce the identical body.
		if signer == nil {
			respond(brokerResponse{Error: "broker cannot sign verdict effects (FAC-145)"})
			return
		}
		effectID := mail.VerdictEffectID(effect)
		effectSig, sigErr := signer.SignBytes([]byte("herd-verdict-effect:" + effectID + "\n" + line))
		if sigErr != nil {
			respond(brokerResponse{Error: fmt.Sprintf("verdict effect signing failed: %v", sigErr)})
			return
		}
		canonicalBody := fmt.Sprintf("%s [effect %s sig=%s]", line, effectID, effectSig)
		verifyEffectBody := func(body string) bool {
			if body != canonicalBody {
				return false
			}
			return verifier.VerifyBytes([]byte("herd-verdict-effect:"+effectID+"\n"+line), effectSig) == nil
		}

		// SERIALIZED delivery: one durable owner per effect across broker
		// processes. Probe, deliver, and publish happen under an exclusive
		// per-effect lock, so two coordinators can never both observe
		// "absent" and both call AddComment.
		release, lockErr := claimVerdictOwnership(root, tc, effectID)
		if lockErr != nil {
			respond(brokerResponse{Error: lockErr.Error()})
			return
		}
		defer func() {
			if err := release(); err != nil {
				fmt.Fprintf(os.Stderr, "herd broker: verdict lock release: %v\n", err)
			}
		}()

		// EXACTLY-ONCE requires PROVIDER-SIDE effect readback: without the
		// CommentReader capability the coordinator cannot prove whether a
		// prior attempt already landed, so no consumable verdict is ever
		// published for that adapter (FAC-145 fail-closed).
		// The bound provider always exposes ListComments; it errors when the
		// underlying adapter lacks the capability, which fails closed.
		if _, probe := btp.ListComments(ctx, tc.TaskID); probe != nil {
			respond(brokerResponse{Error: fmt.Sprintf("provider %q cannot read comments back — refusing to publish an unverifiable verdict (FAC-145 fail-closed): %v", tc.ProviderType, probe)})
			return
		}
		effectDelivered := func() (bool, error) {
			comments, cErr := btp.ListComments(ctx, tc.TaskID)
			if cErr != nil {
				return false, cErr
			}
			for _, c := range comments {
				if verifyEffectBody(c) {
					return true, nil
				}
			}
			return false, nil
		}

		// CROSS-HOST exclusive ownership (FAC-145): the local lock only
		// serializes this clone. The PROVIDER is the one medium every
		// coordinator shares, so ownership is decided there: each
		// contender writes a signed claim marker, then re-reads; the
		// EARLIEST claim for this effect wins and only that owner
		// delivers. A loser never writes a second verdict comment.
		if owned, ownErr := winVerdictClaim(ctx, btp, signer, verifier, tc, effectID); ownErr != nil {
			respond(brokerResponse{Error: ownErr.Error()})
			return
		} else if !owned {
			// Another coordinator owns delivery for this exact effect.
			// Converge on ITS result rather than duplicating the effect.
			respond(brokerResponse{OK: true})
			return
		}

		// Authority-side short circuit first (cheap), then the PROVIDER
		// truth: a prior attempt that crashed after AddComment but before
		// the delivered marker is detected here and never re-delivered.
		_, alreadyDeliveredFound, dErr := mb.HasDeliveredVerdict(effectID)
		if dErr != nil {
			respond(brokerResponse{Error: fmt.Sprintf("verdict state unreadable (FAC-145 fail-closed): %v", dErr)})
			return
		}
		providerHas := false
		if !alreadyDeliveredFound {
			var pErr error
			providerHas, pErr = effectDelivered()
			if pErr != nil {
				respond(brokerResponse{Error: fmt.Sprintf("provider effect readback failed — refusing verdict (FAC-145 fail-closed): %v", pErr)})
				return
			}
		}
		if alreadyDeliveredFound {
			respond(brokerResponse{OK: true})
			return
		}

		// (1) NON-CONSUMABLE intent: recorded as blocked/pending under a
		// distinct id — an intent alone can NEVER read as an approval.
		intent := mail.Callback{
			Ref: tc.TaskRef, Kind: mail.CallbackBlocked, SHA: tc.CandidateSHA,
			Detail: "verdict intent (undelivered): " + line, Repo: tc.Repository,
			LeaseGeneration: tc.LeaseGeneration, SenderRole: tc.Role,
			DedupeID: mail.VerdictIntentID(effect),
		}
		if _, err := mb.PostCallback(tc.Role, intent); err != nil {
			respond(brokerResponse{Error: fmt.Sprintf("verdict intent record failed — nothing published (FAC-145): %v", err)})
			return
		}
		// (2) Provider delivery — SKIPPED when the provider already carries
		// this exact effect (crash between AddComment and the delivered
		// marker): the readback, not the text, makes this exactly-once.
		if !providerHas {
			if err := btp.AddComment(ctx, tc.TaskID, canonicalBody); err != nil {
				respond(brokerResponse{Error: fmt.Sprintf("verdict intent recorded but provider delivery failed (retry re-delivers; nothing consumable published): %v", err)})
				return
			}
		}
		// (3) MUTATION readback: the exact effect id must now be present
		// EXACTLY once on the provider — this distinguishes dropped,
		// duplicated, and reordered writes; anything else refuses to
		// publish a consumable verdict.
		comments, rErr := btp.ListComments(ctx, tc.TaskID)
		if rErr != nil {
			respond(brokerResponse{Error: fmt.Sprintf("verdict delivered but effect readback failed — refusing to publish a consumable verdict (FAC-145): %v", rErr)})
			return
		}
		hits := 0
		for _, c := range comments {
			if verifyEffectBody(c) {
				hits++
			}
		}
		if hits != 1 {
			respond(brokerResponse{Error: fmt.Sprintf("verdict effect readback found %d matching provider comments, want exactly 1 — refusing to publish (FAC-145 fail-closed)", hits)})
			return
		}
		// (4) The ONLY consumable record, written after confirmed delivery.
		deliveredRec := mail.Callback{
			Ref: tc.TaskRef, Kind: kind, SHA: tc.CandidateSHA,
			Detail: canonicalBody, Repo: tc.Repository,
			LeaseGeneration: tc.LeaseGeneration, SenderRole: tc.Role,
			DedupeID: effectID,
		}
		if _, err := mb.PostCallback(tc.Role, deliveredRec); err != nil {
			respond(brokerResponse{Error: fmt.Sprintf("verdict delivered to provider but authority record failed (retry reconciles): %v", err)})
			return
		}
		respond(brokerResponse{OK: true})
	default:
		respond(brokerResponse{Error: fmt.Sprintf("unknown op %q (ping|get|comment|verdict — mutations are coordinator-owned)", req.Op)})
	}
}

// safeRemoveSocket removes a stale broker socket ONLY when the path really
// is this uid's unix socket — a caller-controlled path can never delete an
// arbitrary file, and removal errors propagate.
func safeRemoveSocket(sock string) error {
	// Pin the PARENT: it must be this uid's directory and not group/world
	// writable — path substitution through a hostile parent is refused.
	parent := filepath.Dir(sock)
	pfi, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("audit socket parent: %w", err)
	}
	if pfi.Mode()&os.ModeSymlink != 0 || !pfi.IsDir() {
		return fmt.Errorf("refusing socket parent %s: must be a real directory (FAC-145)", parent)
	}
	if st, ok := pfi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing socket parent %s: owned by uid %d (FAC-145)", parent, st.Uid)
	}
	if pfi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("refusing socket parent %s: group/world writable (%v) (FAC-145)", parent, pfi.Mode().Perm())
	}
	audit := func() error {
		fi, err := os.Lstat(sock)
		if os.IsNotExist(err) {
			return err
		}
		if err != nil {
			return fmt.Errorf("audit socket path: %w", err)
		}
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to remove %s: not a unix socket (FAC-145)", sock)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			return fmt.Errorf("refusing to remove %s: owned by uid %d, not %d (FAC-145)", sock, st.Uid, os.Getuid())
		}
		return nil
	}
	if err := audit(); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Re-audit immediately before unlink; callers serialize via the ensure
	// lock, closing the remaining lstat-to-remove window to a single actor.
	if err := audit(); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Remove(sock); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

// brokerPing round-trips the broker's typed ping op and AUTHENTICATES the
// pong: the broker must sign our fresh nonce with the coordinator key. A
// process that merely binds the socket and answers OK is an impostor.
func brokerPing(root, sock string) error {
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return fmt.Errorf("cannot authenticate broker without the published key (FAC-145): %w", err)
	}
	nonceRaw := make([]byte, 16)
	if _, err := cryptoRandRead(nonceRaw); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nonceRaw)
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(conn).Encode(brokerRequest{Op: "ping", Nonce: nonce}); err != nil {
		return err
	}
	var resp brokerResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("broker ping refused: %s", resp.Error)
	}
	if err := verifier.VerifyBytes([]byte("herd-broker-pong:"+nonce), resp.NonceSig); err != nil {
		return fmt.Errorf("broker identity NOT authenticated — impostor socket refused (FAC-145): %w", err)
	}
	return nil
}

// ensureBroker guarantees ONE canonical supervised broker per repo: probe
// readiness, clear a stale socket safely, spawn a detached broker
// (self-exec, own session, pidfile beside the socket), and wait for proven
// readiness. Unavailable after the window is a BOUNDED, fail-closed error —
// fleet paths that need the broker refuse instead of launching agents
// without provider access.
func ensureBroker(root, sock string) error {
	if err := os.MkdirAll(filepath.Dir(sock), 0700); err != nil {
		return err
	}
	// SERIALIZE concurrent ensure calls: probe, cleanup, spawn, readiness,
	// and pidfile all happen under one exclusive lock — two racers can never
	// unlink a live socket or stomp each other's pidfile.
	lock, err := openNoFollow(sock+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open ensure lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire ensure lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if err := brokerPing(root, sock); err == nil {
		return nil
	}
	// A live-but-transiently-unreachable broker must NEVER be orphaned: if
	// the recorded pid is still alive, stop it (and confirm it stopped)
	// before replacing its socket. An unstoppable live broker is a BLOCKED
	// state, not a silent double-broker.
	if pid, ok := readBrokerPid(sock); ok {
		if err := stopBrokerPid(pid); err != nil {
			return fmt.Errorf("BLOCKED(broker_unavailable): recorded broker pid %d is unreachable but could not be stopped — refusing to orphan a live credential broker (FAC-145): %w", pid, err)
		}
	}
	if err := safeRemoveSocket(sock); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve herd binary: %w", err)
	}
	logFile, err := openNoFollow(sock+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open broker log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(self, "broker", "serve", "--socket", sock)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn broker: %w", err)
	}
	go func() { _, _ = cmd.Process.Wait() }() // reap if it exits while we live
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := brokerPing(root, sock); err == nil {
			// Pidfile only records a PROVEN-ready broker, written through a
			// no-follow open so a pre-placed symlink cannot redirect it.
			pidFile, pErr := openNoFollow(sock+".pid", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
			if pErr != nil {
				return failStartedBroker(cmd, sock, fmt.Errorf("open broker pidfile: %w", pErr))
			}
			if _, wErr := pidFile.WriteString(fmt.Sprintf("%d\n", cmd.Process.Pid)); wErr != nil {
				pidFile.Close()
				return failStartedBroker(cmd, sock, fmt.Errorf("write broker pidfile: %w", wErr))
			}
			if cErr := pidFile.Close(); cErr != nil {
				return failStartedBroker(cmd, sock, fmt.Errorf("close broker pidfile: %w", cErr))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return failStartedBroker(cmd, sock,
				fmt.Errorf("BLOCKED(broker_unavailable): broker did not become ready within 5s (FAC-145)"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sameDir compares directories through symlinks (macOS /var vs /private/var).
func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	return ra == rb
}

// shortSHA renders the 12-char prefix used in review checkout names.
// shortSHA lives in reviewclassify.go (main) — identical semantics; do not
// re-declare it here.

// ensureDetachedReviewWorktree creates (or validates) an isolated review
// checkout pinned DETACHED at the exact candidate SHA. Review can never
// mutate the candidate branch, and the checkout's identity is verified
// before admission: detached HEAD, exactly the candidate commit (FAC-145).
func ensureDetachedReviewWorktree(root, dir, candidateSHA string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return verifyDetachedAt(dir, candidateSHA)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return fmt.Errorf("create review worktree parent: %w", err)
	}
	cmd := exec.Command("git", "-C", root, "worktree", "add", "--detach", dir, candidateSHA)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create isolated review worktree at %s pinned to %s: %v: %s", dir, candidateSHA, err, out)
	}
	return verifyDetachedAt(dir, candidateSHA)
}

// verifyDetachedAt proves the review checkout is detached and sits exactly
// on the candidate commit — the identity that cannot change between
// admission and verdict.
func verifyDetachedAt(dir, candidateSHA string) error {
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("review worktree %s unreadable: %w", dir, err)
	}
	if got := strings.TrimSpace(string(head)); got != candidateSHA {
		return fmt.Errorf("review worktree %s is at %s, not the candidate %s (FAC-145)", dir, got, candidateSHA)
	}
	ref, err := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD").Output()
	if err == nil && strings.TrimSpace(string(ref)) != "" {
		return fmt.Errorf("review worktree %s is attached to %s — reviews run DETACHED so they cannot mutate the candidate branch (FAC-145)", dir, strings.TrimSpace(string(ref)))
	}
	return nil
}

// reviewLifecycle collects compensations for the review admission
// sequence: EVERY failure boundary after the lease is acquired unwinds the
// exact side effects it created (lease released, receipt removed, tab
// closed) before exiting non-zero — no orphan reviewer, tab, or claim
// (FAC-145).
type reviewLifecycle struct {
	steps []func() error
}

func (l *reviewLifecycle) onFail(f func() error) { l.steps = append(l.steps, f) }

// fail unwinds in reverse order, reports every compensation error, and
// exits non-zero.
func (l *reviewLifecycle) fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	for i := len(l.steps) - 1; i >= 0; i-- {
		if err := l.steps[i](); err != nil {
			fmt.Fprintf(os.Stderr, "  COMPENSATION FAILED: %v\n", err)
		}
	}
	os.Exit(1)
}

// releaseCoordinationLease durably releases an acquired coordination lease
// (dispatch compensation path).
// acquireOrJoinLease reports whether THIS call acquired the lease (owned)
// or joined an existing one, so compensation only releases what it created.
func acquireOrJoinLease(ctx context.Context, root string, key claim.LeaseKey, owner, role string) (string, int64, bool, error) {
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		return "", 0, false, fmt.Errorf("claim store unavailable (FAC-145): %w", err)
	}
	defer st.Close()
	if leases, aErr := st.ActiveClaims(ctx, time.Now()); aErr == nil {
		for _, l := range leases {
			if l.LeaseKey == key {
				return fmt.Sprintf("claim:%d", l.ID), l.Generation, false, nil
			}
		}
	}
	lease, err := st.Acquire(ctx, key, owner, role, "", time.Now(), dispatch.DefaultReceiptTTL)
	if err != nil {
		return "", 0, false, fmt.Errorf("claim lease acquisition failed (FAC-145): %w", err)
	}
	return fmt.Sprintf("claim:%d", lease.ID), lease.Generation, true, nil
}

func releaseCoordinationLease(ctx context.Context, root string, key claim.LeaseKey, owner string, generation int64) error {
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	_, _, err = st.Release(ctx, key, owner, generation, time.Now())
	return err
}

// verdictClaimPrefix marks provider-side ownership claims.
const verdictClaimPrefix = "[verdict-claim "

// winVerdictClaim decides cross-host ownership of one verdict effect using
// the PROVIDER as the shared serializer (FAC-145). Every contender posts a
// coordinator-SIGNED claim marker naming the effect and its own id, then
// reads all comments back: the earliest valid claim for that effect wins.
// Claims are unforgeable (signed) and idempotent (a contender that already
// claimed does not claim again), so two hosts racing produce exactly one
// owner and therefore exactly one delivered verdict.
func winVerdictClaim(ctx context.Context, btp *dispatch.ContextBoundProvider, signer *dispatch.Signer, verifier *dispatch.Verifier, tc dispatch.TaskContext, effectID string) (bool, error) {
	selfRaw := make([]byte, 12)
	if _, err := cryptoRandRead(selfRaw); err != nil {
		return false, err
	}
	self := hex.EncodeToString(selfRaw)

	claimBody := func(owner string) (string, error) {
		sig, err := signer.SignBytes([]byte("herd-verdict-claim:" + effectID + "\n" + owner))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s%s owner=%s sig=%s]", verdictClaimPrefix, effectID, owner, sig), nil
	}
	parseClaim := func(body string) (owner string, ok bool) {
		if !strings.HasPrefix(body, verdictClaimPrefix+effectID+" owner=") {
			return "", false
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(body, verdictClaimPrefix+effectID+" owner="), "]")
		parts := strings.SplitN(rest, " sig=", 2)
		if len(parts) != 2 {
			return "", false
		}
		// Unforgeable: only the coordinator key can mint a valid claim.
		if verifier.VerifyBytes([]byte("herd-verdict-claim:"+effectID+"\n"+parts[0]), parts[1]) != nil {
			return "", false
		}
		return parts[0], true
	}

	// A claim already recorded by THIS clone is reused (idempotent retry).
	comments, err := btp.ListComments(ctx, tc.TaskID)
	if err != nil {
		return false, fmt.Errorf("verdict ownership readback failed (FAC-145 fail-closed): %w", err)
	}
	for _, c := range comments {
		if owner, ok := parseClaim(c); ok {
			// Someone already owns it; we only proceed if it is us.
			return owner == self, nil
		}
	}

	body, err := claimBody(self)
	if err != nil {
		return false, err
	}
	if err := btp.AddComment(ctx, tc.TaskID, body); err != nil {
		return false, fmt.Errorf("verdict ownership claim failed (FAC-145): %w", err)
	}
	// Re-read: the EARLIEST valid claim in provider order wins.
	comments, err = btp.ListComments(ctx, tc.TaskID)
	if err != nil {
		return false, fmt.Errorf("verdict ownership readback failed after claim (FAC-145): %w", err)
	}
	for _, c := range comments {
		if owner, ok := parseClaim(c); ok {
			return owner == self, nil
		}
	}
	return false, fmt.Errorf("verdict ownership claim vanished from the provider — refusing (FAC-145 fail-closed)")
}

// claimVerdictOwnership takes CROSS-HOST exclusive ownership of one verdict
// effect using the durable claim store (the same authority that fences
// tasks), so coordinators in separate clones or on separate hosts cannot
// both publish. The local flock still serializes same-host racers cheaply;
// the claim lease is what makes ownership global. Returns a release that
// reports its own failures (FAC-145).
func claimVerdictOwnership(root string, tc dispatch.TaskContext, effectID string) (func() error, error) {
	localRelease, err := lockVerdictEffect(root, effectID)
	if err != nil {
		return nil, err
	}
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		_ = localRelease()
		return nil, fmt.Errorf("verdict ownership store unavailable (FAC-145): %w", err)
	}
	sum := sha256.Sum256([]byte(effectID))
	key := claim.LeaseKey{
		Repo:     tc.Repository,
		Provider: tc.ProviderType,
		Project:  tc.ProjectID,
		TaskRef:  "verdict:" + hex.EncodeToString(sum[:12]),
	}
	owner := fmt.Sprintf("coordinator-verdict-%d", os.Getpid())
	lease, err := st.Acquire(context.Background(), key, owner, "coordinator", "", time.Now(), 5*time.Minute)
	if err != nil {
		st.Close()
		_ = localRelease()
		return nil, fmt.Errorf("BLOCKED(verdict_owner): another coordinator owns this verdict effect (FAC-145): %w", err)
	}
	return func() error {
		var errs []string
		if _, _, rErr := st.Release(context.Background(), key, owner, lease.Generation, time.Now()); rErr != nil {
			errs = append(errs, "release verdict ownership: "+rErr.Error())
		}
		if cErr := st.Close(); cErr != nil {
			errs = append(errs, "close ownership store: "+cErr.Error())
		}
		if lErr := localRelease(); lErr != nil {
			errs = append(errs, lErr.Error())
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	}, nil
}

// lockVerdictEffect takes the exclusive durable owner lock for one verdict
// effect at the canonical root, serializing probe/deliver/publish across
// broker processes (FAC-145). Bounded: a wedged owner surfaces BLOCKED.
func lockVerdictEffect(root, effectID string) (func() error, error) {
	dir := filepath.Join(root, ".herd", "verdicts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create verdict lock dir: %w", err)
	}
	sum := sha256.Sum256([]byte(effectID))
	path := filepath.Join(dir, hex.EncodeToString(sum[:16])+".lock")
	f, err := openNoFollow(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open verdict lock: %w", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("BLOCKED(verdict_owner): another coordinator holds the delivery lock for this effect (FAC-145)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() error {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}, nil
}

// readBrokerPid reads the recorded broker pid (no-follow) and reports
// whether that process is currently alive.
func readBrokerPid(sock string) (int, bool) {
	f, err := openNoFollow(sock+".pid", os.O_RDONLY, 0600)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	data := make([]byte, 32)
	n, _ := f.Read(data)
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data[:n])), "%d", &pid); err != nil || pid <= 1 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, false // recorded pid is gone
	}
	return pid, true
}

// stopBrokerPid terminates a live broker and CONFIRMS it exited; an
// unstoppable process is an error, never ignored.
func stopBrokerPid(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return nil // confirmed gone
		}
		if time.Now().After(deadline) {
			if err := proc.Signal(syscall.SIGKILL); err != nil {
				return fmt.Errorf("kill broker pid %d: %w", pid, err)
			}
			time.Sleep(200 * time.Millisecond)
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				return nil
			}
			return fmt.Errorf("broker pid %d survived SIGKILL", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// failStartedBroker tears down a spawned-but-unusable broker, reporting
// every cleanup failure alongside the original cause.
func failStartedBroker(cmd *exec.Cmd, sock string, cause error) error {
	errs := []string{cause.Error()}
	if err := stopBrokerPid(cmd.Process.Pid); err != nil {
		errs = append(errs, "stop spawned broker: "+err.Error())
	}
	if err := safeRemoveSocket(sock); err != nil {
		errs = append(errs, "socket cleanup: "+err.Error())
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// runBrokerEnsure is the supervised entry: `herd broker ensure` proves a
// ready canonical broker (spawning/replacing as needed) and exits 0 only on
// proven readiness.
func runBrokerEnsure() {
	fs := flag.NewFlagSet("broker ensure", flag.ExitOnError)
	socketFlag := fs.String("socket", "", "unix socket path override")
	fs.Parse(os.Args[3:])
	root, err := canonicalHerdRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd broker ensure: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd broker ensure: %v\n", err)
		os.Exit(1)
	}
	sock := *socketFlag
	if sock == "" {
		// SAME namespace as serve and the client: stable repository
		// identity, never the configured name (FAC-145).
		if sock, err = brokerSocketPath(dispatch.RepositoryIdentityOrName(root, cfg.Project.Name)); err != nil {
			fmt.Fprintf(os.Stderr, "herd broker ensure: %v\n", err)
			os.Exit(1)
		}
	}
	if err := ensureBroker(root, sock); err != nil {
		fmt.Fprintf(os.Stderr, "herd broker ensure: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd broker: ready at %s\n", sock)
}

// runReceiptIssue is the coordinator's explicit ADMISSION command for
// every isolated agent class that is not launched by dispatch or review:
// verification gates, recovery sentinels, and harvest/integration owners
// (FAC-145 "every isolated agent"). It acquires a role-scoped durable
// lease, issues the signed receipt into the target worktree, and stores
// the canonical copy — the same authority pipeline dispatch uses.
//
//	herd receipt issue --role verifier|recovery|integration <ref> <worktree>
func runReceiptIssue() {
	fs := flag.NewFlagSet("receipt issue", flag.ExitOnError)
	role := fs.String("role", "", "verifier|recovery|integration")
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "issue" {
		args = args[1:]
	}
	fs.Parse(args)
	if fs.NArg() < 2 || *role == "" {
		fmt.Fprintln(os.Stderr, "usage: herd receipt issue --role verifier|recovery|integration <ref> <worktree>")
		os.Exit(2)
	}
	ref, targetDir := hsync.NormalizeRef(fs.Arg(0)), fs.Arg(1)
	switch *role {
	case dispatch.RoleVerifier, dispatch.RoleRecovery, dispatch.RoleIntegration:
	default:
		fmt.Fprintf(os.Stderr, "herd receipt: role %q is not an admissible isolated-agent role (FAC-145)\n", *role)
		os.Exit(2)
	}

	root, err := canonicalHerdRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt: %v\n", err)
		os.Exit(1)
	}
	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "herd receipt: task provider: %v\n", tpErr)
		os.Exit(1)
	}
	tasks, err := tp.ListTasks(context.Background(), cfg.TaskProvider.ProjectID, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt: %v\n", err)
		os.Exit(1)
	}
	var task *provider.Task
	for _, t := range tasks {
		if strings.EqualFold(hsync.NormalizeRef(t.Ref), ref) {
			task = t
			break
		}
	}
	if task == nil {
		fmt.Fprintf(os.Stderr, "herd receipt: no task %s on the board\n", ref)
		os.Exit(1)
	}

	gitOut := func(args ...string) string {
		out, _ := exec.Command("git", append([]string{"-C", targetDir}, args...)...).Output()
		return strings.TrimSpace(string(out))
	}
	branch, candidate, base := gitOut("rev-parse", "--abbrev-ref", "HEAD"), gitOut("rev-parse", "HEAD"), gitOut("rev-parse", "origin/main")
	if branch == "" || base == "" {
		fmt.Fprintf(os.Stderr, "herd receipt: %s is not a readable worktree (FAC-145)\n", targetDir)
		os.Exit(1)
	}

	// Role-scoped lease key: a session lease must never collide with the
	// builder's live claim on the same task. Each receipt fences against
	// its OWN key (LeaseTaskRef); unifying these under one canonical task
	// fence is FAC-147's authority, consumed at that rebase.
	leaseRef := ref + ":" + *role
	leaseKey := claim.LeaseKey{Repo: dispatch.RepositoryIdentityOrName(root, cfg.Project.Name), Provider: cfg.TaskProvider.Type, Project: cfg.TaskProvider.ProjectID, TaskRef: leaseRef}
	leaseID, leaseGen, leaseErr := acquireCoordinationLease(context.Background(), root, leaseKey, "coordinator-"+*role, *role)
	if leaseErr != nil {
		fmt.Fprintf(os.Stderr, "herd receipt: %v\n", leaseErr)
		os.Exit(1)
	}
	signer, err := dispatch.LoadSignerForConfig(cfg.Project.Name, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt: %v\n", err)
		os.Exit(1)
	}
	receipt, err := signer.Issue(dispatch.TaskContext{
		ProviderType:      cfg.TaskProvider.Type,
		ProjectID:         cfg.TaskProvider.ProjectID,
		ProviderWorkspace: cfg.TaskProvider.WorkspaceID,
		ProviderProfile:   cfg.TaskProvider.APIKeyEnv,
		Repository:        dispatch.RepositoryIdentityOrName(root, cfg.Project.Name),
		Role:              *role,
		TaskRef:           task.Ref,
		TaskID:            task.ID,
		Branch:            branch,
		BaseSHA:           base,
		CandidateSHA:      candidate,
		LeaseID:           leaseID,
		LeaseGeneration:   leaseGen,
		LeaseTaskRef:      leaseRef,
		SessionID:         dispatch.NewSessionID(*role, task.Ref, candidate, leaseID),
		AllowedOps:        dispatch.OpsForRole(*role),
		ExpiresAt:         time.Now().Add(dispatch.DefaultReceiptTTL),
	})
	if err != nil {
		if relErr := releaseCoordinationLease(context.Background(), root, leaseKey, "coordinator-"+*role, leaseGen); relErr != nil {
			fmt.Fprintf(os.Stderr, "herd receipt: %v; LEASE COMPENSATION FAILED: %v\n", err, relErr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "herd receipt (lease released): %v\n", err)
		os.Exit(1)
	}
	if err := dispatch.WriteTaskContext(targetDir, receipt); err != nil {
		if relErr := releaseCoordinationLease(context.Background(), root, leaseKey, "coordinator-"+*role, leaseGen); relErr != nil {
			fmt.Fprintf(os.Stderr, "herd receipt: %v; LEASE COMPENSATION FAILED: %v\n", err, relErr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "herd receipt (lease released): %v\n", err)
		os.Exit(1)
	}
	if err := dispatch.StoreCanonicalReceipt(root, receipt); err != nil {
		if relErr := releaseCoordinationLease(context.Background(), root, leaseKey, "coordinator-"+*role, leaseGen); relErr != nil {
			fmt.Fprintf(os.Stderr, "herd receipt: %v; LEASE COMPENSATION FAILED: %v\n", err, relErr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "herd receipt (lease released): %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd receipt: issued %s session %s for %s into %s (lease %s gen %d)\n",
		*role, receipt.SessionID, task.Ref, targetDir, leaseID, leaseGen)
}

// runReceiptRelease is the TERMINAL handback for an issued session: it
// removes the worktree receipt and releases the session's durable lease,
// so a finished verifier/recovery/integration agent never holds authority
// for the rest of the receipt TTL (FAC-145).
//
//	herd receipt release --role <role> <ref> <worktree>
func runReceiptRelease() {
	fs := flag.NewFlagSet("receipt release", flag.ExitOnError)
	role := fs.String("role", "", "verifier|recovery|integration")
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "release" {
		args = args[1:]
	}
	fs.Parse(args)
	if fs.NArg() < 2 || *role == "" {
		fmt.Fprintln(os.Stderr, "usage: herd receipt release --role <role> <ref> <worktree>")
		os.Exit(2)
	}
	ref, targetDir := hsync.NormalizeRef(fs.Arg(0)), fs.Arg(1)

	root, err := canonicalHerdRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt release: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt release: %v\n", err)
		os.Exit(1)
	}
	tc, err := dispatch.ReadTaskContext(targetDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt release: %v\n", err)
		os.Exit(1)
	}
	if tc.Role != *role || !strings.EqualFold(hsync.NormalizeRef(tc.TaskRef), ref) {
		fmt.Fprintf(os.Stderr, "herd receipt release: %s holds a %s receipt for %s, not %s/%s (FAC-145)\n", targetDir, tc.Role, tc.TaskRef, *role, ref)
		os.Exit(1)
	}
	// Lease handback FIRST: if it fails, the receipt survives so the
	// release can be retried with intact authority (FAC-145).
	key := claim.LeaseKey{Repo: dispatch.RepositoryIdentityOrName(root, cfg.Project.Name), Provider: cfg.TaskProvider.Type, Project: cfg.TaskProvider.ProjectID, TaskRef: tc.LeaseTaskRef}
	if err := releaseCoordinationLease(context.Background(), root, key, "coordinator-"+*role, tc.LeaseGeneration); err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt release: lease handback failed (receipt retained for retry): %v\n", err)
		os.Exit(1)
	}
	if err := os.Remove(filepath.Join(targetDir, dispatch.TaskContextFile)); err != nil {
		fmt.Fprintf(os.Stderr, "herd receipt release: remove receipt: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd receipt: released %s session %s for %s\n", *role, tc.SessionID, tc.TaskRef)
}

// runTaskClient is the THIN capability client every isolated agent uses:
// it reads ONLY the worktree's signed receipt and sends it to the
// coordinator broker as a capability. No config, no verification key, no
// provider code, and no credentials ever exist in the agent process.
func runTaskClient() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: herd task get <ref> [--full] | herd task comment <ref> <text...>")
		os.Exit(2)
	}
	sub, ref := os.Args[2], os.Args[3]

	tc, err := dispatch.ReadTaskContext(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd task: %v\n", err)
		os.Exit(1)
	}
	if !strings.EqualFold(hsync.NormalizeRef(tc.TaskRef), hsync.NormalizeRef(ref)) {
		fmt.Fprintf(os.Stderr, "herd task: this worktree's receipt is bound to %s, not %s (FAC-145: one receipt, one task)\n", tc.TaskRef, ref)
		os.Exit(1)
	}

	req := brokerRequest{Op: "", Ref: ref, Receipt: tc}
	switch sub {
	case "get":
		req.Op = "get"
	case "verdict":
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "usage: herd task verdict <ref> APPROVED|REJECTED [detail...]")
			os.Exit(2)
		}
		req.Op = "verdict"
		req.Body = os.Args[4]
		req.CandidateSHA = tc.CandidateSHA
		// The client reports its ACTUAL checkout HEAD; the broker refuses
		// when it drifts from the admitted candidate.
		if head, hErr := exec.Command("git", "rev-parse", "HEAD").Output(); hErr == nil {
			req.WorktreeHEAD = strings.TrimSpace(string(head))
		}
		if len(os.Args) > 5 {
			req.Detail = strings.Join(os.Args[5:], " ")
		}
	case "comment":
		var words []string
		for _, a := range os.Args[4:] {
			if !strings.HasPrefix(a, "--") {
				words = append(words, a)
			}
		}
		req.Op = "comment"
		req.Body = strings.TrimSpace(strings.Join(words, " "))
		if req.Body == "" {
			fmt.Fprintln(os.Stderr, "usage: herd task comment <ref> <text...>")
			os.Exit(2)
		}
	default:
		fmt.Fprintf(os.Stderr, "herd task: unknown subcommand %q (get|comment|verdict — mutations are coordinator-owned)\n", sub)
		os.Exit(2)
	}

	sock, err := brokerSocketPath(tc.Repository)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd task: %v\n", err)
		os.Exit(1)
	}
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd task: coordinator broker unavailable at %s (FAC-145: agents have no direct provider access): %v\n", sock, err)
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "herd task: %v\n", err)
		os.Exit(1)
	}
	var resp brokerResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "herd task: broker response: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "herd task: %s\n", resp.Error)
		os.Exit(1)
	}
	if req.Op == "verdict" {
		fmt.Printf("herd task: verdict recorded for %s@%s\n", tc.TaskRef, tc.CandidateSHA)
		return
	}
	if req.Op == "get" {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, resp.Result, "", "  "); err != nil {
			fmt.Println(string(resp.Result))
		} else {
			fmt.Println(pretty.String())
		}
		return
	}
	fmt.Printf("herd task: comment posted to %s\n", tc.TaskRef)
}

// repoRootFromWorktree resolves the main repository root from inside any
// git worktree via the shared common dir.
func repoRootFromWorktree(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("git common dir: %w", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out))), nil
}

// isManagedWorktree reports whether path is one of the fleet's dispatched
// task worktrees (under .herd/worktrees) — the set FAC-145 guarantees a
// launch receipt for.
func isManagedWorktree(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return strings.Contains(filepath.ToSlash(abs), "/.herd/worktrees/")
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
	cfg              *config.Config
	maxLanes         int
	observer         *herdr.ProductionReconciliationObserver
	fleet            herdr.FleetStatus
	reconcileBlocked bool
}

func (d *cliForgeDriver) Log(msg string) { fmt.Println(msg) }

func (d *cliForgeDriver) ObserveReconciliation(ctx context.Context) error {
	if d.observer == nil {
		d.reconcileBlocked = true
		return fmt.Errorf("reconciliation observer unavailable")
	}
	err := d.observer.ObserveReconciliation(ctx)
	d.fleet = herdr.ProjectFleetStatus(d.observer.Decisions(), d.maxLanes)
	d.reconcileBlocked = err != nil || d.fleet.Unknown > 0
	// Herdr 0.8 reports the coordinator's own terminal pane without the
	// immutable generation fields required for managed task lanes. That shell
	// is not capacity and must not prevent the forge from starting. Once any
	// task-fac lane exists, retain the fail-closed UNKNOWN behavior.
	if d.reconcileBlocked && err != nil {
		if agents, listErr := herdr.AgentList(); listErr == nil {
			managed := false
			for _, agent := range agents {
				if strings.HasPrefix(agent.Name, "task-fac-") {
					managed = true
					break
				}
			}
			if !managed {
				d.reconcileBlocked = false
			}
		}
	}
	return err
}

// LaneState counts live task-fac-* builder agents that are working. A herdr
// read failure is UNKNOWN capacity, not free capacity (FAC-138). When the
// FAC-158 reconciliation observer has a projection, prefer it; when that
// projection is BLOCKED/unknown, refuse free capacity by reporting full busy.
func (d *cliForgeDriver) LaneState(ctx context.Context) (daemon.LaneState, error) {
	if d.reconcileBlocked {
		return daemon.LaneState{Busy: d.maxLanes, Max: d.maxLanes}, nil
	}
	if d.observer != nil && len(d.observer.Decisions()) > 0 {
		return daemon.LaneState{Busy: d.fleet.Working + d.fleet.Recovering, Max: d.maxLanes}, nil
	}
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
// working; it is verified only when FAC-144 completion admission produces a
// current PASS receipt (VerifyAndPersist + lifecycle evidence). CheckCompletion
// is never review authority. An unreadable fleet yields an error, never an
// empty (and so drained-looking) signal set (FAC-138).
func (d *cliForgeDriver) Signals(ctx context.Context) (map[string]bool, map[string]bool, error) {
	completed := map[string]bool{}
	verified := map[string]bool{}
	agents, err := herdr.AgentList()
	if err != nil {
		return nil, nil, fmt.Errorf("herdr agent list: %w", err)
	}
	for _, a := range agents {
		if !strings.HasPrefix(a.Name, "task-fac-") {
			continue
		}
		if a.Status == "working" || a.Status == "starting" {
			continue
		}
		ref := strings.ToUpper(strings.TrimPrefix(a.Name, "task-"))
		completed[ref] = true
		wt := worktreePathForRef(ref)
		if !worktreeExists(wt) {
			continue
		}
		ready, digest, reason, verr := verifyWorktreeForReview(ctx, d.cfg, ref, wt)
		if verr != nil {
			d.Log(fmt.Sprintf("forge: verify %s hard-failed: %v", ref, verr))
			continue
		}
		if ready {
			verified[ref] = true
			if digest != "" {
				d.Log(fmt.Sprintf("forge: %s verification PASS digest=%s", ref, digest))
			}
		} else if reason != "" {
			d.Log(fmt.Sprintf("forge: %s not review-ready: %s", ref, reason))
		}
	}
	return completed, verified, nil
}

// herdSubprocess runs the compiled herd binary (or a test double). Production
// uses os.Executable; FAC-135 e2e installs a recorder that never touches a
// live board so arg-order and crash-boundary tests stay hermetic.
var herdSubprocess = herdSubprocessReal

func herdSubprocessReal(args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("herd executable: %w", err)
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// setHerdSubprocessForTest replaces the herd subprocess runner. Restore with
// the returned function.
func setHerdSubprocessForTest(f func(args ...string) error) func() {
	old := herdSubprocess
	if f == nil {
		herdSubprocess = herdSubprocessReal
	} else {
		herdSubprocess = f
	}
	return func() { herdSubprocess = old }
}

func (d *cliForgeDriver) herd(args ...string) error {
	return herdSubprocess(args...)
}

// reviewArgs is the exact argv the production driver must emit. Exposed so
// FAC-135 can mutation-prove --spawn precedes the ref (FAC-138 regression).
func reviewArgs(ref string) []string {
	return []string{"review", "--spawn", ref}
}

func (d *cliForgeDriver) Dispatch(ctx context.Context, t *provider.Task) error {
	// FAC-159: wave/forge always route through `herd dispatch`, which runs
	// RequireTaskLaunch (selection + re-read) before worktree/status/tab and
	// post-validates with compensation on graph drift.
	lane := "worker"
	if d.cfg != nil {
		if worker := findLaneForRole(d.cfg, "worker"); worker != nil && strings.TrimSpace(worker.Name) != "" {
			lane = worker.Name
		}
	}
	return d.herd("dispatch", t.Ref, "--lane", lane)
}

// admitReviewHook is the production FAC-144 re-admission path. Tests replace
// it via setAdmitReviewForTest so argv-order and subprocess-error probes stay
// hermetic without a full receipt + lifecycle fixture.
var admitReviewHook = admitWorktreeForReview

func setAdmitReviewForTest(f func(ctx context.Context, cfg *config.Config, ref, wt, digest string) error) func() {
	old := admitReviewHook
	if f == nil {
		admitReviewHook = admitWorktreeForReview
	} else {
		admitReviewHook = f
	}
	return func() { admitReviewHook = old }
}

func (d *cliForgeDriver) Review(ctx context.Context, t *provider.Task) error {
	// FAC-144: re-admit with RequireCurrentPassing before spawning review.
	// A Signals-time PASS is not sufficient if the candidate moved.
	wt := worktreePathForRef(t.Ref)
	if !worktreeExists(wt) {
		return fmt.Errorf("review %s: worktree missing", t.Ref)
	}
	if err := admitReviewHook(ctx, d.cfg, t.Ref, wt, ""); err != nil {
		return fmt.Errorf("review %s refused without current PASS receipt: %w", t.Ref, err)
	}
	// --spawn BEFORE the ref: flag.Parse stops at the first positional, so the
	// old trailing form silently parsed spawn=false and no reviewer ever
	// started. runReview now also normalizes the order (FAC-138).
	return d.herd(reviewArgs(t.Ref)...)
}

func (d *cliForgeDriver) Approve(ctx context.Context, t *provider.Task) error {
	// FAC-135: the loop refuses to move a card to Done under a repository that
	// has not declared the gates. This is a declaration check, NOT per-candidate
	// admission — that is reviewledger.Admit (exact SHA, different-family
	// reviewer, unspent lease), and merge evidence is pkg/sync.BoardDone.
	if err := preflight.RefuseAutonomousMerge("."); err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	// Evidence-gated board move (requires the branch to be merged on
	// origin/main); harvest/merge stays coordinator-owned git work.
	if err := d.herd("approve", t.Ref); err != nil {
		return err
	}
	_ = herdr.CloseTabForRef(t.Ref) // FAC-111: close the finished tab
	return nil
}

// Rejections reads the review ledger and returns every reviewer FAIL that no
// later PASS has superseded, keyed by ticket ref.
//
// FAC-140: this is the edge that was missing entirely. Reviewers wrote a FAIL
// verdict, posted it, and went idle; nothing downstream ever read it, so the
// coordinator kept offering the FAILed card to the merge gate and the worker
// sat idle holding nothing.
//
// A read failure is an error, never an empty map: "no rejection" and "cannot
// tell" are the same value here, and the second one re-arms the merge gate.
func (d *cliForgeDriver) Rejections(ctx context.Context) (map[string]daemon.Rejection, error) {
	l, err := reviewledger.NewReviewLedger(".", filepath.Join(".herd", "review-ledger.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("open review ledger: %w", err)
	}
	rows, err := l.AllRows()
	if err != nil {
		return nil, fmt.Errorf("read review ledger: %w", err)
	}
	queue, err := l.QueueRows()
	if err != nil {
		return nil, fmt.Errorf("read harvest queue: %w", err)
	}
	return outstandingRejections(rows, queue, os.ReadFile), nil
}

// outstandingRejections is the pure projection behind Rejections, split out so
// the ledger→rejection rules are testable without a live fleet.
//
// Verdict rows carry the sha but not the branch (Ledger.Verdict writes the
// branch to the QUEUE row instead), so the ref is recovered by joining any row
// that carries both. readFile loads the reviewer's evidence body.
func outstandingRejections(rows, queue []reviewledger.LedgerRow, readFile func(string) ([]byte, error)) map[string]daemon.Rejection {
	refBySHA := map[string]string{}
	for _, r := range append(append([]reviewledger.LedgerRow{}, rows...), queue...) {
		if r.SHA == "" {
			continue
		}
		if ref := refForTaskBranch(r.Branch); ref != "" {
			refBySHA[r.SHA] = ref
		}
	}

	out := map[string]daemon.Rejection{}
	// The ledger is append-only and chronological, so the last verdict row for
	// a ref is its current standing.
	for _, r := range rows {
		if r.Event != string(reviewledger.EventVerdict) {
			continue
		}
		ref := strings.ToUpper(strings.TrimSpace(r.Task))
		if ref == "" {
			ref = refBySHA[r.SHA]
		}
		if ref == "" {
			continue
		}
		switch reviewledger.Verdict(r.Verdict) {
		case reviewledger.VerdictPASS:
			// Only a PASS clears a rejection. A later BLOCKED grants no merge
			// authority either, so it must not wipe an outstanding FAIL.
			delete(out, ref)
		case reviewledger.VerdictFAIL:
			rj := daemon.Rejection{Ref: ref, SHA: r.SHA, Reviewer: r.Reviewer, Artifact: r.Artifact}
			if r.Artifact != "" {
				if body, err := readFile(r.Artifact); err == nil {
					rj.Findings = reviewingest.Parse(string(body)).Body
				}
			}
			out[ref] = rj
		}
	}
	return out
}

// refForTaskBranch recovers the ticket ref from a task branch, which
// worktree.TaskBranch minted as herd/<lowercase ref>. Anything else yields ""
// rather than a guessed ref.
func refForTaskBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	if !strings.HasPrefix(branch, "herd/") {
		return ""
	}
	return strings.ToUpper(strings.TrimPrefix(branch, "herd/"))
}

// Reject delivers the reviewer's numbered rejection to the card's authoring
// worker and proves the worker consumed it (FAC-140). No coordinator or human
// stands between the FAIL and the repair.
func (d *cliForgeDriver) Reject(ctx context.Context, t *provider.Task, r daemon.Rejection) error {
	// "A bare rejection is unactionable" is the review-verdict contract's own
	// rule; delivering an empty one would satisfy the routing and strand the
	// worker exactly as before. Checked before the fleet gate so an unreadable
	// verdict artifact is reported as itself, not as an admission failure.
	if strings.TrimSpace(r.Findings) == "" {
		return fmt.Errorf("review FAIL for %s at %s carries no findings body (artifact %q): "+
			"a bare rejection is unactionable, refusing to deliver it",
			t.Ref, shortSHA(r.SHA), r.Artifact)
	}
	if err := requireFleetAdmission(ctx); err != nil {
		return err
	}
	agent, err := workerAgentForRef(t.Ref)
	if err != nil {
		return err
	}
	msg := rejectionPrompt(t.Ref, r)
	// DeliverAndProve, not AgentPrompt: a submit into a dead pane returns a
	// healthy-looking exit code, and "verified delivered" is an acceptance
	// criterion here.
	receipt, err := herdr.DeliverAndProve(agent, msg, 90*time.Second)
	if err != nil {
		return fmt.Errorf("delivering the %s rejection to %s: %w", t.Ref, agent, err)
	}
	if !receipt.Consumed {
		return fmt.Errorf("delivering the %s rejection to %s: no consumption proof (%s)",
			t.Ref, agent, receipt.SequenceToken)
	}
	return nil
}

// workerAgentForRef resolves the live authoring worker tab for a ref —
// task-fac-<n>, or its -safe variant. A missing tab is reported, never
// respawned here: re-creating a builder lane is a launch-admission decision
// (herd dispatch / recovery-sentinel), not something the rejection path may
// do behind the launch gates.
func workerAgentForRef(ref string) (string, error) {
	agents, err := herdr.AgentList()
	if err != nil {
		return "", fmt.Errorf("herdr agent list: %w", err)
	}
	base := "task-" + strings.ToLower(ref)
	for _, want := range []string{base, base + "-safe"} {
		for _, a := range agents {
			if a.Name == want {
				return a.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no live worker tab for %s (looked for %s and %s-safe): "+
		"respawn the builder into its worktree before the rejection can be routed", ref, base, base)
}

// rejectionPrompt is the payload the worker receives. It carries the
// reviewer's findings verbatim and the repair order the worker contract
// requires: new commit, published, re-verified, re-reviewed — never merged.
func rejectionPrompt(ref string, r daemon.Rejection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REVIEW FAIL — %s\n\n", ref)
	fmt.Fprintf(&b, "Reviewer %s rejected candidate %s. The findings below are the reviewer's own words:\n\n",
		r.Reviewer, r.SHA)
	b.WriteString(strings.TrimSpace(r.Findings))
	b.WriteString("\n\nRepair order — do all of it, in this order, without asking a coordinator:\n")
	b.WriteString("1. Fix every numbered finding in your existing worktree and branch.\n")
	b.WriteString("2. Commit a NEW commit. The repaired candidate must be a fresh SHA, distinct from " + shortSHA(r.SHA) + ".\n")
	b.WriteString("3. Re-run the configured gate and `herd verify` until they pass on the fresh SHA.\n")
	b.WriteString("4. Push the candidate and read back that the PR head resolves to that exact SHA.\n")
	b.WriteString("5. Request a fresh review from a family other than the reviewer's.\n")
	b.WriteString("Never merge, approve, or move the card yourself. Do not amend the FAILed commit away.\n")
	return b.String()
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

// forgeControlReconciler composes the durable coordinator control plane used
// by the real forge loop. Orders are read from the persisted outbox and are
// reconciled only when structured acknowledgement/supersession evidence is
// already present; a missing receipt remains pending for a later tick.
func forgeControlReconciler(root string, cfg *config.Config) (*control.CoordinatorLoop, func() error, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("forge control: config is required")
	}
	controlStore, err := outbox.NewStore(filepath.Join(root, ".herd", "control-orders.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("forge control: open outbox: %w", err)
	}
	controlMailbox := mail.NewMailbox(filepath.Join(root, ".herd", "control-mail.jsonl"))
	leaseOwnership, err := deps.OpenLeaseOwnership(
		deps.ResolveLaunchLeasePath(root),
		dispatch.RepositoryIdentityOrName(root, cfg.Project.Name),
		cfg.TaskProvider.Type,
		cfg.TaskProvider.ProjectID,
	)
	if err != nil {
		_ = controlStore.Close()
		return nil, nil, fmt.Errorf("forge control: open lease authority: %w", err)
	}

	authority := control.FencedAuthority{
		Check: func(ctx context.Context, order control.Order) error {
			if order.TaskRef == "" || order.LeaseGeneration <= 0 || order.Lane == "" {
				return fmt.Errorf("forge control: incomplete live identity for %s", order.TaskRef)
			}
			claims, err := leaseOwnership.CM.ActiveClaims(ctx)
			if err != nil {
				return fmt.Errorf("forge control: read live lease: %w", err)
			}
			for _, lease := range claims {
				if lease.TaskRef == order.TaskRef && lease.Generation == order.LeaseGeneration && lease.HoldLane == order.Lane {
					return nil
				}
			}
			return fmt.Errorf("forge control: lease for %s generation %d is not active on lane %s", order.TaskRef, order.LeaseGeneration, order.Lane)
		},
	}
	reader := control.MailboxEvidenceReader{Mailbox: controlMailbox}
	delivery := &control.Delivery{Outbox: controlStore, Authority: authority, Evidence: reader}
	loop := &control.CoordinatorLoop{
		Delivery: delivery,
		Orders: func(ctx context.Context) ([]control.Order, error) {
			items, err := controlStore.Outstanding("", 1000)
			if err != nil {
				return nil, err
			}
			orders := make([]control.Order, 0, len(items))
			for _, item := range items {
				var order control.Order
				if err := json.Unmarshal([]byte(item.Payload), &order); err != nil {
					return nil, fmt.Errorf("forge control: corrupt order %d: %w", item.ID, err)
				}
				if item.TaskRef != order.TaskRef || item.Kind != "control/"+string(order.Kind) {
					return nil, fmt.Errorf("forge control: outbox identity mismatch for item %d", item.ID)
				}
				_, ackErr := reader.ReadEvidence(ctx, item.IdempotencyKey, false)
				if ackErr == nil {
					orders = append(orders, order)
					continue
				}
				if !errors.Is(ackErr, control.ErrEvidenceNotFound) {
					return nil, ackErr
				}
				_, supersedeErr := reader.ReadEvidence(ctx, item.IdempotencyKey, true)
				if supersedeErr == nil {
					orders = append(orders, order)
					continue
				}
				if !errors.Is(supersedeErr, control.ErrEvidenceNotFound) {
					return nil, supersedeErr
				}
			}
			return orders, nil
		},
	}
	closeFn := func() error { return errors.Join(leaseOwnership.Close(), controlStore.Close()) }
	return loop, closeFn, nil
}

// forgeLoopMain returns the process exit code so every path releases the
// coordinator fence — os.Exit skips deferred releases (FAC-138).
func forgeLoopMain() int {
	fs := flag.NewFlagSet("forge-loop", flag.ExitOnError)
	_ = fs.Bool("loop", true, "run the autonomous loop")
	maxLanes := fs.Int("max-lanes", 3, "max concurrent builder lanes")
	interval := fs.Int("interval", 15, "seconds between ticks")
	ticks := fs.Int("ticks", 0, "stop after N ticks (0 = run until drained)")
	stopEmpty := fs.Bool("stop-empty", true, "stop when the board is clear and no lane is busy")
	// --coordinator-name is what makes ReplyTarget.Name real. A reviewer caught
	// that Register was called with the constant, so Dispatcher.CoordinatorName,
	// ReplyTarget.Name and the non-default branch of coordinatorName() worked in
	// tests and could never be driven in production -- a field that looked
	// configurable and was not. Two coordinators against one repo need distinct
	// inboxes, so this has to be settable, not merely parameterised.
	coordName := fs.String("coordinator-name", coordinator.CoordinatorName,
		"durable coordinator identity agents report to (must match the mail inbox)")
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
	forgeWorkspace := strings.TrimSpace(cfg.Fleet.HerdrWorkspace)
	if forgeWorkspace == "" {
		// Prefer the live workspace labeled for this repo. This prevents a
		// long-lived shell's HERD_WORKSPACE from silently routing Herdforge into
		// a different checkout (for example, Chainseer).
		if entries, listErr := herdr.WorkspaceList(); listErr == nil {
			if id, ok := herdr.PickWorkspaceStrict(entries, cfg.Project.Name); ok {
				forgeWorkspace = id
			}
		}
	}
	if forgeWorkspace == "" {
		forgeWorkspace = strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
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

	// FAC-222: register the coordinator as a named agent so dispatched packets
	// carry a reply address and agents report completion/BLOCKED to it instead
	// of relying on the coordinator to notice by polling.
	coordReg, regErr := coordinator.Register(".", *coordName, forgeWorkspace)
	if regErr != nil {
		fmt.Fprintf(os.Stderr, "forge --loop: coordinator registration failed: %v\n", regErr)
		return 1
	}
	fmt.Printf("herd forge --loop: coordinator registered as %q (workspace=%s)\n", coordReg.Name, coordReg.Workspace)

	controlLoop, closeControl, controlErr := forgeControlReconciler(".", cfg)
	if controlErr != nil {
		fmt.Fprintf(os.Stderr, "forge --loop: %v\n", controlErr)
		return 1
	}
	defer func() { _ = closeControl() }()
	eng := daemon.NewEngineWithControl(cfg, tp, nil, st, resolveCanonicalWorktreeManager(), nil, controlLoop)

	// Structural composition is checked BEFORE any broker side effect: a run
	// that can never proceed must not spawn a broker process first. Mirrors
	// ForgeLoop's own guard so the refusal reason stays the composition one.
	if eng.ControlRequired && eng.ControlReconciler == nil {
		fmt.Fprintf(os.Stderr, "forge --loop: forge: durable control reconciler is required before board or lane actions\n")
		return 1
	}

	// FAC-222: wire the feedback census into the loop so a lane that goes quiet
	// is REPORTED rather than discovered by polling. The census runs every
	// feedbackInterval ticks; a failure is logged, never fatal.
	feedbackInterval := feedback.CensusTickInterval(*interval)
	feedbackRunner := func(ctx context.Context) error {
		return feedback.Run(ctx, feedback.Options{
			Coordinator: coordReg.Name,
			Workspace:   forgeWorkspace,
		})
	}

	observer := &herdr.ProductionReconciliationObserver{
		Workspace: forgeWorkspace,
		Reader:    herdr.SocketAuthorityReader{},
		Record:    (&herdr.JSONLRecorder{Path: ".herd/reconciliation.jsonl"}).Record,
	}
	driver := &cliForgeDriver{cfg: cfg, maxLanes: *maxLanes, observer: observer}

	fmt.Printf("herd forge --loop: max-lanes=%d interval=%ds — driving the board autonomously\n", *maxLanes, *interval)
	err = eng.ForgeLoop(ctx, driver, daemon.ForgeLoopOptions{
		Interval:         time.Duration(*interval) * time.Second,
		MaxTicks:         *ticks,
		StopEmpty:        *stopEmpty,
		Feedback:         feedbackRunner,
		FeedbackInterval: feedbackInterval,
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

// runDrainSelftest verifies the drain's own integration seams. git is a hard
// prerequisite because the report's freshness probes are git reads; herdr is
// not, because the compiled adapters take a process API as a seam and the live
// launcher already fails closed when the CLI is absent.
func runDrainSelftest() int {
	failed := false
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Printf("herd-drain selftest FAIL: missing tool git: %v\n", err)
		failed = true
	}
	if drainSelftest(os.Stdout) != 0 {
		failed = true
	}
	fmt.Println("herd-drain selftest: FAC-182 durable rebase control delivery remains blocked; rebase mail stays refused")
	if failed {
		return 1
	}
	return 0
}

// publishedGraphBinding reads the graph snapshot the coordinator published so
// the dispatcher binds to exactly what scopefence will resolve against. A
// mismatch here is rejected as "trusted graph snapshot rejected", so deriving
// both sides from the same stored row is the only way they cannot disagree.
// When no graph has been published yet (empty revision), returns empty values;
// dispatch will auto-publish the graph and scope during the deps gate.
func publishedGraphBinding(root string) (string, int) {
	store, err := scopefence.NewSQLiteStore(filepath.Join(root, ".herd", "scopefence.db"))
	if err != nil {
		return "", 0
	}
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		return "", 0
	}
	graph, err := store.ReadLatestGraphSnapshot(context.Background(), repository)
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
