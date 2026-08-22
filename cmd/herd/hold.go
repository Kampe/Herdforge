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
	"github.com/Kampe/Herdforge/pkg/standing"
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
		lanes = append(lanes, lifecycle.CanonicalLane{Name: lane.Name, Role: lane.Role, Standing: lane.Standing})
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
	ReleaseAndRearm(context.Context, lifecycle.HoldIdentity, string, string, int64) (lifecycle.LoopState, error)
}

type holdCommandRequest struct {
	Config       *config.Config
	LaneValue    string
	Task         string
	Scope        string
	ExplicitLane bool
	Owner        string
	Action       string
	Actor        string
	Reason       string
	Code         string
	Repository   string
	Generation   int64
	Until        string
}

type holdCommandDependencies struct {
	AuthenticateRepository func() (string, error)
	OpenAuthority          func() (holdAuthorityBoundary, error)
	Encode                 func(any) error
	Flush                  func() error
	Now                    func() time.Time
}

func closeHoldAuthority(primary error, authority holdAuthorityBoundary) error {
	if authority == nil {
		return primary
	}
	return errors.Join(primary, authority.Close())
}

// executeHoldCommand is the non-exiting hold command adapter. All target and
// repository validation completes before OpenAuthority, and every authority,
// output, flush, and close error remains observable to the caller.
func executeHoldCommand(ctx context.Context, req holdCommandRequest, deps holdCommandDependencies) (err error) {
	if req.Action != "on" && req.Action != "off" && req.Action != "status" {
		return fmt.Errorf("unknown hold action %q", req.Action)
	}
	if deps.AuthenticateRepository == nil {
		return errors.New("authenticated repository resolver is required")
	}
	if deps.OpenAuthority == nil {
		return errors.New("hold authority opener is required")
	}
	if deps.Encode == nil {
		return errors.New("hold output encoder is required")
	}
	if deps.Flush == nil {
		return errors.New("hold output flush is required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if req.Owner != strings.TrimSpace(req.Owner) {
		return errors.New("hold owner must be canonical")
	}
	if req.Scope == "task" && strings.TrimSpace(req.Owner) == "" {
		return errors.New("task hold owner is required")
	}

	authenticated, err := deps.AuthenticateRepository()
	if err != nil {
		return fmt.Errorf("resolve repository identity: %w", err)
	}
	if strings.TrimSpace(authenticated) == "" || authenticated != strings.TrimSpace(authenticated) {
		return errors.New("authenticated repository identity is empty or noncanonical")
	}
	if req.Repository != "" && (req.Repository != authenticated || req.Repository != strings.TrimSpace(req.Repository)) {
		return fmt.Errorf("repository override %q does not match authenticated repository", req.Repository)
	}

	identity, err := composeHoldIdentity(req.Config, req.LaneValue, req.Task, req.Scope, req.ExplicitLane, req.Owner, authenticated)
	if err != nil {
		return err
	}
	authority, err := deps.OpenAuthority()
	if err != nil {
		return err
	}
	defer func() { err = closeHoldAuthority(err, authority) }()

	current, err := authority.CurrentGeneration(ctx, identity)
	if err != nil {
		return err
	}
	gen := req.Generation
	if gen == 0 {
		gen = current
		if req.Action == "on" {
			exists, existsErr := authority.HasCurrent(ctx, identity)
			if existsErr != nil {
				return existsErr
			}
			if !exists {
				gen = 1
			} else {
				decision, checkErr := authority.Check(ctx, identity, current)
				if checkErr != nil {
					return checkErr
				}
				if !decision.Held {
					gen = current + 1
				}
			}
		}
	}

	var receipt any
	switch req.Action {
	case "status":
		decision, checkErr := authority.Check(ctx, identity, gen)
		if checkErr != nil {
			return checkErr
		}
		receipt = map[string]any{"repository": identity.Repository, "owner": identity.Owner, "lane": identity.Lane, "task": identity.Task, "generation": decision.Generation, "held": decision.Held, "reason": decision.Reason, "code": decision.Code}
	case "on":
		expires, parseErr := parseHoldExpiry(req.Until, deps.Now)
		if parseErr != nil {
			return parseErr
		}
		record, holdErr := authority.Hold(ctx, identity, req.Actor, req.Reason, req.Code, gen, expires)
		if holdErr != nil {
			return holdErr
		}
		receipt = record
	case "off":
		if identity.Scope == "lane" {
			state, releaseErr := authority.ReleaseAndRearm(ctx, identity, req.Actor, req.Reason, gen)
			if releaseErr != nil {
				return releaseErr
			}
			receipt = state
			break
		}
		record, releaseErr := authority.Release(ctx, identity, req.Actor, req.Reason, "operator_release", gen)
		if releaseErr != nil {
			return releaseErr
		}
		receipt = record
	}
	if err := deps.Encode(receipt); err != nil {
		return err
	}
	return deps.Flush()
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
	cfg, err := config.LoadConfig(config.DefaultConfigPath)
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
	configured := map[string]bool{}
	roleLanes := map[string]lifecycle.CanonicalLane{}
	for _, lane := range cfg.Lanes {
		if strings.TrimSpace(lane.Role) != "" {
			role := strings.ToLower(strings.TrimSpace(lane.Role))
			configured[role] = true
			roleLanes[role] = lifecycle.CanonicalLane{Name: strings.TrimSpace(lane.Name), Role: strings.TrimSpace(lane.Role), Standing: lane.Standing}
		}
	}
	registry, err := canonicalLaneRegistry(cfg)
	if err != nil {
		return nil, err
	}
	active := map[string][]lifecycle.HoldIdentity{}
	for _, task := range tasks {
		status := strings.ToLower(strings.TrimSpace(task.Status))
		if !activeResolverAcceptsStatus(status) {
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
	cfg, cfgErr := config.LoadConfig(config.DefaultConfigPath)
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", cfgErr)
		os.Exit(1)
	}
	err := executeHoldCommand(ctx, holdCommandRequest{Config: cfg, LaneValue: *lane, Task: *task, Scope: *scope, ExplicitLane: laneSet || *scope == "task", Owner: *owner, Action: action, Actor: *actor, Reason: *reason, Code: *code, Repository: *repositoryFlag, Generation: *generation, Until: *until}, holdCommandDependencies{
		AuthenticateRepository: holdRepository,
		OpenAuthority: func() (holdAuthorityBoundary, error) {
			root, err := worktree.ResolveCanonicalRoot(ctx, ".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
			if err != nil {
				return nil, err
			}
			return lifecycle.NewHoldAuthority(lifecycle.CanonicalStatePath(root))
		},
		Encode: json.NewEncoder(os.Stdout).Encode,
		Flush:  func() error { return nil },
		Now:    time.Now,
	})
	if err != nil {
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

// resolveLaneLoopMode reads a lane's loop contract from the durable hold store.
//
// FAC-524: standing previously took this from the live agent list, but herdr
// emits no loop_mode field at all, so every lane defaulted to running and a
// held lane was indistinguishable from an available one. Herdforge owns this
// state, so it is read from Herdforge's own store.
//
// A lane with no configured loop is not an error: it has simply never been
// given a standing contract, and reports an empty mode so the caller keeps its
// existing default.
func resolveLaneLoopMode(cfg *config.Config, laneName string) (standing.LoopMode, error) {
	if cfg == nil {
		return "", errors.New("hold configuration is required")
	}
	registry, err := canonicalLaneRegistry(cfg)
	if err != nil {
		return "", err
	}
	lane, err := resolveHoldLane(registry, laneName, true)
	if err != nil {
		return "", err
	}
	repository, err := holdRepository()
	if err != nil {
		return "", err
	}
	authority, err := newProductionHoldAuthority()
	if err != nil {
		return "", err
	}
	defer authority.Close()

	state, err := authority.Loop(context.Background(),
		lifecycle.HoldIdentity{Repository: repository, Owner: lane.Role, Lane: lane.Name, Scope: "lane"})
	if err != nil {
		// No configured loop for this lane; not a failure.
		return "", nil
	}
	return standing.LoopMode(state.Mode), nil
}

// activeResolverAcceptsStatus reports whether a provider status is one the
// active-task resolver understands.
//
// Every canonical provider status must be listed. "archived" was missing, so
// one archived card made the resolver fail closed and took `herd attention`
// down for the entire fleet -- pushing coordinators to raw herdr for triage
// Herdforge was supposed to provide. Terminal statuses are accepted here and
// then skipped as not active, exactly like done. A genuinely unknown status
// still fails closed: silently treating it as inactive would hide real work.
func activeResolverAcceptsStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "todo", "to-do", "backlog", "planned",
		"in-progress", "in_progress", "working", "started",
		"review", "in-review",
		"done", "complete", "closed", "blocked",
		"archived", "archive":
		return true
	}
	return false
}
