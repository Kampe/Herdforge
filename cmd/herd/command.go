package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/cmdauth"
)

// FAC-195: `herd command` is the execution boundary for root/coordinator
// authorized commands. A worker lane does not run a guarded command directly;
// it runs it through `herd command run`, which consumes one durable attempt
// before creating any process and refuses once the budget is spent or a
// stop-on-first-failure command has returned nonzero.
//
// exitRefused is deliberately distinct from the authorized command's own exit
// code so a caller can tell "the command ran and failed" (its real exit code)
// from "the boundary refused to run it at all".
const exitRefused = 77

func commandDBPath(dbFlag string) string {
	if dbFlag != "" {
		return dbFlag
	}
	return cmdauth.DefaultPath(firstEnv("HERD_ROOT", "HERD_REPO_ROOT", "."))
}

func runCommand() {
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, subcommandUsage["command"])
		os.Exit(2)
	}
	switch args[0] {
	case "authorize":
		runCommandAuthorize(args[1:])
	case "run":
		runCommandRun(args[1:])
	case "receipts":
		runCommandReceipts(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "herd-command: unknown action %q\n%s\n", args[0], subcommandUsage["command"])
		os.Exit(2)
	}
}

// splitArgv separates flags from the authorized argv at the "--" delimiter.
// Everything after it is the exact command, taken literally.
func splitArgv(args []string) (flags, argv []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func parseDisposition(s string) (cmdauth.Disposition, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "stop-on-first-failure", "stop_on_first_failure":
		return cmdauth.StopOnFirstFailure, nil
	case "continue-on-failure", "continue_on_failure":
		return cmdauth.ContinueOnFailure, nil
	}
	return "", fmt.Errorf("disposition must be stop-on-first-failure or continue-on-failure, got %q", s)
}

func runCommandAuthorize(args []string) {
	fs := flag.NewFlagSet("command authorize", flag.ContinueOnError)
	id := fs.String("id", "", "Immutable command ID (must be distinct per authorization)")
	lane := fs.String("lane", "", "Lane the command is authorized for")
	session := fs.String("session", "", "Session ID the command is authorized for")
	authority := fs.String("authority", "", "Issuing authority (e.g. root, coordinator)")
	maxAttempts := fs.Int("max-attempts", 1, "Attempt budget")
	disposition := fs.String("disposition", "stop-on-first-failure", "stop-on-first-failure | continue-on-failure")
	dir := fs.String("dir", ".", "Working directory the command is authorized to run in")
	dbFlag := fs.String("db", "", "Ledger path (default $HERD_ROOT/.herd/command-authorizations.db)")
	fs.Usage = func() { fmt.Println(subcommandUsage["command"]) }

	flags, argv := splitArgv(args)
	if err := fs.Parse(flags); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "herd-command authorize: %v\n", err)
		}
		os.Exit(2)
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "herd-command authorize: the exact command must follow `--`")
		os.Exit(2)
	}
	disp, err := parseDisposition(*disposition)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-command authorize: %v\n", err)
		os.Exit(2)
	}

	store, err := cmdauth.Open(commandDBPath(*dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-command authorize: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	state, err := store.Authorize(context.Background(), cmdauth.Authorization{
		CommandID:   *id,
		CommandHash: cmdauth.CanonicalHash(*dir, argv),
		MaxAttempts: *maxAttempts,
		Authority:   *authority,
		Lane:        *lane,
		SessionID:   *session,
		Disposition: disp,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-command authorize: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("authorized %s: %d attempt(s), %s, lane=%s session=%s hash=%s\n",
		state.CommandID, state.MaxAttempts, state.Disposition, state.Lane, state.SessionID, state.CommandHash[:12])
}

func runCommandRun(args []string) {
	fs := flag.NewFlagSet("command run", flag.ContinueOnError)
	id := fs.String("id", "", "Command ID presented for execution")
	lane := fs.String("lane", "", "Presenting lane")
	session := fs.String("session", "", "Presenting session ID")
	dir := fs.String("dir", ".", "Working directory to run in")
	dbFlag := fs.String("db", "", "Ledger path (default $HERD_ROOT/.herd/command-authorizations.db)")
	fs.Usage = func() { fmt.Println(subcommandUsage["command"]) }

	flags, argv := splitArgv(args)
	if err := fs.Parse(flags); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "herd-command run: %v\n", err)
		}
		os.Exit(2)
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "herd-command run: the exact command must follow `--`")
		os.Exit(2)
	}

	store, err := cmdauth.Open(commandDBPath(*dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-command run: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	out, err := store.Run(context.Background(),
		cmdauth.Request{CommandID: *id, Lane: *lane, SessionID: *session},
		*dir, argv, cmdauth.OwnedSpawn(os.Stdout, os.Stderr))
	if err != nil && !out.Ran {
		// Refused before process creation: nothing was spawned.
		fmt.Fprintf(os.Stderr, "herd-command run: REFUSED: %v\n", err)
		os.Exit(exitRefused)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-command run: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "herd-command run: attempt %d of %d exited %d\n",
		out.Grant.Attempt, out.Grant.MaxAttempts, out.ExitCode)
	os.Exit(out.ExitCode)
}

func runCommandReceipts(args []string) {
	fs := flag.NewFlagSet("command receipts", flag.ContinueOnError)
	id := fs.String("id", "", "Restrict to one command ID (default: every receipt)")
	jsonFlag := fs.Bool("json", false, "Output JSON")
	dbFlag := fs.String("db", "", "Ledger path (default $HERD_ROOT/.herd/command-authorizations.db)")
	fs.Usage = func() { fmt.Println(subcommandUsage["command"]) }

	if err := fs.Parse(args); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "herd-command receipts: %v\n", err)
		}
		os.Exit(2)
	}
	store, err := cmdauth.Open(commandDBPath(*dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-command receipts: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	receipts, err := store.Receipts(context.Background(), *id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-command receipts: %v\n", err)
		os.Exit(1)
	}
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(receipts); err != nil {
			fmt.Fprintf(os.Stderr, "herd-command receipts: encode: %v\n", err)
			os.Exit(1)
		}
		return
	}
	for _, r := range receipts {
		exit := ""
		if r.ExitCode != nil {
			exit = fmt.Sprintf(" exit=%d", *r.ExitCode)
		}
		attempt := ""
		if r.Attempt > 0 {
			attempt = fmt.Sprintf(" attempt=%d", r.Attempt)
		}
		fmt.Printf("%s  %-10s %s lane=%s session=%s hash=%s%s%s  %s\n",
			r.At.UTC().Format("2006-01-02T15:04:05Z"), r.Event, r.CommandID,
			r.Lane, r.SessionID, short12(r.CommandHash), attempt, exit, r.Reason)
	}
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
