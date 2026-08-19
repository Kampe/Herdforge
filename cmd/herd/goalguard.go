package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/goalguard"
)

func runGoalGuard() error {
	fs := flag.NewFlagSet("goal-guard", flag.ContinueOnError)
	state := fs.String("state", goalguard.DefaultPath(), "durable goal state path")
	set := fs.Bool("set", false, "create or replace a standing goal")
	check := fs.Bool("check", false, "evaluate evidence JSON from stdin")
	stopHook := fs.Bool("stop-hook", false, "Claude Stop hook mode: silent when no goal, block stop while goal is active")
	clear := fs.Bool("clear", false, "remove the durable goal")
	lane := fs.String("lane", "", "standing lane identity")
	task := fs.String("task", "", "task identity")
	owner := fs.String("owner", "", "goal owner")
	generation := fs.Int64("generation", 0, "lease generation")
	max := fs.Int("max", 0, "maximum continuations (0 = unbounded: run until the goal is met)")
	expires := fs.String("expires", "", "RFC3339 expiry")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	modeCount := 0
	for _, mode := range []bool{*set, *check, *clear, *stopHook} {
		if mode {
			modeCount++
		}
	}
	if modeCount != 1 {
		return errors.New("exactly one of --set, --check, --stop-hook, or --clear is required")
	}
	s, err := goalguard.Open(*state)
	if err != nil {
		return err
	}
	if *clear {
		if err := os.Remove(*state); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear: %w", err)
		}
		fmt.Fprintln(os.Stdout, `{"cleared":true}`)
		return nil
	}
	if *set {
		now := time.Now().UTC()
		g := goalguard.Goal{SchemaVersion: goalguard.SchemaVersion, Lane: *lane, Task: *task, Owner: *owner, Generation: *generation, MaxContinuations: *max, CreatedAt: now, UpdatedAt: now}
		if strings.TrimSpace(*expires) != "" {
			expiry, parseErr := time.Parse(time.RFC3339Nano, *expires)
			if parseErr != nil {
				return fmt.Errorf("parse expiry: %w", parseErr)
			}
			g.ExpiresAt = &expiry
		}
		if err := s.Set(g); err != nil {
			return err
		}
		return writeGoalJSON(os.Stdout, g)
	}
	if *stopHook {
		return runGoalGuardStopHook(s, nil)
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var evidence goalguard.Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return fmt.Errorf("decode evidence: %w", err)
	}
	if strings.TrimSpace(evidence.Lane) == "" {
		// stdin is not Evidence JSON — it is a Claude Stop hook payload.
		// --check wired as a Stop hook must behave like --stop-hook, not
		// spam "incomplete evidence" on every session end.
		return runGoalGuardStopHook(s, raw)
	}
	decision, err := s.Evaluate(evidence)
	if errors.Is(err, goalguard.ErrMissing) {
		// No durable goal means there is nothing to guard. Stop hooks run
		// --check on every session end; absence is a quiet no-decision, not
		// an error to spam.
		return writeGoalJSON(os.Stdout, goalguard.Decision{Reason: "no_goal"})
	}
	if err != nil {
		return err
	}
	return writeGoalJSON(os.Stdout, decision)
}

// runGoalGuardStopHook adapts the guard to Claude Code's Stop hook contract:
// no durable goal means nothing to guard (silent exit 0, stop allowed), and an
// active goal blocks the stop via {"decision":"block"} so the agent keeps
// working until the goal is met. When the payload carries stop_hook_active the
// previous block in this turn was already delivered, so return success —
// re-blocking loops until the harness force-overrides. The nudge repeats on
// the agent's next natural stop instead.
func runGoalGuardStopHook(s *goalguard.Store, payload []byte) error {
	if payload == nil {
		payload, _ = io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	}
	var hook struct {
		StopHookActive bool `json:"stop_hook_active"`
	}
	_ = json.Unmarshal(payload, &hook)
	if hook.StopHookActive {
		return nil
	}
	g, err := s.Load()
	if errors.Is(err, goalguard.ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	evidence := goalguard.Evidence{Lane: g.Lane, Task: g.Task, Owner: g.Owner, Generation: g.Generation, LeaseHeld: true, Now: time.Now().UTC()}
	decision, err := s.Evaluate(evidence)
	if err != nil {
		return err
	}
	if !decision.Continue {
		return writeGoalJSON(os.Stdout, decision)
	}
	block := map[string]string{
		"decision": "block",
		"reason":   fmt.Sprintf("goal-guard: goal %q on lane %q is not met (continuation %d). Keep working toward the goal; stop only when it is complete, then run `herd goal-guard --clear`.", g.Task, g.Lane, decision.Continuations),
	}
	return writeGoalJSON(os.Stdout, block)
}

func writeGoalJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
