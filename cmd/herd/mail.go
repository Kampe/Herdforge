package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/feedback"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mail"
)

var listMailAgents = herdr.AgentList

// runMail exposes ordinary durable mailbox operations. Privileged control
// envelopes remain owned by runControlArgs so their validation cannot drift.
func runMail() {
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usageFor("mail"))
		os.Exit(2)
	}
	switch args[0] {
	case "send":
		runMailSend(args[1:])
	case "inbox", "read":
		runMailInbox(args[0], args[1:])
	case "control":
		runControlArgs(args[1:])
	case "help", "--help", "-h":
		fmt.Println(usageFor("mail"))
	default:
		fmt.Fprintf(os.Stderr, "mail: unknown mode %q\n%s\n", args[0], usageFor("mail"))
		os.Exit(2)
	}
}

func runMailSend(args []string) {
	fs := flag.NewFlagSet("mail send", flag.ContinueOnError)
	sender := fs.String("from", "", "message sender")
	recipient := fs.String("to", "", "message recipient")
	subject := fs.String("subject", "", "optional message subject")
	body := fs.String("body", "", "message body")
	bodyFile := fs.String("file", "", "read message body bytes from a file; use - for stdin")
	mailPath := fs.String("mail", "", "mailbox path override")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if strings.TrimSpace(*sender) == "" || strings.TrimSpace(*recipient) == "" {
		fmt.Fprintln(os.Stderr, "mail send: --from and --to are required")
		os.Exit(2)
	}
	if *body != "" && *bodyFile != "" {
		fmt.Fprintln(os.Stderr, "mail send: use only one of --body, --file, or stdin")
		os.Exit(2)
	}
	payload := []byte(*body)
	if *bodyFile != "" {
		var err error
		if *bodyFile == "-" {
			payload, err = io.ReadAll(os.Stdin)
		} else {
			payload, err = os.ReadFile(*bodyFile)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "mail send: read body: %v\n", err)
			os.Exit(1)
		}
	} else if *body == "" {
		var err error
		payload, err = io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mail send: read stdin: %v\n", err)
			os.Exit(1)
		}
	}
	if len(payload) == 0 {
		fmt.Fprintln(os.Stderr, "mail send: a non-empty body is required via --body, --file, or stdin")
		os.Exit(2)
	}
	warnUnknownMailParticipants(strings.TrimSpace(*sender), strings.TrimSpace(*recipient))
	path, err := controlMailPath(*mailPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail send: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mail send: %v\n", err)
		os.Exit(1)
	}
	env, err := mail.NewMailbox(path).SendMessage(strings.TrimSpace(*sender), strings.TrimSpace(*recipient), *subject, string(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail send: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(env); err != nil {
		fmt.Fprintf(os.Stderr, "mail send: encode response: %v\n", err)
		os.Exit(1)
	}
}

// warnUnknownMailParticipants keeps ordinary mail permissive for future lanes,
// while giving senders an immediate signal when a typo or stale rename would
// otherwise create an orphaned mailbox. AgentList includes current and
// recently-live Herdr rows, which is the namespace used by fleet dispatch.
func warnUnknownMailParticipants(sender, recipient string) {
	agents, err := listMailAgents()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail send: recipient liveness unavailable: %v\n", err)
		return
	}
	known := make(map[string]struct{}, len(agents))
	var recipientAgent *herdr.AgentEntry
	for _, agent := range agents {
		if name := strings.TrimSpace(agent.Name); name != "" {
			known[name] = struct{}{}
			if name == recipient || name == resolveSendTarget(recipient) {
				candidate := agent
				recipientAgent = &candidate
			}
		}
	}
	// Also treat standing roles that resolve to a known live name as known
	for _, role := range []string{sender, recipient} {
		if live := resolveSendTarget(role); live != role {
			if _, ok := known[live]; ok {
				known[role] = struct{}{}
			}
		}
	}
	if recipientAgent != nil && recipientAgent.PaneID != "" && recipientAgent.Status != "done" {
		fmt.Fprintf(os.Stderr, "mail send: hint: recipient %q has a live pane; durable-only mail is not surfaced there; use herd send for pane delivery\n", recipient)
	}
	for _, participant := range []struct {
		role string
		name string
	}{{"sender", sender}, {"recipient", recipient}} {
		if _, ok := known[participant.name]; !ok {
			fmt.Fprintf(os.Stderr, "mail send: warning: %s %q is not a known live or recently-live Herdr agent; message will still be filed\n", participant.role, participant.name)
		}
	}
}

// readFeedbackMailbox returns envelopes from the feedback producer's per-recipient
// mailbox. An absent file is not an error: most lanes are never polled.
func readFeedbackMailbox(recipient string) ([]*mail.Envelope, error) {
	if recipient == "" {
		return nil, nil
	}
	// One resolver, shared with the writer, so the two cannot drift apart.
	path := filepath.Join(feedback.FleetMailDir(firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")), recipient+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*mail.Envelope
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// The feedback producer uses a DIFFERENT SCHEMA from the control bus, and
		// that is the real split -- not just two paths. Feedback writes
		// id(int)/from/to/summary/message/read_at; mail.Envelope expects
		// id(string)/sender/recipient/subject/body/read. A direct unmarshal fails
		// on the id type alone, which is why the first cut of this fix returned
		// zero envelopes from a file that plainly had two.
		var fb struct {
			ID      int64   `json:"id"`
			From    string  `json:"from"`
			To      string  `json:"to"`
			When    string  `json:"timestamp"`
			Summary string  `json:"summary"`
			Message string  `json:"message"`
			ReadAt  *string `json:"read_at"`
		}
		if err := json.Unmarshal([]byte(line), &fb); err != nil {
			// Reported, never silently dropped: a feedback request nobody can
			// parse is still a request that was made.
			fmt.Fprintf(os.Stderr, "mail: unparseable feedback envelope in %s: %v\n", path, err)
			continue
		}
		ts, _ := time.Parse(time.RFC3339, fb.When)
		out = append(out, &mail.Envelope{
			ID:        fmt.Sprintf("feedback-%d", fb.ID),
			Sequence:  fb.ID,
			Sender:    fb.From,
			Recipient: fb.To,
			Subject:   fb.Summary,
			Body:      fb.Message,
			Read:      fb.ReadAt != nil && strings.TrimSpace(*fb.ReadAt) != "",
			Timestamp: ts,
		})
	}
	return out, nil
}

func runMailInbox(mode string, args []string) {
	fs := flag.NewFlagSet("mail "+mode, flag.ContinueOnError)
	recipient := fs.String("recipient", "", "inbox recipient")
	mailPath := fs.String("mail", "", "mailbox path override")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if strings.TrimSpace(*recipient) == "" {
		fmt.Fprintf(os.Stderr, "mail %s: --recipient is required\n", mode)
		os.Exit(2)
	}
	path, err := controlMailPath(*mailPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail %s: %v\n", mode, err)
		os.Exit(1)
	}
	result, err := mail.NewMailbox(path).ReadInboxStatus(strings.TrimSpace(*recipient))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail %s: %v\n", mode, err)
		os.Exit(1)
	}
	envelopes := result.Envelopes
	if envelopes == nil {
		envelopes = make([]*mail.Envelope, 0)
	}
	// FAC-629: also drain the FEEDBACK mailbox.
	//
	// pkg/feedback writes each lane's FLEET_FEEDBACK request to
	// <state>/mail/<recipient>.jsonl, and nothing could read it back. `herd mail
	// inbox` only ever read .herd/control-mail.jsonl, so a lane asked for feedback
	// was told "recipient has no mailbox history" while its requests sat unread on
	// disk. The herd-smith lane -- whose ENTIRE JOB is aggregating fleet feedback
	// and reporting it -- had two unread requests and no way to reach them.
	//
	// A feedback agent that cannot read its feedback is not a partial failure, it
	// is the whole function missing. Both mailboxes are now drained by one
	// command, because a recipient should not have to know which producer wrote
	// to it.
	feedbackEnvelopes, feedbackErr := readFeedbackMailbox(strings.TrimSpace(*recipient))
	if feedbackErr != nil {
		// Report and continue: a missing feedback mailbox is normal for lanes that
		// were never polled, and must not fail a control-mail read.
		fmt.Fprintf(os.Stderr, "mail %s: feedback mailbox: %v\n", mode, feedbackErr)
	}
	envelopes = append(envelopes, feedbackEnvelopes...)

	if !result.RecipientSeen && len(feedbackEnvelopes) == 0 {
		fmt.Fprintf(os.Stderr, "mail %s: recipient %q has no mailbox history\n", mode, strings.TrimSpace(*recipient))
	}
	if err := json.NewEncoder(os.Stdout).Encode(envelopes); err != nil {
		fmt.Fprintf(os.Stderr, "mail %s: encode response: %v\n", mode, err)
		os.Exit(1)
	}
}
