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
	clear := fs.Bool("clear", false, "remove the durable goal")
	lane := fs.String("lane", "", "standing lane identity")
	task := fs.String("task", "", "task identity")
	owner := fs.String("owner", "", "goal owner")
	generation := fs.Int64("generation", 0, "lease generation")
	max := fs.Int("max", 8, "maximum continuations")
	expires := fs.String("expires", "", "RFC3339 expiry")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	modeCount := 0
	for _, mode := range []bool{*set, *check, *clear} {
		if mode {
			modeCount++
		}
	}
	if modeCount != 1 {
		return errors.New("exactly one of --set, --check, or --clear is required")
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
	var evidence goalguard.Evidence
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64*1024)).Decode(&evidence); err != nil {
		return fmt.Errorf("decode evidence: %w", err)
	}
	decision, err := s.Evaluate(evidence)
	if err != nil {
		return err
	}
	return writeGoalJSON(os.Stdout, decision)
}

func writeGoalJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
