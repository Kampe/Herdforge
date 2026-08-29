package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/envplan"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/security"
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

type environmentBindingRequest struct {
	TicketRef    string
	TaskID       string
	RecoverStale bool
}

type environmentBindingAuthorities struct {
	Provider      string
	ProjectID     string
	Tasks         provider.TaskProvider
	GraphRevision string
	Runs          *runstate.Store
	Claims        security.LiveClaimLookup
	Launches      dispatch.LiveLaunchLookup
}

func environmentBinding(ctx context.Context, ticketRef string) (envplan.Binding, error) {
	return environmentBindingForRequest(ctx, environmentBindingRequest{TicketRef: ticketRef})
}

func environmentBindingForRequest(ctx context.Context, req environmentBindingRequest) (envplan.Binding, error) {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("load config: %w", err)
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("task provider: %w", err)
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
	authorities := environmentBindingAuthorities{Provider: cfg.TaskProvider.Type, ProjectID: cfg.TaskProvider.ProjectID, Tasks: tp, GraphRevision: graph, Runs: runs}
	if req.RecoverStale {
		if err := security.WireCanonicalClaimAuthority("."); err != nil {
			return envplan.Binding{}, fmt.Errorf("wire recovery claim authority: %w", err)
		}
		authorities.Claims, err = security.RequireClaimAuthority()
		if err != nil {
			return envplan.Binding{}, fmt.Errorf("recovery claim authority: %w", err)
		}
		authorities.Launches = dispatch.NewReceiptLiveLaunchLookup(launch.DefaultReceiptPath(), nil)
	}
	return environmentBindingFromAuthorities(ctx, req, authorities)
}

func environmentBindingFromAuthorities(ctx context.Context, req environmentBindingRequest, a environmentBindingAuthorities) (envplan.Binding, error) {
	if strings.TrimSpace(req.TicketRef) == "" || strings.TrimSpace(a.Provider) == "" || strings.EqualFold(strings.TrimSpace(a.Provider), "unknown") || strings.TrimSpace(a.ProjectID) == "" || a.Tasks == nil || a.Runs == nil || strings.TrimSpace(a.GraphRevision) == "" {
		return envplan.Binding{}, fmt.Errorf("environment binding: %w: incomplete provider, project, graph, or task authority", runstate.ErrAmbiguous)
	}
	var task *provider.Task
	var err error
	if strings.TrimSpace(req.TaskID) != "" {
		task, err = a.Tasks.GetTask(ctx, strings.TrimSpace(req.TaskID))
		if err != nil {
			return envplan.Binding{}, fmt.Errorf("get exact task: %w", err)
		}
	} else {
		tasks, listErr := a.Tasks.ListTasks(ctx, a.ProjectID, "")
		if listErr != nil {
			return envplan.Binding{}, fmt.Errorf("list tasks: %w", listErr)
		}
		for _, candidate := range tasks {
			if candidate == nil || candidate.Ref != req.TicketRef {
				continue
			}
			if task != nil {
				return envplan.Binding{}, fmt.Errorf("environment binding: %w: multiple tasks have ref %s", runstate.ErrAmbiguous, req.TicketRef)
			}
			task = candidate
		}
	}
	if task == nil || task.ID == "" || task.Ref != req.TicketRef || task.ProjectID != a.ProjectID {
		return envplan.Binding{}, fmt.Errorf("environment binding: %w: exact task/ref/project mismatch", runstate.ErrAmbiguous)
	}

	runID := "dispatch:" + task.ID
	graphAuthority := func(context.Context) (string, error) { return a.GraphRevision, nil }
	var run *runstate.RunState
	if req.RecoverStale {
		if strings.TrimSpace(req.TaskID) == "" {
			return envplan.Binding{}, fmt.Errorf("environment binding: %w: stale recovery requires exact task id", runstate.ErrAmbiguous)
		}
		recovery := dispatch.StaleRunRecovery{Runs: a.Runs, Tasks: a.Tasks, ProjectID: a.ProjectID, Graph: graphAuthority, Claims: a.Claims, Launches: a.Launches}
		run, err = recovery.Recover(ctx, task.ID, task.Ref)
	} else {
		authority := runstate.Authority{Tasks: a.Tasks, Graph: graphAuthority}
		run, err = a.Runs.Resume(ctx, runID, authority)
		if errors.Is(err, runstate.ErrNotFound) {
			next, buildErr := runstate.FromTasks(runID, "dispatch", task.Ref, a.GraphRevision, runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
			if buildErr != nil {
				return envplan.Binding{}, fmt.Errorf("build dispatch runstate: %w", buildErr)
			}
			if _, checkpointErr := a.Runs.Checkpoint(ctx, next, 0); checkpointErr != nil {
				return envplan.Binding{}, fmt.Errorf("checkpoint dispatch runstate: %w", checkpointErr)
			}
			run, err = a.Runs.Resume(ctx, runID, authority)
		}
	}
	if err != nil {
		return envplan.Binding{}, fmt.Errorf("resume dispatch runstate: %w", err)
	}
	if err := run.Dispatchable(task.Ref); err != nil {
		return envplan.Binding{}, fmt.Errorf("dispatch runstate: %w", err)
	}
	for _, saved := range run.Tasks {
		if saved.ID == task.ID && saved.Ref == task.Ref {
			return envplan.Binding{TaskRef: task.Ref, TaskID: task.ID, Provider: a.Provider, ProviderRevision: saved.ProviderRevision, GraphRevision: run.DependencyGraphRevision, RunID: run.ID, RunRevision: run.Revision}, nil
		}
	}
	return envplan.Binding{}, errors.New("dispatch runstate omitted requested task")
}

type environmentPlanCreateRequest struct {
	Binding      environmentBindingRequest
	ExpiresAt    time.Time
	Capabilities []string
}

func parseEnvironmentPlanCreateArgs(args []string, now time.Time) (environmentPlanCreateRequest, error) {
	fs := flag.NewFlagSet("envplan create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	expiresAt := fs.String("expires-at", "", "RFC3339 expiry (required)")
	taskID := fs.String("task-id", "", "Exact provider task ID")
	recoverStale := fs.Bool("recover-stale-run", false, "Explicitly recover the exact stale dispatch run")
	capabilities := multiStringFlag{}
	fs.Var(&capabilities, "capability", "Requested capability: board-write, network, credential-broker (repeatable)")
	if err := fs.Parse(leadingPositionalArgs(args)); err != nil || len(fs.Args()) != 1 {
		return environmentPlanCreateRequest{}, errors.New("usage: herd envplan create <ticket-ref> --expires-at <RFC3339> --capability <capability> [--task-id <id> --recover-stale-run]")
	}
	expiry, err := time.Parse(time.RFC3339, *expiresAt)
	if err != nil || !expiry.After(now.UTC()) {
		return environmentPlanCreateRequest{}, errors.New("--expires-at must be a future RFC3339 timestamp")
	}
	if *recoverStale && strings.TrimSpace(*taskID) == "" {
		return environmentPlanCreateRequest{}, errors.New("--recover-stale-run requires exact --task-id")
	}
	return environmentPlanCreateRequest{
		Binding:      environmentBindingRequest{TicketRef: fs.Args()[0], TaskID: strings.TrimSpace(*taskID), RecoverStale: *recoverStale},
		ExpiresAt:    expiry.UTC(),
		Capabilities: append([]string(nil), capabilities...),
	}, nil
}

func runEnvPlanCreate(args []string) {
	req, err := parseEnvironmentPlanCreateArgs(args, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan create: %v\n", err)
		os.Exit(2)
	}
	binding, err := environmentBindingForRequest(context.Background(), req.Binding)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envplan create: %v\n", err)
		os.Exit(1)
	}
	requests := make([]envplan.Request, 0, len(req.Capabilities))
	for _, raw := range req.Capabilities {
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
	plan, err := store.Create(context.Background(), envplan.Plan{Binding: binding, Requests: requests, ExpiresAt: req.ExpiresAt})
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
