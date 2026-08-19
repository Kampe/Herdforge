package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
			if name == recipient {
				candidate := agent
				recipientAgent = &candidate
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
	if !result.RecipientSeen {
		fmt.Fprintf(os.Stderr, "mail %s: recipient %q has no mailbox history\n", mode, strings.TrimSpace(*recipient))
	}
	envelopes := result.Envelopes
	if envelopes == nil {
		envelopes = make([]*mail.Envelope, 0)
	}
	if err := json.NewEncoder(os.Stdout).Encode(envelopes); err != nil {
		fmt.Fprintf(os.Stderr, "mail %s: encode response: %v\n", mode, err)
		os.Exit(1)
	}
}
