package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/mail"
)

// runHandoffs makes draining the durable handoff queue a SUPPORTED operation
// rather than a convention.
//
// FAC-569: handoffs were queued durably (FAC-567) and the live supervisor still
// applied pane supersession -- "The latest handoff supersedes the DeFi request"
// -- processing one and leaving both records unread. Part of the reason is that
// there was no command to see or acknowledge the queue: draining was a habit
// nobody had, expressed nowhere in the tooling.
//
// Acknowledgement is deliberately separate from reading. An entry is marked done
// only when its handoff reaches a disposition, so an unread entry always means
// unfinished work. Marking one done to clear a list would recreate the loss this
// exists to prevent.
func runHandoffs(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: herd handoffs <list|done> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		runHandoffsList(args[1:])
	case "done":
		runHandoffsDone(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "herd handoffs: unknown subcommand %q (want list or done)\n", args[0])
		os.Exit(2)
	}
}

func handoffRecipient(args []string) (string, bool, []string) {
	recipient := ""
	asJSON := false
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--recipient":
			if i+1 < len(args) {
				recipient = args[i+1]
				i++
			}
		case "--json":
			asJSON = true
		default:
			rest = append(rest, args[i])
		}
	}
	return recipient, asJSON, rest
}

func runHandoffsList(args []string) {
	recipient, asJSON, _ := handoffRecipient(args)
	if strings.TrimSpace(recipient) == "" {
		// Fail closed: listing "everyone's" queue would invite acting on
		// another lane's work.
		fmt.Fprintln(os.Stderr, "herd handoffs list: --recipient <agent> is required")
		os.Exit(2)
	}
	pending, err := PendingReviewHandoffs(mail.CallbackMailPath("."), recipient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd handoffs list: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(pending); err != nil {
			fmt.Fprintf(os.Stderr, "herd handoffs list: encode: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(pending) == 0 {
		fmt.Printf("herd handoffs: no pending handoffs for %s\n", recipient)
		return
	}
	fmt.Printf("herd handoffs: %d pending for %s — each is independent work; none supersedes another\n",
		len(pending), recipient)
	for _, env := range pending {
		fmt.Printf("  %s  seq=%d  %s\n", env.ID, env.Sequence, env.Subject)
	}
	fmt.Println("Mark one done only after it reaches a disposition: herd handoffs done <id>")
}

func runHandoffsDone(args []string) {
	recipient, _, rest := handoffRecipient(args)
	if strings.TrimSpace(recipient) == "" || len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: herd handoffs done <id> --recipient <agent>")
		os.Exit(2)
	}
	id := rest[0]
	box := mail.NewMailbox(mail.CallbackMailPath("."))
	if err := box.MarkHandled(recipient, id); err != nil {
		fmt.Fprintf(os.Stderr, "herd handoffs done: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd handoffs: %s acknowledged for %s\n", id, recipient)
}
