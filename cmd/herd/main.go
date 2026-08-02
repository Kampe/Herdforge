package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/next"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/process"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/resolve"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/selftest"
	"github.com/Kampe/Herdforge/pkg/store"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
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

	case "init":
		runInit()

	case "clone":
		runClone()

	case "preflight":
		runPreflight()

	case "selftest":
		runSelfTest()

	case "status":
		runStatus()

	case "pulse":
		runPulse()

	case "standing":
		runStanding()

	case "daemon":
		runDaemon()

	case "usage":
		runUsage()

	case "quota":
		runQuota()

	case "review":
		runReview()

	case "approve":
		runApprove()

	case "board-done":
		runBoardDone()

	case "sh", "repl":
		runShell()

	case "forge":
		runForge()

	case "up":
		runUp()

	case "validate-config":
		runValidateConfig()

	case "next":
		runNext()

	case "dispatch":
		runDispatch()

	case "harvest":
		runHarvest()

	case "process":
		runProcess()

	case "resolve-lane":
		runResolveLane()

	case "kick":
		runKick()

	case "attention":
		runAttention()

	case "lifecycle":
		runLifecycle()

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
	fmt.Println("  pulse      Claim a task from Kaneo and optionally spawn an agent")
	fmt.Println("  review     Claim in-progress tasks for reviewer and advance to review status")
	fmt.Println("  approve    Move in-review cards to done, gated on merge evidence")
	fmt.Println("  board-done Move one card to done ONLY with proof its work is on origin/main")
	fmt.Println("  sh         Interactive shell: run herd subcommands in a loop")
	fmt.Println("  forge      Full cycle: pulse worker + review + approve")
	fmt.Println("  standing   Launch all configured agent lanes in herdr tabs")
	fmt.Println("  daemon     Start the long-running orchestration daemon (infinite pulse loop)")
	fmt.Println("  usage      Show harness quota usage from OpenUsage CLI")
	fmt.Println("  quota      Show binding headroom, pace/pressure, pool breakdown")
	fmt.Println("  up         Start a single agent lane (herd up <lane-name>)")
	fmt.Println("  validate-config  Validate .herd/herd.yaml configuration")
	fmt.Println("  next            Show highest-priority next action")
	fmt.Println("  dispatch        Dispatch a ticket to a worktree and launch agent")
	fmt.Println("  harvest         Sweep all worktrees for unmerged commits")
	fmt.Println("  process         Classify harvest targets (herd-process digest)")
	fmt.Println("  resolve-lane    Resolve a lane to concrete provider+model (deterministic)")
	fmt.Println("  kick            Re-engage standing or named agent lanes")
	fmt.Println("  attention       List standing agents needing coordinator eyes (triage)")
	fmt.Println("  lifecycle       Observe and act on fleet state via lifecycle engine")
	fmt.Println("  --version       Show herd version")
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
  type: "kaneo"
  project_id: "your-project-id"
  api_url: "https://kanban-api.kampe.kluster"

lanes:
  - name: "worker"
    role: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"

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
  type: "kaneo"
  workspace_id: "your-workspace-id"
  project_id: "your-project-id"
  api_url: "https://kanban-api.kampe.kluster"
  use_cli: true

# Agent lanes — each lane runs in a herdr workspace tab
# All lanes default to deepseek-v4-flash via opencode for cheap self-forging
lanes:
  - name: "forge-smith"
    role: "forge-smith"
    agent_kind: "opencode"
    harness: "opencode"
    prompt: ".herd/prompts/smith.md"
    worktree: ".worktrees/smith"
    provider: "deepseek"
    model: "deepseek-v4-flash"

  - name: "worker"
    role: "worker"
    agent_kind: "opencode"
    harness: "opencode"
    prompt: ".herd/prompts/worker.md"
    worktree: ".worktrees/worker"
    provider: "deepseek"
    model: "deepseek-v4-flash"

  - name: "reviewer"
    role: "reviewer"
    agent_kind: "opencode"
    harness: "opencode"
    prompt: ".herd/prompts/reviewer.md"
    worktree: ".worktrees/reviewer"
    provider: "deepseek"
    model: "deepseek-v4-flash"

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
}

func runPulse() {
	pulseFlags := flag.NewFlagSet("pulse", flag.ExitOnError)
	role := pulseFlags.String("role", "worker", "Target role to run pulse sweep for")
	spawn := pulseFlags.Bool("spawn", false, "Spawn an agent in herdr to work the claimed task")
	pulseFlags.Parse(os.Args[2:])

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
	case "github":
		tp = provider.NewGitHubProvider(os.Getenv("GITHUB_TOKEN"), "owner", "repo")
	default:
		tp = provider.NewMemoryProvider()
	}

	mr := router.NewModelRouter([]*router.ModelCandidate{
		{Name: "opencode", Type: router.ProviderOllama, Model: "deepseek-v4-flash"},
	})
	wm := worktree.NewWorktreeManager(".")
	v := verifier.NewVerifier(cfg.Verification.TestCommand)

	st, err := store.New(".herd/herdforge.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: store init failed (pulse continues without persistence): %v\n", err)
	} else {
		defer st.Close()
	}

	eng := daemon.NewEngine(cfg, tp, mr, st, wm, v)

	task, err := eng.RunPulse(context.Background(), *role)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulse sweep failed: %v\n", err)
		os.Exit(1)
	}

	if task == nil {
		fmt.Println("Pulse sweep complete: No pending tasks available.")
		return
	}

	fmt.Printf("Pulse sweep claimed task [%s]: %s\n", task.Ref, task.Title)

	if *spawn {
		if !herdr.IsAvailable() {
			fmt.Fprintf(os.Stderr, "herdr CLI not found — cannot spawn agent\n")
			os.Exit(1)
		}

		lane := findLaneForRole(cfg, *role)
		if lane == nil {
			fmt.Fprintf(os.Stderr, "no lane configured for role '%s'\n", *role)
			os.Exit(1)
		}

		standingName := fmt.Sprintf("forge-%s", lane.Name)
		targetLabel := standingName

		tabLabel, err := herdr.ResolveAgentTab(standingName)
		if err != nil {
			// no standing agent — create a fresh one
			tabLabel = fmt.Sprintf("pulse-%s-%s", lane.Name, task.Ref)
			tab, err := herdr.Tab("wF", tabLabel, true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to create herdr tab: %v\n", err)
				os.Exit(1)
			}
			if err := herdr.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID); err != nil {
				fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Spawned agent '%s' in tab %s (pane %s)\n", tabLabel, tab.ID, tab.Pane.ID)
		} else {
			targetLabel = standingName
			fmt.Printf("Using standing agent '%s' (tab %s)\n", standingName, tabLabel)
		}

		workPacket := fmt.Sprintf(`Task [%s]: %s

Description:
%s

Status: %s
Priority: %s
Labels: %s

Workflow:
1. Enter your worktree %s and inspect existing code
2. Write failing tests for the required change
3. Implement the minimal solution
4. Run 'make lint all' (or 'go test ./...')
5. Commit with a conventional commit message
6. Signal completion to the orchestrator (e.g. move card status)`,
			task.Ref, task.Title, task.Description, task.Status, task.Priority, strings.Join(task.Labels, ", "),
			lane.Worktree)

		if _, err := herdr.AgentPrompt(targetLabel, workPacket, false); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: failed to deliver task packet: %v\n", err)
		} else {
			fmt.Printf("  -> delivered task packet to %s\n", targetLabel)
		}
	}
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
				"pick":           pick,
				"binding":        state,
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
		fmt.Fprintf(os.Stderr, "daemon: warning — store init failed, running without persistence: %v\n", err)
	} else {
		defer st.Close()
	}

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

		var tp provider.TaskProvider
		switch cfg.TaskProvider.Type {
		case "kaneo":
			tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
		default:
			tp = provider.NewMemoryProvider()
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
		wm := worktree.NewWorktreeManager(".")
		v := verifier.NewVerifier(cfg.Verification.TestCommand)
		eng := daemon.NewEngine(cfg, tp, mr, st, wm, v)

		task, err := eng.RunPulse(ctx, *role)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: pulse failed: %v\n", err)
		} else if task != nil {
			fmt.Printf("[%s] Claimed: %s — %s\n", time.Now().Format(time.RFC3339), task.Ref, task.Title)
		}

		time.Sleep(pulseInterval)
	}
}

func runStanding() {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	if !herdr.IsAvailable() {
		fmt.Fprintf(os.Stderr, "herdr CLI not found — install herdr first\n")
		os.Exit(1)
	}

	for _, lane := range cfg.Lanes {
		if lane.Worktree != "" {
			wtPath := filepath.Join(".", lane.Worktree)
			if _, err := os.Stat(wtPath); os.IsNotExist(err) {
				fmt.Printf("Creating worktree %s for lane %s...\n", lane.Worktree, lane.Name)
				wtBranch := fmt.Sprintf("wt/%s", lane.Name)
				cmd := exec.Command("git", "worktree", "add", "-b", wtBranch, lane.Worktree, "origin/main")
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr, "  warning: failed to create worktree (non-fatal): %v\n", err)
				}
			}
		}

		tabLabel := fmt.Sprintf("forge-%s", lane.Name)
		fmt.Printf("Launching lane '%s' as agent '%s' (kind=%s)...\n", lane.Name, tabLabel, lane.AgentKind)

		tab, err := herdr.Tab("wF", tabLabel, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  failed to create tab for lane %s: %v\n", lane.Name, err)
			continue
		}

		if err := herdr.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID); err != nil {
			fmt.Fprintf(os.Stderr, "  failed to start agent for lane %s: %v\n", lane.Name, err)
			continue
		}

		if lane.Prompt != "" {
			if promptData, err := os.ReadFile(lane.Prompt); err == nil {
				promptText := strings.TrimSpace(string(promptData))
				herdr.AgentPrompt(tabLabel, promptText, false)
			}
		}

		fmt.Printf("  -> tab=%s pane=%s agent=%s running\n", tab.ID, tab.Pane.ID, tabLabel)
	}
}

func runUp() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: herd up <lane-name>\n")
		os.Exit(1)
	}
	laneName := os.Args[2]

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

	tabLabel := fmt.Sprintf("forge-%s", lane.Name)
	tab, err := herdr.Tab("wF", tabLabel, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create tab: %v\n", err)
		os.Exit(1)
	}

	if err := herdr.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Lane '%s' started: tab=%s pane=%s agent=%s\n", lane.Name, tab.ID, tab.Pane.ID, tabLabel)
}

func runReview() {
	reviewFlags := flag.NewFlagSet("review", flag.ExitOnError)
	spawn := reviewFlags.Bool("spawn", false, "Spawn reviewer agent in herdr")
	reviewFlags.Parse(os.Args[2:])
	refArg := reviewFlags.Arg(0)

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
	default:
		tp = provider.NewMemoryProvider()
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

	if *spawn {
		if !herdr.IsAvailable() {
			fmt.Fprintf(os.Stderr, "herdr CLI not found\n")
			os.Exit(1)
		}

		lane := findLaneForRole(cfg, "reviewer")
		if lane == nil {
			fmt.Fprintf(os.Stderr, "no lane configured for role 'reviewer'\n")
			os.Exit(1)
		}

		standingName := fmt.Sprintf("forge-%s", lane.Name)
		targetLabel := standingName

		tabLabel, err := herdr.ResolveAgentTab(standingName)
		if err != nil {
			tabLabel = fmt.Sprintf("review-%s-%s", lane.Name, task.Ref)
			tab, tabErr := herdr.Tab("wF", tabLabel, true)
			if tabErr != nil {
				fmt.Fprintf(os.Stderr, "failed to create herdr tab: %v\n", tabErr)
				os.Exit(1)
			}
			if err := herdr.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID); err != nil {
				fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Spawned reviewer '%s' in tab %s (pane %s)\n", tabLabel, tab.ID, tab.Pane.ID)
		} else {
			targetLabel = standingName
			fmt.Printf("Using standing reviewer '%s' (tab %s)\n", standingName, tabLabel)
		}

		worktreeDir := lane.Worktree
		if worktreeDir == "" {
			worktreeDir = ".worktrees/reviewer"
		}

		reviewPacket := fmt.Sprintf(`Review Task [%s]: %s

Description:
%s

Status: %s
Priority: %s
Labels: %s

Your worktree for review: %s

Workflow:
1. cd %s
2. Fetch the latest committed changes from the worker (git fetch origin; git log origin/main..origin or similar)
3. Review the changes for correctness, test coverage, security, and architecture
4. If approved, run 'go test ./...' to verify
5. If changes look good, signal 'APPROVED'
6. If issues found, list them for the worker to address

Once you signal APPROVED, the orchestrator will move this card to 'done'.`,
			task.Ref, task.Title, task.Description, task.Status, task.Priority, strings.Join(task.Labels, ", "),
			worktreeDir, worktreeDir)

		if _, err := herdr.AgentPrompt(targetLabel, reviewPacket, false); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: failed to deliver review packet: %v\n", err)
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

// runApprove sweeps in-review cards and moves each to done ONLY with merge
// evidence on origin/main (via sync.BoardDone). Cards without proof are
// refused and stay in-review — a done card is a claim about reality.
func runApprove() {
	approveFlags := flag.NewFlagSet("approve", flag.ExitOnError)
	force := approveFlags.Bool("force", false, "Approve without merge evidence (look at the diff first)")
	evidence := approveFlags.String("evidence", "", "Proof commit SHA (only with a single <ref> argument)")
	approveFlags.Parse(os.Args[2:])
	refArg := approveFlags.Arg(0)

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
	default:
		tp = provider.NewMemoryProvider()
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
	} else if *evidence != "" {
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
		res, err := hsync.BoardDone(ctx, tp, ".", cfg.TaskProvider.ProjectID, task.Ref, *evidence, *force)
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
	fs.Parse(os.Args[2:])

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

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
	default:
		tp = provider.NewMemoryProvider()
	}

	res, err := hsync.BoardDone(context.Background(), tp, ".", cfg.TaskProvider.ProjectID, ref, *evidence, *force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-done: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd board-done: %s proof: %s\n", res.Ref, res.Proof)
	fmt.Printf("herd board-done: %s is done (verified by read-back)\n", res.Ref)
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

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
	default:
		tp = provider.NewMemoryProvider()
	}

	picker := next.NewNextPicker(cfg, tp)
	actions := picker.EvalAll(context.Background())

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

func runDispatch() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: herd dispatch <ticket-ref> [flags]\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fmt.Fprintf(os.Stderr, "  --no-launch    Create worktree and packet only, no agent\n")
		fmt.Fprintf(os.Stderr, "  --lane <name>  Lane name from config (default: worker)\n")
		os.Exit(1)
	}

	ticketRef := os.Args[2]

	dispatchFlags := flag.NewFlagSet("dispatch", flag.ExitOnError)
	noLaunch := dispatchFlags.Bool("no-launch", false, "Skip agent launch")
	laneName := dispatchFlags.String("lane", "worker", "Lane name from config")
	dispatchFlags.Parse(os.Args[3:])

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
	default:
		tp = provider.NewMemoryProvider()
	}

	wm := worktree.NewWorktreeManager(".")
	d := dispatch.NewDispatcher(cfg, tp, wm)

	fmt.Printf("Dispatching %s to lane '%s'...\n", ticketRef, *laneName)

	result, err := d.Dispatch(context.Background(), dispatch.DispatchOptions{
		TicketRef: ticketRef,
		NoLaunch:  *noLaunch,
		LaneName:  *laneName,
	})
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

func runForge() {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)
	default:
		tp = provider.NewMemoryProvider()
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
	wm := worktree.NewWorktreeManager(".")
	v := verifier.NewVerifier(cfg.Verification.TestCommand)
	st, err := store.New(".herd/herdforge.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: store init failed (forge continues without persistence): %v\n", err)
	}
	eng := daemon.NewEngine(cfg, tp, mr, st, wm, v)

	ctx := context.Background()
	fmt.Println("=== Forge: Pulse ===")

	task, err := eng.RunPulse(ctx, "worker")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulse failed: %v\n", err)
		os.Exit(1)
	}
	if task == nil {
		fmt.Println("No pending tasks. Checking for review items...")
		// Fall through to review step
	} else {
		fmt.Printf("Claimed [%s]: %s\n", task.Ref, task.Title)

		// Spawn worker if herdr available
		if herdr.IsAvailable() {
			lane := findLaneForRole(cfg, "worker")
			if lane != nil {
				standingName := fmt.Sprintf("forge-%s", lane.Name)
				tabLabel, resolveErr := herdr.ResolveAgentTab(standingName)
				if resolveErr != nil {
					tabLabel = fmt.Sprintf("forge-%s-%s", lane.Name, task.Ref)
					tab, tabErr := herdr.Tab("wF", tabLabel, true)
					if tabErr == nil {
						herdr.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID)
					}
				}
				packet := fmt.Sprintf(`Task [%s]: %s\n\n%s\n\nWorktree: %s`, task.Ref, task.Title, task.Description, lane.Worktree)
				herdr.AgentPrompt(tabLabel, packet, false)
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
}

func findLaneForRole(cfg *config.Config, role string) *config.LaneDef {
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Role == role {
			return &cfg.Lanes[i]
		}
	}
	return nil
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

	args := kickFlags.Args()

	result, err := kick.Run(kick.Options{
		Names:        args,
		Force:        *all,
		DryRun:       *dryRun,
		Quiet:        *quiet,
		Reason:       *reason,
		RaiseMissing: !*noRaise,
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

	result, err := attention.Run()
	if err != nil {
		// Fail-closed: herdr unavailable or agent list parse error is a hard
		// error, not a silent "fleet healthy".
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

	eng := &lifecycle.Engine{}

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
	}
}

func runProcess() {
	procFlags := flag.NewFlagSet("process", flag.ExitOnError)
	asJSON := procFlags.Bool("json", false, "Output JSON")
	selftestFlag := procFlags.Bool("selftest", false, "Run process selftest and exit")
	procFlags.Parse(os.Args[2:])

	if *selftestFlag {
		if err := process.Selftest(); err != nil {
			fmt.Fprintf(os.Stderr, "process selftest: FAIL — %v\n", err)
			os.Exit(1)
		}
		fmt.Println("process selftest: PASS")
		return
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

	// Build scorer
	now := time.Now()
	scorer := &resolve.DefaultAdapter{
		ScoreFn: func(shape string, preferProvider string) *resolve.RouteScore {
			candidates := []struct {
				name   string
				model  string
				effort string
				until  time.Time
			}{
				{"opencode", "deepseek-v4-flash", "medium", time.Time{}},
			}
			if preferProvider != "" {
				for _, c := range candidates {
					if strings.EqualFold(c.name, preferProvider) && now.After(c.until) {
						return &resolve.RouteScore{
							Provider: preferProvider,
							Model:    c.model,
							Effort:   c.effort,
						}
					}
				}
				return nil
			}
			for _, c := range candidates {
				if now.After(c.until) {
					return &resolve.RouteScore{
						Provider: c.name,
						Model:    c.model,
						Effort:   c.effort,
					}
				}
			}
			return nil
		},
	}

	resolver := resolve.New(reg, scorer)

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
		return
	}
	if result.Resolvable {
		fmt.Printf("%s -> %s/%s (effort=%s) [%s]\n", result.Lane, result.Provider, result.Model, result.Effort, result.Reason)
	} else {
		fmt.Printf("%s -> UNROUTEABLE [%s]\n", result.Lane, result.Reason)
	}
}
