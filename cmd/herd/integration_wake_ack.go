package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Kampe/Herdforge/pkg/beat"
)

func runIntegrationWake(args []string) error {
	fs := flag.NewFlagSet("integration-wake", flag.ContinueOnError)
	emit := fs.Bool("emit", false, "enqueue one executable integration wake")
	sha := fs.String("candidate", "", "exact candidate SHA")
	generation := fs.Int64("generation", 0, "exact delivered wake generation")
	pr := fs.Int("pr", 0, "exact pull request number")
	task := fs.String("task", "", "task ref")
	owner := fs.String("owner", "", "exact owner identity")
	target := fs.String("target", "", "exact owner pane")
	session := fs.String("session", "", "exact owner session")
	action := fs.String("action", "", "executable next action naming the SHA and PR")
	nowFlag := fs.String("now", "", "RFC3339 clock for enqueue and escalation")
	maxAge := fs.Duration("max-age", integrationWakeAge, "unconsumed wake escalation age")
	asJSON := fs.Bool("json", false, "structured wake output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("integration-wake accepts no positional arguments")
	}
	if *emit {
		return emitIntegrationWake(*sha, *generation, *pr, *task, *owner, *target, *session, *action, *nowFlag, *maxAge, *asJSON)
	}
	return acknowledgeIntegrationWake(*sha, *generation)
}

func runIntegrationWakeAck(args []string) error {
	return runIntegrationWake(args)
}

func acknowledgeIntegrationWake(sha string, generation int64) error {
	if sha == "" || generation <= 0 {
		return fmt.Errorf("integration-wake requires --candidate and a positive --generation, with no positional arguments")
	}
	root, err := canonicalHerdRoot()
	if err != nil {
		return err
	}
	if err = beat.AcknowledgeIntegrationWake(integrationWakePath(root), sha, generation, time.Now().UTC()); err != nil {
		return err
	}
	fmt.Printf("integration wake %s generation %d acknowledged; no merge or board action performed\n", sha, generation)
	return nil
}

func emitIntegrationWake(sha string, generation int64, pr int, task, owner, target, session, action, nowFlag string, maxAge time.Duration, asJSON bool) error {
	if generation != 0 {
		return fmt.Errorf("integration-wake --emit records a prompt; omit --generation and acknowledge separately")
	}
	now := time.Now().UTC()
	if nowFlag != "" {
		parsed, err := time.Parse(time.RFC3339, nowFlag)
		if err != nil {
			return fmt.Errorf("integration-wake --now must be RFC3339: %w", err)
		}
		now = parsed.UTC()
	}
	intent := beat.IntegrationAction{
		CandidateSHA: sha,
		PullRequest:  pr,
		Task:         task,
		Owner:        owner,
		Target:       target,
		Session:      session,
		Action:       action,
	}
	root, err := canonicalHerdRoot()
	if err != nil {
		return err
	}
	wake, err := beat.EnqueueIntegrationWake(context.Background(), integrationWakePath(root), intent, now, maxAge, func(context.Context, beat.IntegrationWake) error {
		return nil
	})
	if err != nil {
		return err
	}
	report := struct {
		Wake   beat.IntegrationWake `json:"wake"`
		Merged bool                 `json:"merged"`
	}{Wake: wake, Merged: false}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Printf("integration wake %s generation %d owner=%s action=%s; no merge or board action performed\n", wake.CandidateSHA, wake.Generation, wake.Owner, wake.Action)
	return nil
}
