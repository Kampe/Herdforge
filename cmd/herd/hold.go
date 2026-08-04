package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

func newProductionHoldAuthority() (*lifecycle.HoldAuthority, error) {
	root, err := worktree.ResolveCanonicalRoot(context.Background(), ".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
	if err != nil {
		return nil, fmt.Errorf("resolve canonical hold root: %w", err)
	}
	path := lifecycle.CanonicalStatePath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create hold state directory: %w", err)
	}
	return lifecycle.NewHoldAuthority(path)
}

func holdRepository() (string, error) {
	identity, err := dispatch.AuthenticatedRepositoryIdentity(".")
	if err != nil || strings.TrimSpace(identity) == "" {
		return "", fmt.Errorf("resolve repository identity: %w", err)
	}
	return strings.TrimSpace(identity), nil
}

func canonicalLaneRegistry(cfg *config.Config) (lifecycle.CanonicalLaneRegistry, error) {
	lanes := make([]lifecycle.CanonicalLane, 0, len(cfg.Lanes))
	for _, lane := range cfg.Lanes {
		lanes = append(lanes, lifecycle.CanonicalLane{Name: lane.Name, Role: lane.Role})
	}
	return lifecycle.NewCanonicalLaneRegistry(lanes)
}

func resolveHoldLane(registry lifecycle.CanonicalLaneRegistry, value string, explicitLane bool) (lifecycle.CanonicalLane, error) {
	if explicitLane {
		return registry.ResolveLaneName(value)
	}
	return registry.ResolveRole(value)
}

// composeHoldIdentity validates the complete hold target before opening the
// durable authority or reading its generation. It is intentionally pure so
// invalid lane/role composition cannot cause store or authority effects.
func composeHoldIdentity(cfg *config.Config, laneValue, task, scope string, explicitLane bool, owner, repository string) (lifecycle.HoldIdentity, error) {
	if cfg == nil {
		return lifecycle.HoldIdentity{}, errors.New("hold configuration is required")
	}
	if scope != "lane" && scope != "task" {
		return lifecycle.HoldIdentity{}, fmt.Errorf("invalid hold scope %q", scope)
	}
	registry, err := canonicalLaneRegistry(cfg)
	if err != nil {
		return lifecycle.HoldIdentity{}, err
	}
	lane, err := resolveHoldLane(registry, laneValue, explicitLane)
	if err != nil {
		return lifecycle.HoldIdentity{}, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = lane.Role
	}
	if !strings.EqualFold(owner, lane.Role) {
		return lifecycle.HoldIdentity{}, fmt.Errorf("owner %q does not match configured lane role %q", owner, lane.Role)
	}
	if strings.TrimSpace(repository) == "" || (scope == "task" && strings.TrimSpace(task) == "") {
		return lifecycle.HoldIdentity{}, errors.New("complete repository and task identity are required")
	}
	if scope == "lane" {
		task = ""
	}
	return lifecycle.HoldIdentity{Repository: strings.TrimSpace(repository), Owner: lane.Role, Lane: lane.Name, Task: strings.TrimSpace(task), Scope: scope}, nil
}

type holdAuthorityBoundary interface {
	Close() error
	CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error)
	HasCurrent(context.Context, lifecycle.HoldIdentity) (bool, error)
	Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error)
	Hold(context.Context, lifecycle.HoldIdentity, string, string, string, int64, *time.Time) (lifecycle.HoldRecord, error)
	Release(context.Context, lifecycle.HoldIdentity, string, string, string, int64) (lifecycle.HoldRecord, error)
}

func prepareHoldCommand(cfg *config.Config, laneValue, task, scope string, explicitLane bool, owner, repository string, open func() (holdAuthorityBoundary, error)) (lifecycle.HoldIdentity, holdAuthorityBoundary, error) {
	identity, err := composeHoldIdentity(cfg, laneValue, task, scope, explicitLane, owner, repository)
	if err != nil {
		return lifecycle.HoldIdentity{}, nil, err
	}
	if open == nil {
		return lifecycle.HoldIdentity{}, nil, errors.New("hold authority opener is required")
	}
	authority, err := open()
	if err != nil {
		return lifecycle.HoldIdentity{}, nil, err
	}
	return identity, authority, nil
}

func holdOwner() string {
	if v := strings.TrimSpace(os.Getenv("HERD_OWNER")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("USER")); v != "" {
		return v
	}
	return "herd-cli"
}

func holdIdentity(value, owner, repository string) lifecycle.HoldIdentity {
	return lifecycle.HoldIdentity{Repository: repository, Owner: owner, Lane: value, Scope: "lane"}
}

func productionActiveTasks(ctx context.Context, lane string) ([]lifecycle.HoldIdentity, error) {
	resolver, err := loadProductionActiveTaskResolver(ctx)
	if err != nil {
		return nil, err
	}
	return resolver(ctx, lane)
}

func loadProductionActiveTaskResolver(ctx context.Context) (lifecycle.ActiveTaskResolver, error) {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return nil, err
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		return nil, err
	}
	repository, err := holdRepository()
	if err != nil {
		return nil, err
	}
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "")
	if err != nil {
		return nil, err
	}
	accepted := map[string]bool{"todo": true, "to-do": true, "backlog": true, "planned": true, "in-progress": true, "in_progress": true, "working": true, "started": true, "done": true, "complete": true, "closed": true, "blocked": true, "review": true, "in-review": true}
	configured := map[string]bool{}
	roleLanes := map[string]lifecycle.CanonicalLane{}
	for _, lane := range cfg.Lanes {
		if strings.TrimSpace(lane.Role) != "" {
			role := strings.ToLower(strings.TrimSpace(lane.Role))
			configured[role] = true
			roleLanes[role] = lifecycle.CanonicalLane{Name: strings.TrimSpace(lane.Name), Role: strings.TrimSpace(lane.Role)}
		}
	}
	registry, err := canonicalLaneRegistry(cfg)
	if err != nil {
		return nil, err
	}
	active := map[string][]lifecycle.HoldIdentity{}
	for _, task := range tasks {
		status := strings.ToLower(strings.TrimSpace(task.Status))
		if !accepted[status] {
			return nil, fmt.Errorf("active task resolver: unknown provider status %q", task.Status)
		}
		isActive := status == "in-progress" || status == "in_progress" || status == "working" || status == "started"
		if !isActive {
			continue
		}
		ref := task.Ref
		if ref == "" {
			ref = task.ID
		}
		if ref == "" {
			return nil, lifecycle.ErrActiveTaskUnknown
		}
		for _, label := range task.Labels {
			role := strings.ToLower(strings.TrimSpace(label))
			if configured[role] {
				lane := roleLanes[role]
				active[lane.Name] = append(active[lane.Name], lifecycle.HoldIdentity{Repository: repository, Owner: lane.Role, Lane: lane.Name, Task: ref, Scope: "task"})
			}
		}
	}
	for role := range active {
		sort.Slice(active[role], func(i, j int) bool { return active[role][i].Task < active[role][j].Task })
	}
	return func(_ context.Context, lane string) ([]lifecycle.HoldIdentity, error) {
		canonical, err := registry.ResolveLaneName(lane)
		if err != nil {
			return nil, err
		}
		return append([]lifecycle.HoldIdentity(nil), active[canonical.Name]...), nil
	}, nil
}

func runHold() {
	ctx := context.Background()
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: herd hold <lane-or-task> <on|off|status> [flags]")
		os.Exit(2)
	}
	if os.Args[2] == "--help" || os.Args[2] == "-h" {
		fmt.Println("usage: herd hold <task> on|off|status --lane <configured-lane-name> --owner <configured-role> [--until RFC3339|duration]")
		return
	}
	value := os.Args[2]
	args := append([]string(nil), os.Args[3:]...)
	action := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("hold", flag.ExitOnError)
	actor := fs.String("actor", holdOwner(), "stable actor identifier")
	reason := fs.String("reason", "operator hold", "stable reason")
	code := fs.String("code", "operator_hold", "stable reason code")
	lane := fs.String("lane", value, "configured lane name; required for task scope")
	task := fs.String("task", value, "exact task identity (default: positional value)")
	scope := fs.String("scope", "task", "target scope: task or lane")
	owner := fs.String("owner", "", "configured role; required for task scope")
	repositoryFlag := fs.String("repository", "", "exact repository identity (default: current repository)")
	generation := fs.Int64("generation", 0, "exact generation (default: next for hold, current for release/status)")
	until := fs.String("until", "", "absolute RFC3339 time or bounded duration from now")
	fs.Parse(args)
	if action != "on" && action != "off" && action != "status" {
		fmt.Fprintf(os.Stderr, "hold: unknown action %q\n", action)
		os.Exit(2)
	}
	repository, err := holdRepository()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(*repositoryFlag) != "" {
		repository = strings.TrimSpace(*repositoryFlag)
	}
	if *scope != "task" && *scope != "lane" {
		fmt.Fprintf(os.Stderr, "hold: invalid --scope %q\n", *scope)
		os.Exit(2)
	}
	if *scope == "lane" {
		*task = ""
	}
	laneSet, ownerSet := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "lane" {
			laneSet = true
		}
		if f.Name == "owner" {
			ownerSet = true
		}
	})
	if *scope == "task" && (!laneSet || !ownerSet) {
		fmt.Fprintln(os.Stderr, "hold: task scope requires explicit --lane and --owner")
		os.Exit(2)
	}
	cfg, cfgErr := config.LoadConfig(".herd/herd.yaml")
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", cfgErr)
		os.Exit(1)
	}
	identity, a, err := prepareHoldCommand(cfg, *lane, *task, *scope, laneSet || *scope == "task", *owner, repository, func() (holdAuthorityBoundary, error) {
		root, err := worktree.ResolveCanonicalRoot(ctx, ".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
		if err != nil {
			return nil, err
		}
		return lifecycle.NewHoldAuthority(lifecycle.CanonicalStatePath(root))
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", err)
		os.Exit(2)
	}
	defer a.Close()
	current, err := a.CurrentGeneration(ctx, identity)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", err)
		os.Exit(1)
	}
	gen := *generation
	if gen == 0 {
		gen = current
		if action == "on" {
			exists, existsErr := a.HasCurrent(ctx, identity)
			if existsErr != nil {
				fmt.Fprintf(os.Stderr, "hold: %v\n", existsErr)
				os.Exit(1)
			}
			if !exists {
				gen = 1
			} else if decision, checkErr := a.Check(ctx, identity, current); checkErr == nil && !decision.Held {
				gen = current + 1
			}
		}
	}
	if action == "status" {
		decision, checkErr := a.Check(ctx, identity, gen)
		if checkErr != nil {
			if errors.Is(checkErr, lifecycle.ErrHoldMissing) {
				fmt.Fprintf(os.Stderr, "hold: %v\n", checkErr)
			} else {
				fmt.Fprintf(os.Stderr, "hold status: %v\n", checkErr)
			}
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"repository": identity.Repository, "owner": identity.Owner, "lane": identity.Lane, "task": identity.Task, "generation": decision.Generation, "held": decision.Held, "reason": decision.Reason, "code": decision.Code})
		return
	}
	var record lifecycle.HoldRecord
	if action == "on" {
		expires, parseErr := parseHoldExpiry(*until, time.Now)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "hold: %v\n", parseErr)
			os.Exit(1)
		}
		record, err = a.Hold(ctx, identity, *actor, *reason, *code, gen, expires)
	} else {
		record, err = a.Release(ctx, identity, *actor, *reason, "operator_release", gen)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hold %s: %v\n", action, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", err)
		os.Exit(1)
	}
}

func parseHoldExpiry(raw string, now func() time.Time) (*time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return &t, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > 7*24*time.Hour {
		return nil, fmt.Errorf("invalid --until %q: use RFC3339 or a duration from 1ns through 168h", raw)
	}
	t := now().Add(d)
	return &t, nil
}
