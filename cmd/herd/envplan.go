package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/envplan"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
)

const environmentPlanStorePath = ".herd/environment-plans.db"

func runEnvPlan() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, subcommandUsage["envplan"])
		os.Exit(2)
	}
	switch os.Args[2] {
	case "create":
		runEnvPlanCreate(os.Args[3:])
	case "inspect":
		runEnvPlanInspect(os.Args[3:])
	case "grant":
		runEnvPlanGrant(os.Args[3:])
	case "revoke":
		runEnvPlanRevoke(os.Args[3:])
	default:
		fmt.Fprintf(os.Stderr, "envplan: unknown action %q\n", os.Args[2])
		os.Exit(2)
	}
}

func openEnvironmentPlanStore() (*envplan.Store, error) {
	return envplan.Open(filepath.Clean(environmentPlanStorePath))
}

func parseEnvironmentCapability(value string) (envplan.Capability, error) {
	capability := envplan.Capability(strings.TrimSpace(value))
	switch capability {
	case envplan.CapabilityNetwork, envplan.CapabilityBoardWrite, envplan.CapabilityCredential:
		return capability, nil
	default:
		return "", fmt.Errorf("unknown environment capability %q", value)
	}
}

func environmentBinding(ctx context.Context, ticketRef string) (envplan.Binding, error) {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("load config: %w", err)
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("task provider: %w", err)
	}
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "")
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("list tasks: %w", err)
	}
	var task *provider.Task
	for _, candidate := range tasks {
		if candidate != nil && candidate.Ref == ticketRef {
			task = candidate
			break
		}
	}
	if task == nil || strings.TrimSpace(task.ID) == "" {
		return envplan.Binding{}, fmt.Errorf("task %s not found", ticketRef)
	}
	graph, _ := publishedGraphBinding(".")
	if strings.TrimSpace(graph) == "" {
		return envplan.Binding{}, errors.New("published dependency graph revision is required before creating an environment plan")
	}
	runs, err := runstate.Open(filepath.Join(".herd", "dispatch-runs.db"))
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("open dispatch runstate: %w", err)
	}
	defer runs.Close()
	authority := runstate.Authority{Tasks: tp, Graph: func(context.Context) (string, error) { return graph, nil }}
	runID := "dispatch:" + task.ID
	run, err := runs.Resume(ctx, runID, authority)
	if errors.Is(err, runstate.ErrNotFound) {
		next, buildErr := runstate.FromTasks(runID, "dispatch", task.Ref, graph, runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
		if buildErr != nil {
			return envplan.Binding{}, fmt.Errorf("build dispatch runstate: %w", buildErr)
		}
		if _, checkpointErr := runs.Checkpoint(ctx, next, 0); checkpointErr != nil {
			return envplan.Binding{}, fmt.Errorf("checkpoint dispatch runstate: %w", checkpointErr)
		}
		run, err = runs.Resume(ctx, runID, authority)
	}
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("resume dispatch runstate: %w", err)
	}
	if err := run.Dispatchable(task.Ref); err != nil {
		return envplan.Binding{}, fmt.Errorf("dispatch runstate: %w", err)
	}
	for _, saved := range run.Tasks {
		if saved.ID == task.ID && saved.Ref == task.Ref {
			return envplan.Binding{TaskRef: task.Ref, TaskID: task.ID, Provider: cfg.TaskProvider.Type, ProviderRevision: saved.ProviderRevision, GraphRevision: run.DependencyGraphRevision, RunID: run.ID, RunRevision: run.Revision}, nil
		}
	}
	return envplan.Binding{}, errors.New("dispatch runstate omitted requested task")
}

func runEnvPlanCreate(args []string) {
	fs := flag.NewFlagSet("envplan create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	expiresAt := fs.String("expires-at", "", "RFC3339 expiry (required)")
	capabilities := multiStringFlag{}
	fs.Var(&capabilities, "capability", "Requested capability: board-write, network, credential-broker (repeatable)")
	if err := fs.Parse(leadingPositionalArgs(args)); err != nil || len(fs.Args()) != 1 {
		fmt.Fprintln(os.Stderr, "usage: herd envplan create <ticket-ref> --expires-at <RFC3339> --capability <capability>")
		os.Exit(2)
	}
	expiry, err := time.Parse(time.RFC3339, *expiresAt)
	if err != nil || !expiry.After(time.Now().UTC()) {
		fmt.Fprintln(os.Stderr, "envplan create: --expires-at must be a future RFC3339 timestamp")
		os.Exit(2)
	}
	binding, err := environmentBinding(context.Background(), fs.Args()[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan create: %v\n", err)
		os.Exit(1)
	}
	requests := make([]envplan.Request, 0, len(capabilities))
	for _, raw := range capabilities {
		capability, capErr := parseEnvironmentCapability(raw)
		if capErr != nil {
			fmt.Fprintf(os.Stderr, "envplan create: %v\n", capErr)
			os.Exit(2)
		}
		requests = append(requests, envplan.Request{Capability: capability, Evidence: envplan.Evidence{Authority: "security", Revision: binding.GraphRevision, Subject: "task:" + binding.TaskRef}})
	}
	store, err := openEnvironmentPlanStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan create: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	plan, err := store.Create(context.Background(), envplan.Plan{Binding: binding, Requests: requests, ExpiresAt: expiry})
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan create: %v\n", err)
		os.Exit(1)
	}
	writeEnvironmentPlan(plan)
}

func runEnvPlanInspect(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: herd envplan inspect <plan-id>")
		os.Exit(2)
	}
	store, err := openEnvironmentPlanStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan inspect: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	plan, err := store.Load(context.Background(), args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan inspect: %v\n", err)
		os.Exit(1)
	}
	writeEnvironmentPlan(plan)
}

func runEnvPlanGrant(args []string)  { runEnvPlanGrantOrRevoke(args, false) }
func runEnvPlanRevoke(args []string) { runEnvPlanGrantOrRevoke(args, true) }

func runEnvPlanGrantOrRevoke(args []string, revoke bool) {
	name := "grant"
	if revoke {
		name = "revoke"
	}
	fs := flag.NewFlagSet("envplan "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	planID := fs.String("plan", "", "Exact environment plan ID")
	capability := fs.String("capability", "", "Requested capability")
	operator := fs.String("operator", "", "Attributable operator identity")
	expiresAt := fs.String("expires-at", "", "Future RFC3339 grant expiry")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	cap, err := parseEnvironmentCapability(*capability)
	if err != nil || strings.TrimSpace(*planID) == "" || (!revoke && strings.TrimSpace(*operator) == "") {
		fmt.Fprintf(os.Stderr, "envplan %s: --plan, --capability%s are required\n", name, map[bool]string{true: "", false: ", and --operator"}[revoke])
		os.Exit(2)
	}
	store, err := openEnvironmentPlanStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan %s: %v\n", name, err)
		os.Exit(1)
	}
	defer store.Close()
	var plan *envplan.Plan
	if revoke {
		plan, err = store.Revoke(context.Background(), *planID, cap)
	} else {
		expiry, parseErr := time.Parse(time.RFC3339, *expiresAt)
		if parseErr != nil || !expiry.After(time.Now().UTC()) {
			fmt.Fprintln(os.Stderr, "envplan grant: --expires-at must be a future RFC3339 timestamp")
			os.Exit(2)
		}
		plan, err = store.Grant(context.Background(), *planID, cap, *operator, expiry)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan %s: %v\n", name, err)
		os.Exit(1)
	}
	writeEnvironmentPlan(plan)
}

func writeEnvironmentPlan(plan *envplan.Plan) {
	if err := json.NewEncoder(os.Stdout).Encode(plan); err != nil {
		fmt.Fprintf(os.Stderr, "envplan output: %v\n", err)
		os.Exit(1)
	}
}

type multiStringFlag []string

func (f *multiStringFlag) String() string         { return strings.Join(*f, ",") }
func (f *multiStringFlag) Set(value string) error { *f = append(*f, value); return nil }
