package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/feedback"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/posture"
)

// runFeedback ports bin/herd-feedback: the periodic fleet-wide control-plane
// feedback census. Reports outstanding replies from the current census, then
// opens a new one when the interval has elapsed.
func runFeedback() {
	fs := flag.NewFlagSet("feedback", flag.ExitOnError)
	intervalSec := fs.Int("interval", int(feedback.DefaultInterval.Seconds()), "Seconds between censuses")
	graceSec := fs.Int("grace", int(feedback.DefaultGrace.Seconds()), "Seconds before missing replies are reported")
	dryRun := fs.Bool("dry-run", false, "Report census state without sending anything")
	fs.Parse(os.Args[2:])

	interval := time.Duration(*intervalSec) * time.Second
	grace := time.Duration(*graceSec) * time.Second
	now := time.Now().UTC()

	stateDir := filepath.Join(".herd", "feedback")
	state, err := feedback.Load(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd feedback: %v\n", err)
		os.Exit(1)
	}

	// A census over an unresolved workspace would report a false empty fleet.
	workspace, err := herdr.RequireWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd feedback: workspace unresolved; refusing a false empty census: %v\n", err)
		os.Exit(1)
	}

	coordinator := strings.TrimSpace(os.Getenv("HERD_COORDINATOR"))
	if coordinator == "" {
		coordinator = "herdforge-orchestrator"
	}
	mailbox := mail.NewMailbox(filepath.Join(".herd", "control-mail.jsonl"))

	// Report the outstanding census before considering a new one.
	if state.Epoch != "" {
		replied := repliedLanes(mailbox, coordinator, state.Epoch)
		missing := feedback.Missing(state.Lanes, replied)
		fmt.Printf("herd feedback: epoch=%s replies=%d/%d missing=%s\n",
			state.Epoch, len(replied), len(state.Lanes), strings.Join(missing, ","))
		requestedAt := time.Unix(state.RequestedAtEpoch, 0).UTC()
		if feedback.Overdue(requestedAt, now, grace, len(missing)) {
			fmt.Fprintf(os.Stderr, "herd feedback: FEEDBACK_MISSING after %s: %s\n", grace, strings.Join(missing, ","))
		}
	}

	last := time.Time{}
	if state.RequestedAtEpoch > 0 {
		last = time.Unix(state.RequestedAtEpoch, 0).UTC()
	}
	if !feedback.Due(last, now, interval) {
		return
	}
	// Wind-down means the fleet is being brought to rest; do not open new work.
	if _, frozen := posture.BoardFrozen("."); frozen {
		fmt.Println("herd feedback: board frozen, not starting a new census")
		return
	}

	agents, err := herdr.AgentList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd feedback: agent list: %v\n", err)
		os.Exit(1)
	}

	epoch := feedback.Epoch(now)
	body := feedback.RequestBody(epoch, coordinator)
	subject := feedback.Subject(epoch)

	var lanes []string
	for _, a := range agents {
		if a.Workspace != workspace || strings.TrimSpace(a.Name) == "" || a.Name == coordinator {
			continue
		}
		if *dryRun {
			fmt.Printf("WOULD_REQUEST lane=%s status=%s\n", a.Name, a.Status)
			lanes = append(lanes, a.Name)
			continue
		}
		if _, err := mailbox.SendMessage(coordinator, a.Name, subject, body); err != nil {
			// Durable delivery is the authoritative half; if it fails the lane
			// is genuinely not in the census, so do not record it as requested.
			fmt.Fprintf(os.Stderr, "herd feedback: durable send to %s failed: %v\n", a.Name, err)
			continue
		}
		lanes = append(lanes, a.Name)
		if feedback.NeedsWake(a.Status) && a.PaneID != "" {
			nudge := fmt.Sprintf("Read and answer the %s request in your durable inbox before taking more work.", subject)
			if _, err := herdr.Send(a.PaneID, nudge, true, 30*time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "herd feedback: WARN could not prove wake delivery to %s (durable inbox copy exists): %v\n", a.Name, err)
			}
		}
	}

	if *dryRun {
		fmt.Printf("herd feedback: DRY_RUN epoch=%s lanes=%d\n", epoch, len(lanes))
		return
	}
	if err := feedback.Save(stateDir, &feedback.State{Epoch: epoch, RequestedAtEpoch: now.Unix(), Lanes: lanes}); err != nil {
		fmt.Fprintf(os.Stderr, "herd feedback: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd feedback: requested epoch=%s lanes=%d durable=yes\n", epoch, len(lanes))
}

// repliedLanes counts senders whose subject matches this census epoch.
func repliedLanes(mailbox *mail.Mailbox, coordinator, epoch string) []string {
	envelopes, err := mailbox.ReadInbox(coordinator)
	if err != nil {
		return nil
	}
	prefix := feedback.Subject(epoch)
	seen := map[string]bool{}
	var out []string
	for _, env := range envelopes {
		if !strings.HasPrefix(env.Subject, prefix) || seen[env.Sender] {
			continue
		}
		seen[env.Sender] = true
		out = append(out, env.Sender)
	}
	return out
}
