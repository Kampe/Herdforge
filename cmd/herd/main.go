package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/verifier"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

const version = "0.1.0-dev"

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

	case "status":
		runStatus()

	case "pulse":
		runPulse()

	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand '%s'\nRun 'herd --help' for usage.\n", command)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Herdforge: Standalone Multi-Agent Orchestration Daemon")
	fmt.Println("\nUsage:")
	fmt.Println("  herd <command> [flags]")
	fmt.Println("\nCommands:")
	fmt.Println("  init       Scaffold default .herd/herd.yaml configuration file")
	fmt.Println("  preflight  Run workspace boundary and repo-relative path verification")
	fmt.Println("  status     Display current orchestration engine status")
	fmt.Println("  pulse      Execute one orchestration sweep pass across task queue")
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

model_providers:
  - name: "claude-pro"
    type: "anthropic"
    model: "claude-3-7-sonnet"
  - name: "gemini-flash"
    type: "google"
    model: "gemini-2.5-flash"

roles:
  - name: "herd-smith"
    provider: "claude-pro"
    fallback_provider: "gemini-flash"
    prompt_path: ".herd/prompts/smith.md"

verification:
  test_command: "go test ./..."
`

	if err := os.WriteFile(cfgPath, []byte(defaultConfig), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write default config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Scaffolded .herd/herd.yaml successfully.")
}

func runPreflight() {
	if err := preflight.CheckWorktreeBoundary("."); err != nil {
		fmt.Fprintf(os.Stderr, "Preflight failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Preflight boundary check passed. Zero absolute path leaks detected.")
}

func runStatus() {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Printf("Status: Uninitialized (no valid .herd/herd.yaml found)\n")
		return
	}
	fmt.Printf("Status: Active\nProject: %s\nProvider: %s\nRoles: %d configured\n",
		cfg.Project.Name, cfg.TaskProvider.Type, len(cfg.Roles))
}

func runPulse() {
	pulseFlags := flag.NewFlagSet("pulse", flag.ExitOnError)
	role := pulseFlags.String("role", "herd-smith", "Target role to run pulse sweep for")
	pulseFlags.Parse(os.Args[2:])

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var tp provider.TaskProvider
	switch cfg.TaskProvider.Type {
	case "kaneo":
		tp = provider.NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID)
	case "github":
		tp = provider.NewGitHubProvider(os.Getenv("GITHUB_TOKEN"), "owner", "repo")
	default:
		tp = provider.NewMemoryProvider()
	}

	mr := router.NewModelRouter([]*router.ModelCandidate{
		{Name: "claude-pro", Type: router.ProviderAnthropic, Model: "claude-3-7-sonnet"},
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
	} else {
		fmt.Printf("Pulse sweep claimed task [%s]: %s\n", task.Ref, task.Title)
	}
}
