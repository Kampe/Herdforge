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

	"github.com/Kampe/Herdforge/pkg/winddown"
)

const defaultWinddownStatePath = ".herd/winddown.json"

func winddownStatePath() string {
	if path := strings.TrimSpace(os.Getenv("HERD_WINDDOWN_STATE")); path != "" {
		return path
	}
	return defaultWinddownStatePath
}

func newWinddownAuthority() (*winddown.Authority, error) {
	path := winddownStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create wind-down state directory: %w", err)
	}
	return winddown.New(path, nil)
}

// requireFleetAdmission is the one production posture gate for work that can
// claim or re-engage fleet capacity. Missing, corrupt, and unreadable state
// are deliberately rejected by Authority.Gate.
func requireFleetAdmission(ctx context.Context) error {
	a, err := newWinddownAuthority()
	if err != nil {
		return err
	}
	if err := a.Gate(ctx); err != nil {
		return fmt.Errorf("fleet admission rejected: %w", err)
	}
	return nil
}

func winddownActor() string {
	if actor := strings.TrimSpace(os.Getenv("HERD_ACTOR")); actor != "" {
		return actor
	}
	if actor := strings.TrimSpace(os.Getenv("USER")); actor != "" {
		return actor
	}
	return "herd-cli"
}

func runWindDown() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: herd wind-down <on|off|status> [flags]")
		os.Exit(2)
	}
	action := os.Args[2]
	fs := flag.NewFlagSet("wind-down", flag.ExitOnError)
	actor := fs.String("actor", winddownActor(), "stable actor identifier")
	reason := fs.String("reason", "", "nonempty bounded reason for the posture change")
	generation := fs.Uint64("generation", 0, "monotonic generation (default: next durable generation)")
	deadline := fs.String("deadline", "", "optional RFC3339Nano deadline")
	fs.Parse(os.Args[3:])

	a, err := newWinddownAuthority()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wind-down: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	if action == "status" {
		state, readErr := a.Read(ctx)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "wind-down status: %v\n", readErr)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
			fmt.Fprintf(os.Stderr, "wind-down status: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if action != "on" && action != "off" {
		fmt.Fprintf(os.Stderr, "wind-down: unknown action %q\n", action)
		os.Exit(2)
	}
	if strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(os.Stderr, "wind-down: --reason is required for on/off")
		os.Exit(2)
	}

	var until *time.Time
	if strings.TrimSpace(*deadline) != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, *deadline)
		if parseErr != nil || parsed.UTC().Format(time.RFC3339Nano) != *deadline {
			fmt.Fprintf(os.Stderr, "wind-down: invalid --deadline %q\n", *deadline)
			os.Exit(2)
		}
		parsed = parsed.UTC()
		until = &parsed
	}

	current, readErr := a.Read(ctx)
	if readErr != nil && !errors.Is(readErr, winddown.ErrStateMissing) {
		fmt.Fprintf(os.Stderr, "wind-down: %v\n", readErr)
		os.Exit(1)
	}
	enabled := action == "on"
	gen := *generation
	if gen == 0 {
		if readErr == nil {
			candidate := winddown.State{Enabled: enabled, Actor: *actor, Reason: *reason, Generation: current.Generation, Deadline: until}
			if current.Enabled == candidate.Enabled && current.Actor == candidate.Actor && current.Reason == candidate.Reason && current.Generation == candidate.Generation && sameDeadline(current.Deadline, candidate.Deadline) {
				if err := json.NewEncoder(os.Stdout).Encode(current); err != nil {
					fmt.Fprintf(os.Stderr, "wind-down: %v\n", err)
					os.Exit(1)
				}
				return
			}
			gen = current.Generation + 1
		} else {
			gen = 1
		}
	}
	state, updateErr := a.Update(ctx, enabled, *actor, *reason, gen, until)
	if updateErr != nil {
		fmt.Fprintf(os.Stderr, "wind-down: %v\n", updateErr)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
		fmt.Fprintf(os.Stderr, "wind-down: %v\n", err)
		os.Exit(1)
	}
}

func sameDeadline(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
