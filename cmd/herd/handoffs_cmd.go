package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		// FAC-570: resolve the caller's own identity rather than demanding a
		// value an agent cannot know. The packet used to say
		// "--recipient <your-agent-name>", which is unresolvable from inside a
		// pane: a live supervisor substituted its role id, saw an empty inbox,
		// and concluded there was no work while two records were pending.
		self, err := resolveSelfAgentName()
		if err != nil {
			// Loud, never an empty list. Unresolved identity and an empty queue
			// must not look the same.
			fmt.Fprintf(os.Stderr, "herd handoffs list: %v\n", err)
			os.Exit(1)
		}
		recipient = self
		fmt.Printf("herd handoffs: resolved your agent name as %s\n", recipient)
	}
	// FAC-572: disclose WHICH mailbox is being read.
	//
	// A coordinator listing --recipient X saw two entries pending while the
	// target's own self-resolved list reported none. That question is
	// unanswerable from the output, because CallbackMailPath is CWD-relative:
	// a coordinator at the repo root and an agent in its worktree can resolve
	// different files, and nothing printed said so. A queue tool that does not
	// name its queue makes "pending" and "handled" unattributable.
	// Resolve ONCE: the resolver also emits the divergent-mailbox note, and
	// calling it per use printed that note twice for one command.
	bus := canonicalHandoffMailbox()
	mailPath, pathErr := filepath.Abs(bus)
	if pathErr != nil {
		mailPath = bus
	}
	pending, err := PendingReviewHandoffs(bus, recipient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd handoffs list: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		// The mailbox path is part of the answer, not decoration: two callers
		// disagreeing about "pending" is usually two different files.
		payload := map[string]any{
			"recipient":     recipient,
			"mailbox":       mailPath,
			"handled_state": mail.HandledStatePath(mailPath),
			"pending":       pending,
		}
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "herd handoffs list: encode: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(pending) == 0 {
		fmt.Printf("herd handoffs: no pending handoffs for %s\n  mailbox: %s\n  handled: %s\n",
			recipient, mailPath, mail.HandledStatePath(mailPath))
		return
	}
	fmt.Printf("herd handoffs: %d pending for %s — each is independent work; none supersedes another\n  mailbox: %s\n  handled: %s\n",
		len(pending), recipient, mailPath, mail.HandledStatePath(mailPath))
	for _, env := range pending {
		fmt.Printf("  %s  seq=%d  %s\n", env.ID, env.Sequence, env.Subject)
	}
	fmt.Println("Mark one done only after it reaches a disposition: herd handoffs done <id>")
}

func runHandoffsDone(args []string) {
	recipient, _, rest := handoffRecipient(args)
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: herd handoffs done <id> [--recipient <agent>]")
		os.Exit(2)
	}
	if strings.TrimSpace(recipient) == "" {
		self, err := resolveSelfAgentName()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd handoffs done: %v\n", err)
			os.Exit(1)
		}
		recipient = self
	}
	id := rest[0]
	box := mail.NewMailbox(canonicalHandoffMailbox())
	if err := box.MarkHandled(recipient, id); err != nil {
		fmt.Fprintf(os.Stderr, "herd handoffs done: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd handoffs: %s acknowledged for %s\n", id, recipient)
}
