package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/selftest"
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

	case "review":
		runReview()

	case "approve":
		runApprove()

	case "forge":
		runForge()

	case "up":
		runUp()

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
	fmt.Println("  preflight  Run workspace boundary and repo-relative path verification")
	fmt.Println("  selftest   Run core orchestration behavior self-test suite")
	fmt.Println("  status     Display current orchestration engine status")
	fmt.Println("  pulse      Claim a task from Kaneo and optionally spawn an agent")
	fmt.Println("  review     Claim in-progress tasks for reviewer and advance to review status")
	fmt.Println("  approve    Approve a task in review status and move to done")
	fmt.Println("  forge      Full cycle: pulse worker + review + approve")
	fmt.Println("  standing   Launch all configured agent lanes in herdr tabs")
	fmt.Println("  up         Start a single agent lane (herd up <lane-name>)")
	fmt.Println("  --version  Show herd version")
}

func runInit() {
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

	eng := daemon.NewEngine(cfg, tp, mr, wm, v)

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

	var lane *config.LaneConfig
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
		return tasks[i].Ref < tasks[j].Ref
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

	// Move card to "review" status after dispatching
	if err := tp.UpdateStatus(ctx, task.ID, "review"); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: failed to move card to review status: %v\n", err)
	} else {
		fmt.Printf("  -> moved card [%s] to 'review' status\n", task.Ref)
	}
}

func runApprove() {
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

	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "review")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to list review-status tasks: %v\n", err)
		os.Exit(1)
	}

	if len(tasks) == 0 {
		fmt.Println("No tasks in review status to approve.")
		return
	}

	task := tasks[0]
	if err := tp.UpdateStatus(ctx, task.ID, "done"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to approve task: %v\n", err)
		os.Exit(1)
	}

	if err := tp.AddComment(ctx, task.ID, fmt.Sprintf("Approved by forge reviewer (herd approve)")); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: failed to add approval comment: %v\n", err)
	}

	fmt.Printf("Approved [%s]: %s -> moved to done\n", task.Ref, task.Title)
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
	})
	wm := worktree.NewWorktreeManager(".")
	v := verifier.NewVerifier(cfg.Verification.TestCommand)
	eng := daemon.NewEngine(cfg, tp, mr, wm, v)

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
		if err := tp.UpdateStatus(ctx, t.ID, "review"); err == nil {
			fmt.Printf("  -> moved to 'review' status\n")
		}
	}

	fmt.Println("\n=== Forge: Approve ===")
	reviewTasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "review")
	if err == nil && len(reviewTasks) > 0 {
		t := reviewTasks[0]
		if err := tp.UpdateStatus(ctx, t.ID, "done"); err == nil {
			fmt.Printf("Approved [%s]: %s\n", t.Ref, t.Title)
		}
	}

	fmt.Println("\n=== Forge cycle complete ===")
}

func findLaneForRole(cfg *config.Config, role string) *config.LaneConfig {
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Role == role {
			return &cfg.Lanes[i]
		}
	}
	return nil
}
