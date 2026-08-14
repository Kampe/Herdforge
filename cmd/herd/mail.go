package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/mail"
)

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
		runMailInbox(args[1:])
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
	mailPath := fs.String("mail", "", "mailbox path override")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if strings.TrimSpace(*sender) == "" || strings.TrimSpace(*recipient) == "" || strings.TrimSpace(*body) == "" {
		fmt.Fprintln(os.Stderr, "mail send: --from, --to, and --body are required")
		os.Exit(2)
	}
	path := controlMailPath(*mailPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mail send: %v\n", err)
		os.Exit(1)
	}
	env, err := mail.NewMailbox(path).SendMessage(strings.TrimSpace(*sender), strings.TrimSpace(*recipient), *subject, *body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail send: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(env); err != nil {
		fmt.Fprintf(os.Stderr, "mail send: encode response: %v\n", err)
		os.Exit(1)
	}
}

func runMailInbox(args []string) {
	fs := flag.NewFlagSet("mail inbox", flag.ContinueOnError)
	recipient := fs.String("recipient", "", "inbox recipient")
	mailPath := fs.String("mail", "", "mailbox path override")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if strings.TrimSpace(*recipient) == "" {
		fmt.Fprintln(os.Stderr, "mail inbox: --recipient is required")
		os.Exit(2)
	}
	envelopes, err := mail.NewMailbox(controlMailPath(*mailPath)).ReadInbox(strings.TrimSpace(*recipient))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail inbox: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(envelopes); err != nil {
		fmt.Fprintf(os.Stderr, "mail inbox: encode response: %v\n", err)
		os.Exit(1)
	}
}
