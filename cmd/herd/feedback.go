package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/feedback"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// runFeedback ports bin/herd-feedback: the periodic fleet-wide control-plane
// feedback census. Reports outstanding replies from the current census, then
// opens a new one when the interval has elapsed. Flags are test overrides
// only; production configuration is env-driven (HERD_FEEDBACK_INTERVAL,
// HERD_FEEDBACK_GRACE, HERD_FEEDBACK_DIR, HERD_MAIL_DIR, HERD_WIND_DOWN_SENTINEL,
// HERD_SEND_BIN, HERD_WORKSPACE, HERD_WORKSPACE_LABEL) — see pkg/feedback.
func runFeedback() {
	fs := flag.NewFlagSet("feedback", flag.ExitOnError)
	selftest := fs.Bool("selftest", false, "Run the herd-feedback selftest")
	interval := fs.Int("interval", 0, "Seconds between censuses (0 = HERD_FEEDBACK_INTERVAL or 1800)")
	grace := fs.Int("grace", 0, "Seconds before missing replies are reported (0 = HERD_FEEDBACK_GRACE or 600)")
	mailDir := fs.String("mail-dir", "", "Override the durable mail directory (test only)")
	stateDir := fs.String("state-dir", "", "Override the durable census state directory (test only)")
	fs.Parse(os.Args[2:])

	if *selftest {
		if err := feedback.Selftest(); err != nil {
			fmt.Fprintf(os.Stderr, "herd-feedback selftest FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("herd-feedback selftest: PASS")
		return
	}

	opts := feedback.Options{
		Interval: *interval,
		Grace:    *grace,
		StateDir: *stateDir,
		MailDir:  *mailDir,
	}
	if cfg, cfgErr := config.LoadConfig(".herd/herd.yaml"); cfgErr == nil {
		opts.Roster = configuredFeedbackRoster(cfg, "")
	}
	if err := feedback.Run(context.Background(), opts); err != nil {
		// The workspace-unresolved path already printed its exact required
		// refusal text inside Run; every other error still needs a message
		// here or a failure exits silently with no diagnostic at all.
		if !errors.Is(err, feedback.ErrWorkspaceUnresolved) {
			fmt.Fprintf(os.Stderr, "herd-feedback: %v\n", err)
		}
		os.Exit(1)
	}
}

func configuredFeedbackRoster(cfg *config.Config, coordinator string) []string {
	seen := map[string]bool{}
	var roster []string
	add := func(name string) {
		if strings.TrimSpace(name) != "" && !seen[name] {
			seen[name] = true
			roster = append(roster, name)
		}
	}
	add(coordinator)
	if cfg != nil {
		for _, lane := range cfg.Lanes {
			if lane.Standing {
				add(lane.Name)
				add(standing.AgentNameForRepository(lane.Name, repositoryIdentityForLaunch(cfg)))
			}
		}
	}
	return roster
}
