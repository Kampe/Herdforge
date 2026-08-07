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

	"github.com/Kampe/Herdforge/pkg/posture"
	"github.com/Kampe/Herdforge/pkg/router"
)

// runFamilyPosture implements:
//
//	herd posture claude-only|no-claude|clear|status
//
// It is the generation-fenced family policy (FAC-102). The legacy
// `herd claude-only` / `herd no-claude` entry points remain as thin
// compatibility wrappers over the same authority.
func runFamilyPosture() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: herd posture <claude-only|no-claude|clear|status> [flags]")
		os.Exit(2)
	}
	action := os.Args[2]
	fs := flag.NewFlagSet("posture", flag.ExitOnError)
	actor := fs.String("actor", postureActor(), "stable actor identifier")
	reason := fs.String("reason", "", "nonempty bounded reason for the posture change")
	generation := fs.Uint64("generation", 0, "monotonic generation (default: next durable generation)")
	scope := fs.String("scope", "fleet", "scope of the policy (stable identifier)")
	deadline := fs.String("expires", "", "optional RFC3339Nano expiry")
	shape := fs.String("shape", "implementation", "shape used by status candidate report")
	fs.Parse(os.Args[3:])

	a, err := newFamilyPostureAuthority()
	if err != nil {
		fmt.Fprintf(os.Stderr, "posture: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()

	if action == "status" {
		printFamilyPostureStatus(ctx, a, *shape)
		return
	}

	var mode posture.Mode
	switch action {
	case "claude-only":
		mode = posture.ModeClaudeOnly
	case "no-claude":
		mode = posture.ModeNoClaude
	case "clear":
		mode = posture.ModeClear
	case "-h", "--help":
		fmt.Fprintln(os.Stdout, "Usage: herd posture <claude-only|no-claude|clear|status> [--actor] [--reason] [--generation] [--scope] [--expires]")
		return
	default:
		fmt.Fprintf(os.Stderr, "posture: unknown action %q (claude-only|no-claude|clear|status)\n", action)
		os.Exit(2)
	}

	if strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(os.Stderr, "posture: --reason is required for claude-only|no-claude|clear")
		os.Exit(2)
	}

	var until *time.Time
	if strings.TrimSpace(*deadline) != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, *deadline)
		if parseErr != nil || parsed.UTC().Format(time.RFC3339Nano) != *deadline {
			fmt.Fprintf(os.Stderr, "posture: invalid --expires %q\n", *deadline)
			os.Exit(2)
		}
		parsed = parsed.UTC()
		until = &parsed
	}

	current, readErr := a.Read(ctx)
	if readErr != nil && !errors.Is(readErr, posture.ErrStateMissing) {
		fmt.Fprintf(os.Stderr, "posture: %v\n", readErr)
		os.Exit(1)
	}
	gen := *generation
	if gen == 0 {
		if readErr == nil {
			candidate := posture.State{
				Mode: mode, Actor: *actor, Reason: *reason,
				Generation: current.Generation, Scope: *scope, ExpiresAt: until,
			}
			if current.Mode == candidate.Mode && current.Actor == candidate.Actor &&
				current.Reason == candidate.Reason && current.Generation == candidate.Generation &&
				current.Scope == candidate.Scope && sameFamilyDeadline(current.ExpiresAt, candidate.ExpiresAt) {
				if err := json.NewEncoder(os.Stdout).Encode(current); err != nil {
					fmt.Fprintf(os.Stderr, "posture: %v\n", err)
					os.Exit(1)
				}
				return
			}
			gen = current.Generation + 1
		} else {
			gen = 1
		}
	}
	state, updateErr := a.Update(ctx, mode, *actor, *reason, *scope, gen, until)
	if updateErr != nil {
		fmt.Fprintf(os.Stderr, "posture: %v\n", updateErr)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
		fmt.Fprintf(os.Stderr, "posture: %v\n", err)
		os.Exit(1)
	}
}

func printFamilyPostureStatus(ctx context.Context, a *posture.Authority, shape string) {
	mode, state, err := posture.Effective(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "posture status: %v\n", err)
		os.Exit(1)
	}
	// Durable bytes (may differ from effective when expired or env-overridden).
	durable, durableErr := a.Read(ctx)
	type statusOut struct {
		EffectiveMode string              `json:"effective_mode"`
		Durable       *posture.State      `json:"durable,omitempty"`
		DurableError  string              `json:"durable_error,omitempty"`
		Effective     posture.State       `json:"effective"`
		EnvOverrides  map[string]string   `json:"env_overrides,omitempty"`
		Shape         string              `json:"shape"`
		Candidates    []string            `json:"candidates"`
		Excluded      []posture.Exclusion `json:"excluded"`
	}
	out := statusOut{
		EffectiveMode: posture.ModeLabel(mode),
		Effective:     state,
		Shape:         shape,
		EnvOverrides:  map[string]string{},
	}
	if durableErr == nil {
		out.Durable = &durable
	} else if !errors.Is(durableErr, posture.ErrStateMissing) {
		out.DurableError = durableErr.Error()
	}
	if v, set := posture.EnvOverride(posture.ClaudeOnly); set {
		out.EnvOverrides[posture.ClaudeOnly.EnvVar()] = fmt.Sprintf("%v", v)
	}
	if v, set := posture.EnvOverride(posture.NoClaude); set {
		out.EnvOverrides[posture.NoClaude.EnvVar()] = fmt.Sprintf("%v", v)
	}

	providers, werr := router.Waterfall(shape)
	if werr != nil {
		fmt.Fprintf(os.Stderr, "posture status: waterfall: %v\n", werr)
		os.Exit(1)
	}
	// Provider-level filter first.
	keptProviders, excl := posture.FilterProviders(mode, providers)
	// Family-level filter with resolved default models so status shows
	// anthropic-via-agy exclusions under no-claude.
	var cands []posture.Candidate
	for _, p := range keptProviders {
		model := router.ModelFor(p, shape)
		cands = append(cands, posture.Candidate{
			Provider: p,
			Model:    model,
			Family:   router.FamilyFor(p, model),
		})
	}
	kept, famExcl := posture.Filter(mode, cands)
	excl = append(excl, famExcl...)
	for _, c := range kept {
		out.Candidates = append(out.Candidates, fmt.Sprintf("%s/%s (%s)", c.Provider, c.Model, c.Family))
	}
	out.Excluded = excl
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "posture status: %v\n", err)
		os.Exit(1)
	}
}

func newFamilyPostureAuthority() (*posture.Authority, error) {
	path := posture.DefaultStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create family posture state directory: %w", err)
	}
	return posture.New(path, nil)
}

func postureActor() string {
	if actor := strings.TrimSpace(os.Getenv("HERD_ACTOR")); actor != "" {
		return actor
	}
	if actor := strings.TrimSpace(os.Getenv("USER")); actor != "" {
		return actor
	}
	return "herd-cli"
}

func sameFamilyDeadline(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
